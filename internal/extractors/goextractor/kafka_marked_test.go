package goextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func roleOf(f facts.Fact) string { return f.PropString(facts.PropMessagingRole) }

func TestKafka_SubscriptionConfigInAVariable(t *testing.T) {
	// No Kafka client import: the subscription goes through the service's own
	// messaging package. The TopicName field marks the topic; the registration
	// call's name says it is consumed. Retry and dead-letter topics are not it.
	got := kafkaTopics(t, `package main

import "example.com/shop/internal/pkg/messaging"

func setup(m *Manager) {
	placed := messaging.TopicConfig{
		TopicName:           "billing.orders.order_placed",
		RetryTopicName:      "shop.retry.billing.orders.order_placed",
		DeadLetterTopicName: "shop.dlq.billing.orders.order_placed",
	}
	if err := consumers.RegisterBatchSubscriptionWithDLQ(m, prefix, placed, "order_placed", h.Handle); err != nil {
		panic(err)
	}
}
`)
	f, ok := got["billing.orders.order_placed"]
	if !ok || roleOf(f) != facts.MessagingRoleConsumer {
		t.Fatalf("want the subscribed topic as consumed, got %+v", got)
	}
	for name := range got {
		if name != "billing.orders.order_placed" {
			t.Errorf("unexpected topic %q", name)
		}
	}
}

func TestKafka_PublishedMetadataInAVariable(t *testing.T) {
	got := kafkaTopics(t, `package events

const placedTopic = "shop.orders.order_placed"

func publish(ctx context.Context, p Publisher, ev Event) error {
	meta := async.EventMetadata{
		MessageMetadata: async.MessageMetadata{Origin: "shop", Topic: placedTopic},
	}
	return p.PublishEvent(ctx, ev, meta)
}
`)
	f, ok := got["shop.orders.order_placed"]
	if !ok || roleOf(f) != facts.MessagingRoleProducer {
		t.Fatalf("want the published topic as produced, got %+v", got)
	}
}

func TestKafka_HelperMethodBuildsTheMetadata(t *testing.T) {
	got := kafkaCtorTopics(t, map[string]string{
		"internal/consumers/handler.go": `package consumers

func (p *Publisher) metadataFor(topic string) async.EventMetadata {
	return async.EventMetadata{MessageMetadata: async.MessageMetadata{Origin: "shop", Topic: topic}}
}
`,
		"internal/consumers/wiring.go": `package consumers

const cacheClearedTopic = "shop.users.cache_cleared"

func (p *Publisher) CacheCleared(ctx context.Context, id int) error {
	meta := p.metadataFor(cacheClearedTopic)
	return p.Broker().PublishEvent(ctx, &CacheCleared{ID: id}, meta)
}
`,
	})
	f, ok := got["shop.users.cache_cleared"]
	if !ok || roleOf(f) != facts.MessagingRoleProducer {
		t.Fatalf("want the topic the helper's metadata carries as produced, got %+v", got)
	}
	if f.File != "internal/consumers/wiring.go" || f.PropString("call") != "PublishEvent" {
		t.Errorf("fact = %+v, want the publish call site", f)
	}
}

func TestKafka_MarkedTopicNeedsAMessagingCallAndANamespacedName(t *testing.T) {
	got := kafkaTopics(t, `package app

func wire(bus *EventBus, store *Store) {
	bus.Publish(Event{Topic: "refresh"})
	store.Save(Config{TopicName: "billing.orders.order_placed"})
}
`)
	if len(got) != 0 {
		t.Fatalf("an in-process event and a stored config are not Kafka operations: %+v", got)
	}
}

func TestKafka_SubscriptionRegisteredWithItsTopicWrittenOut(t *testing.T) {
	got := kafkaTopics(t, `package main

func setup(m *Manager) {
	consumers.RegisterSubscription(m, prefix, "catalog.categories.category_changed", "category_changed", h.Handle)
	consumers.RegisterBatchSubscription(m, prefix, []string{"catalog.items.item_listed.replay"}, "replay", h.Handle)
	bus.Subscribe("catalog.items.item_listed", h.Handle)
}
`)
	for _, want := range []string{"catalog.categories.category_changed", "catalog.items.item_listed.replay"} {
		if f, ok := got[want]; !ok || roleOf(f) != facts.MessagingRoleConsumer {
			t.Errorf("%s: want consumed, got %+v", want, got)
		}
	}
	if _, ok := got["catalog.items.item_listed"]; ok {
		t.Error("a bus Subscribe without a Kafka client is not read from its literal")
	}
}
