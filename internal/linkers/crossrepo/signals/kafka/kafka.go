// Package kafka links repos coupled by Kafka topics.
package kafka

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Signal binds ASYNCHRONOUS coupling: a repo that references a Kafka topic owned by
// another loaded repo consumes that repo's events, so it depends on it. The edge is
// drawn consumer -> producer, mirroring HTTP's client -> server.
//
// It is directional despite the transport being fire-and-forget: topic ownership is
// what orients it, and ownership is asymmetric.
type Signal struct {
	v *vocab.Set
}

// New returns the signal, reading topic ownership under the given vocabulary.
func New(v *vocab.Set) *Signal { return &Signal{v: v} }

func (s *Signal) Name() string { return "kafka" }

func (s *Signal) Phase() plugin.SignalPhase { return plugin.PhaseDirectional }

// topicOwner returns the owning-service segment of a Kafka topic name: the part
// before the first ".". By convention a topic is named "<owning-service>.<...>"
// (e.g. "svc-orders.order_placed" -> "svc-orders",
// "shop.orders.order_placed" -> "shop"), so the leading segment identifies the
// producer even when that repo's producer code cannot be parsed.
func (s *Signal) topicOwner(topic string) string {
	topic = strings.TrimSpace(topic)
	if i := strings.Index(topic, s.v.TopicOwnerSeparator); i >= 0 {
		return topic[:i]
	}
	return topic
}

// linkKafka binds asynchronous coupling: a repo that references a Kafka topic owned
// by ANOTHER loaded repo consumes that repo's events, so it depends on it. The edge
// is drawn consumer -> producer (mirroring HTTP client -> server), keyed on the
// topic name's owning-service prefix — the same leading-segment-to-repo resolution
// linkImports uses, and robust to the producer side being unparsed. A repo's
// reference to its OWN topic (owner == repo) is intra-repo and draws no edge, and a
// topic owned by no loaded repo (an export sink, a third-party service) is simply
// left unlinked.
func (s *Signal) Contribute(in plugin.SignalInput, out plugin.EvidenceSink) {
	producers := producerRepos(in.Facts())
	for _, f := range in.Facts() {
		if f.Repo == "" || f.Kind != facts.KindStorage {
			continue
		}
		if f.PropString("storage_kind") != facts.StorageKindTopic {
			continue
		}
		// Historically every topic fact came from Kafka-aware code extraction.
		// AsyncAPI introduces other protocols into the same fact kind; an explicit
		// non-Kafka protocol must not manufacture a via=kafka dependency.
		if protocol := f.PropString(facts.PropMessaging); protocol != "" && !facts.IsKafkaProtocol(protocol) {
			continue
		}
		// A contract can state direction explicitly. Publishing to a topic is an
		// outbound interface, not evidence that this repo consumes the topic owner.
		// Older extractors emit no role and retain name-based inference.
		if f.PropString(facts.PropMessagingRole) == facts.MessagingRoleProducer {
			continue
		}
		label, ok := declaredProducer(producers, f.Name, f.Repo)
		if !ok {
			owner := s.topicOwner(f.Name)
			if owner == "" {
				continue
			}
			label, ok = in.ResolveRepo(owner)
		}
		if !ok || label == f.Repo {
			continue // owner not loaded, or the repo's own topic (it is the producer)
		}
		e := out.Edge(f.Repo, label)
		e.Via(facts.ViaKafka)
		e.Sample(plugin.BucketTopics, f.Name)
	}
}

// producerRepos indexes the topics a loaded repo declares it publishes (an
// AsyncAPI producer channel, a publish call site) by topic name.
func producerRepos(all []facts.Fact) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, f := range all {
		if f.Repo == "" || f.Kind != facts.KindStorage || f.PropString("storage_kind") != facts.StorageKindTopic {
			continue
		}
		if f.PropString(facts.PropMessagingRole) != facts.MessagingRoleProducer {
			continue
		}
		if protocol := f.PropString(facts.PropMessaging); protocol != "" && !facts.IsKafkaProtocol(protocol) {
			continue
		}
		name := strings.TrimSpace(f.Name)
		if out[name] == nil {
			out[name] = map[string]bool{}
		}
		out[name][f.Repo] = true
	}
	return out
}

// declaredProducer resolves a consumed topic to the one other repo that declares
// producing it. Deployments often namespace a topic for the broker
// ("<env>.<team>.<topic>") while the producer's contract names it bare, so the
// consumed name matches when it equals a producer topic or ends with "." plus one,
// on a segment boundary, and the matched suffix keeps at least two segments. The consumer's own repo is not a candidate, and more than
// one candidate repo resolves nothing: the leading-segment rule decides then.
func declaredProducer(producers map[string]map[string]bool, topic, consumer string) (string, bool) {
	topic = strings.TrimSpace(topic)
	if topic == "" || len(producers) == 0 {
		return "", false
	}
	candidates := map[string]bool{}
	collect := func(name string) {
		for repo := range producers[name] {
			if repo != consumer {
				candidates[repo] = true
			}
		}
	}
	collect(topic)
	// A one-segment suffix ("order_placed") is too generic to name a producer.
	for i := strings.Index(topic, "."); i >= 0; {
		if suffix := topic[i+1:]; strings.Contains(suffix, ".") {
			collect(suffix)
		}
		next := strings.Index(topic[i+1:], ".")
		if next < 0 {
			break
		}
		i += next + 1
	}
	if len(candidates) != 1 {
		return "", false
	}
	for repo := range candidates {
		return repo, true
	}
	return "", false
}
