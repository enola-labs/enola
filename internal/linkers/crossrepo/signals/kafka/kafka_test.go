package kafka

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

type fakeInput struct {
	plugin.SignalInput
	facts []facts.Fact
}

func (f fakeInput) Facts() []facts.Fact { return f.facts }
func (f fakeInput) ResolveRepo(candidate string) (string, bool) {
	if candidate == "svc-orders" {
		return candidate, true
	}
	return "", false
}

type fakeEdge struct{}

func (fakeEdge) Via(string)                    {}
func (fakeEdge) Confidence(string)             {}
func (fakeEdge) Sample(plugin.Bucket, string)  {}
func (fakeEdge) Unverified(plugin.Bucket, int) {}

type fakeSink struct {
	plugin.EvidenceSink
	edges int
}

func (s *fakeSink) Edge(_, _ string) plugin.EdgeEvidence { s.edges++; return fakeEdge{} }

func topic(protocol, role string) facts.Fact {
	return facts.Fact{Kind: facts.KindStorage, Name: "svc-orders.order_created", Repo: "svc-billing",
		Props: map[string]any{"storage_kind": facts.StorageKindTopic, "messaging": protocol, "messaging_role": role}}
}

func TestExplicitProtocolAndRoleGateKafkaLinking(t *testing.T) {
	sink := &fakeSink{}
	New(vocab.Default()).Contribute(fakeInput{facts: []facts.Fact{
		topic("mqtt", facts.MessagingRoleConsumer),
		topic("kafka", facts.MessagingRoleProducer),
		topic("kafka", facts.MessagingRoleConsumer),
		topic("kafka-secure", facts.MessagingRoleConsumer),
	}}, sink)
	if sink.edges != 2 {
		t.Fatalf("Kafka and secure Kafka consumers should link, got %d edges", sink.edges)
	}
}

func TestKafkaProtocolFamily(t *testing.T) {
	tests := map[string]bool{
		"kafka": true, "KAFKA": true, " kafka-secure ": true,
		"mqtt": false, "kafkaish": false, "": false,
	}
	for protocol, want := range tests {
		if got := facts.IsKafkaProtocol(protocol); got != want {
			t.Errorf("facts.IsKafkaProtocol(%q) = %v, want %v", protocol, got, want)
		}
	}
}

type recordingSink struct {
	plugin.EvidenceSink
	edges [][2]string
}

func (s *recordingSink) Edge(from, to string) plugin.EdgeEvidence {
	s.edges = append(s.edges, [2]string{from, to})
	return fakeEdge{}
}

type repoInput struct {
	plugin.SignalInput
	facts []facts.Fact
	repos map[string]bool
}

func (r repoInput) Facts() []facts.Fact { return r.facts }
func (r repoInput) ResolveRepo(candidate string) (string, bool) {
	return candidate, r.repos[candidate]
}

func topicFact(repo, name, role string) facts.Fact {
	return facts.Fact{Kind: facts.KindStorage, Name: name, Repo: repo,
		Props: map[string]any{"storage_kind": facts.StorageKindTopic, "messaging": "kafka", "messaging_role": role}}
}

func TestDeclaredProducerResolvesNamespacedTopics(t *testing.T) {
	repos := map[string]bool{"shop": true, "svc-orders": true, "svc-billing": true, "platform": true}
	tests := []struct {
		name  string
		facts []facts.Fact
		want  [][2]string
	}{
		{
			name: "namespaced consumer topic matches the one declared producer",
			facts: []facts.Fact{
				topicFact("shop", "shop.orders.order_placed", facts.MessagingRoleProducer),
				topicFact("svc-billing", "platform.events.shop.orders.order_placed", facts.MessagingRoleConsumer),
			},
			want: [][2]string{{"svc-billing", "shop"}},
		},
		{
			name: "exact name produced by a repo the leading segment does not name",
			facts: []facts.Fact{
				topicFact("svc-orders", "orders.order_placed", facts.MessagingRoleProducer),
				topicFact("svc-billing", "orders.order_placed", facts.MessagingRoleConsumer),
			},
			want: [][2]string{{"svc-billing", "svc-orders"}},
		},
		{
			name: "two producer repos match: fall back to the leading segment",
			facts: []facts.Fact{
				topicFact("shop", "shop.orders.order_placed", facts.MessagingRoleProducer),
				topicFact("svc-orders", "orders.order_placed", facts.MessagingRoleProducer),
				topicFact("svc-billing", "platform.events.shop.orders.order_placed", facts.MessagingRoleConsumer),
			},
			want: [][2]string{{"svc-billing", "platform"}},
		},
		{
			name: "one-segment suffix never matches",
			facts: []facts.Fact{
				topicFact("svc-orders", "order_placed", facts.MessagingRoleProducer),
				topicFact("svc-billing", "platform.order_placed", facts.MessagingRoleConsumer),
			},
			want: [][2]string{{"svc-billing", "platform"}},
		},
		{
			name: "a repo consuming what it produces itself draws nothing",
			facts: []facts.Fact{
				topicFact("shop", "shop.orders.order_placed", facts.MessagingRoleProducer),
				topicFact("shop", "shop.orders.order_placed", facts.MessagingRoleConsumer),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSink{}
			New(vocab.Default()).Contribute(repoInput{facts: tc.facts, repos: repos}, sink)
			if len(sink.edges) != len(tc.want) {
				t.Fatalf("edges = %v, want %v", sink.edges, tc.want)
			}
			for i := range tc.want {
				if sink.edges[i] != tc.want[i] {
					t.Errorf("edge %d = %v, want %v", i, sink.edges[i], tc.want[i])
				}
			}
		})
	}
}
