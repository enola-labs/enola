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

// cdcPrefix opens a change-data-capture topic: `cdc.<database>.<table>`, written by
// a CDC connector from the database's binlog rather than by any code. The data is
// the database owner's, so the owner is named by the database segment, not by
// the prefix.
const cdcPrefix = "cdc."

// topicOwners returns the names a topic's owner may go by, most likely first: the
// leading segment, or for a CDC topic its database, as written and with
// underscores as hyphens (a service's database `svc_orders` for `svc-orders`).
func (s *Signal) topicOwners(topic string) []string {
	topic = strings.TrimSpace(topic)
	if rest, ok := strings.CutPrefix(topic, cdcPrefix); ok {
		db, _, _ := strings.Cut(rest, ".")
		if db == "" {
			return nil
		}
		var out []string
		seen := map[string]bool{}
		add := func(name string) {
			if name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		// A database per environment (`svc_orders_sandbox`) is the same owner's.
		for _, name := range []string{db, trimEnvSuffix(db)} {
			add(name)
			add(strings.ReplaceAll(name, "_", "-"))
		}
		return out
	}
	if owner := s.topicOwner(topic); owner != "" {
		return []string{owner}
	}
	return nil
}

// maxOwnerSegment bounds how far into a topic name an owner is looked for: the
// namespace a broker or a contract registry puts first is a segment or two deep.
const maxOwnerSegment = 4

// innerOwner resolves a topic whose leading segment names no loaded repo by the
// first of its next segments that does, the last segment excepted (it names the
// event, not its owner).
func (s *Signal) innerOwner(topic string, in plugin.SignalInput) (string, bool) {
	segs := strings.Split(strings.TrimSpace(topic), s.v.TopicOwnerSeparator)
	for i := 1; i < len(segs)-1 && i < maxOwnerSegment; i++ {
		if segs[i] == "" {
			continue
		}
		if label, ok := in.ResolveRepo(segs[i]); ok {
			return label, true
		}
	}
	return "", false
}

// cdcEnvSuffixes name the environment a database copy serves.
var cdcEnvSuffixes = []string{"_sandbox", "_staging", "_production", "_prod", "_dev", "_test"}

func trimEnvSuffix(db string) string {
	for _, suf := range cdcEnvSuffixes {
		if trimmed, ok := strings.CutSuffix(db, suf); ok && trimmed != "" {
			return trimmed
		}
	}
	return db
}

// linkKafka binds asynchronous coupling: a repo that references a Kafka topic owned
// by ANOTHER loaded repo consumes that repo's events, so it depends on it. The edge
// is drawn consumer -> producer (mirroring HTTP client -> server), keyed on the
// topic name's owning-service prefix — the same leading-segment-to-repo resolution
// linkImports uses, and robust to the producer side being unparsed. A repo's
// reference to its OWN topic (owner == repo) is intra-repo and draws no edge, and a
// topic owned by no loaded repo (an export sink, a third-party service) is simply
// left unlinked.
// CoverageEdgeType is the edge type consumed topics are counted under in a service's
// edge_coverage.
const CoverageEdgeType = "kafka"

func (s *Signal) Contribute(in plugin.SignalInput, out plugin.EvidenceSink) {
	producers := producerRepos(in.Facts())
	for _, f := range in.Facts() {
		if f.Repo == "" || f.Kind != facts.KindStorage {
			continue
		}
		if f.PropString("storage_kind") != facts.StorageKindTopic {
			continue
		}
		// A topic named in test code (an integration suite's own config, a fixture) is
		// the test's broker, not a dependency of the service.
		if facts.IsTestPath(f.File) {
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
		owner := ""
		inner := false
		if !ok {
			for _, candidate := range s.topicOwners(f.Name) {
				owner = candidate
				if label, ok = in.ResolveRepo(candidate); ok {
					break
				}
			}
		}
		// A namespaced topic names its owner further in
		// (`contracts.backend.orders.order_placed`): when the leading segment
		// names no loaded repo, the first later segment that does is the owner.
		if !ok {
			if label, ok = s.innerOwner(f.Name, in); ok {
				inner = true
			}
		}
		if ok && label == f.Repo {
			continue // the repo's own topic: it is the producer
		}
		// Every consumed topic is a detected outbound edge, as every client call is for
		// HTTP: resolved when a loaded repo owns it, outside the snapshot when its name
		// says whose it is and that repo is not loaded, unresolved otherwise.
		cov := out.Coverage(f.Repo, CoverageEdgeType)
		cov.Detected++
		if !ok {
			if owner != "" {
				cov.Outside++
			}
			continue
		}
		cov.Resolved++
		e := out.Edge(f.Repo, label)
		e.Via(facts.ViaKafka)
		if inner {
			e.Confidence("probable")
		}
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
		if f.PropString(facts.PropMessagingRole) != facts.MessagingRoleProducer || facts.IsTestPath(f.File) {
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
