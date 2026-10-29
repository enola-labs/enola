package rubyextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const rubyPublisherSrc = `# frozen_string_literal: true

module Orders
  class Events::OrderPlaced
    SCHEMA_SUBJECT = 'contracts.backend.shop.orders.order_placed'
    TOPIC = SCHEMA_SUBJECT
    AUDIT_TOPIC = "shop.#{PORTAL}.orders.audited".freeze

    def run
      Broker.outbox.publish_event(payload, topic: TOPIC, key: key)
    end

    def self.backfill(rows)
      Broker.client.publish_events(rows, topic: "shop.#{PORTAL}.orders.backfill")
    end

    def log
      Logger.publish("not a topic")
      Broker.publish_event(payload, topic: topic_for(region))
      Mailer.notify(topic: "billing")
    end

    def audit
      Outbox::CreateMessage.run(outbox: AuditOutbox, topic: AUDIT_TOPIC, payload: {})
    end

    def build_message(row)
      { topic: TOPIC, timestamp: Time.now, payload: row }
    end

    def dead_letters
      { topic: "shop.orders.dlq", enabled: true }
    end
  end
end
`

const rubySubscriberSrc = `# frozen_string_literal: true

Rails.application.config.after_initialize do
  Broker.configure_subscriptions do |config|
    config.subscribe_event 'shop.billing.order_placed' do |s|
      s.topic = 'contracts.backend.shop.orders.order_placed'
      s.handler = Billing::OrderPlacedConsumer
      s.retry_policy = { max_retries: 5 }
    end

    config.subscribe_event 'shop.billing.user_events' do |s|
      s.topic = "shop.#{PORTAL}.users.user_events"
      s.handlers = {
        Billing::UserRegistered::SCHEMA_SUBJECT => Billing::UserRegistered,
      }
    end

    config.subscribe_event 'shop.billing.no_handler' do |s|
      s.topic = 'shop.billing.ignored'
    end
  end
end
`

func messagingOps(ff []facts.Fact) map[string]bool {
	out := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindStorage && f.PropString(facts.PropSource) == facts.MessagingSourceRubyCall {
			out[f.PropString(facts.PropMessagingOperation)+" "+f.Name+" "+f.PropString("code_symbol")] = true
		}
	}
	return out
}

func rubyMessaging(src, file string) []facts.Fact {
	ff, _, _ := extractRubyMessagingFacts([]byte(src), file)
	return ff
}

func TestRubyMessaging(t *testing.T) {
	got := messagingOps(rubyMessaging(rubyPublisherSrc, "app/events/order_placed.rb"))
	for op := range messagingOps(rubyMessaging(rubySubscriberSrc, "config/initializers/broker.rb")) {
		got[op] = true
	}
	want := []string{
		"publish contracts.backend.shop.orders.order_placed Orders::Events::OrderPlaced#run",
		"publish contracts.backend.shop.orders.order_placed Orders::Events::OrderPlaced#build_message",
		"publish shop.{portal}.orders.backfill Orders::Events::OrderPlaced.backfill",
		"publish shop.{portal}.orders.audited Orders::Events::OrderPlaced#audit",
		"subscribe contracts.backend.shop.orders.order_placed Billing::OrderPlacedConsumer",
		"subscribe shop.{portal}.users.user_events Billing::UserRegistered",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %q", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRubyMessaging_NoTopicNoFacts(t *testing.T) {
	src := "class A\n  def x\n    Notifier.publish(event, channel: 'x')\n  end\nend\n"
	if ff := rubyMessaging(src, "a.rb"); len(ff) != 0 {
		t.Errorf("got %d facts from a file without topics", len(ff))
	}
}

// A topic constant another file defines, reached through an included module, is
// resolved once every file is read; a bare name two constants share is not.
func TestRubyMessaging_ConstantInAnotherFile(t *testing.T) {
	files := map[string]string{
		"app/outbox/constants.rb": "module Outbox::Constants\n  THREAD_EVENTS_TOPIC = 'shop.threads.thread_events'\n  SHARED_TOPIC = 'shop.a.events'\nend\n",
		"app/other/constants.rb":  "module Other\n  SHARED_TOPIC = 'shop.b.events'\nend\n",
		"app/outbox/thread_deleted.rb": `class Outbox::ThreadDeleted
  include Outbox::Constants

  def run
    Outbox::CreateMessage.run(topic: THREAD_EVENTS_TOPIC, payload: {})
    Outbox::CreateMessage.run(topic: SHARED_TOPIC, payload: {})
  end
end
`,
	}
	var consts []rubyTopicConst
	var pending []rubyPendingTopic
	for _, name := range []string{"app/other/constants.rb", "app/outbox/constants.rb", "app/outbox/thread_deleted.rb"} {
		_, c, p := extractRubyMessagingFacts([]byte(files[name]), name)
		consts, pending = append(consts, c...), append(pending, p...)
	}
	got := messagingOps(resolveRubyPendingTopics(pending, consts))
	want := "publish shop.threads.thread_events Outbox::ThreadDeleted#run"
	if !got[want] || len(got) != 1 {
		t.Errorf("got %v, want only %q", got, want)
	}
}
