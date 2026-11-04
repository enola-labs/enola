package crossrepo

import (
	"fmt"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// This file is the accumulate-and-materialize half of cross-repo linking: signals
// report evidence into a sink, and the sink turns what it collected into facts.
//
// Evidence used to be a struct with one typed field per signal (endpoints, imports,
// topics, symbols) and a materializer with a matching if-block per field, so a fifth
// signal meant editing both. Now a signal declares a plugin.Bucket and reports into
// it, and materialization is a loop — the only thing this file knows about any
// particular signal is nothing at all.

// edge accumulates everything justifying one consumer -> provider dependency.
type edge struct {
	s          *sink
	consumer   string
	provider   string
	via        map[string]bool
	buckets    map[string]*bucketData
	confidence string // "verified" or "probable" — max over contributing signals
}

// coupling records a SYMMETRIC relationship between two repos with no directional
// evidence in either direction. It is deliberately not an edge: neither repo imports,
// calls, or reaches the other, so a depends_on relation would assert a dependency that
// does not exist — and, because traversal composes depends_on across hops, would let an
// unrelated repo appear to reach one of these through the other.
type coupling struct {
	s       *sink
	repoA   string // lexicographically first, so the pair has one canonical form
	repoB   string
	via     map[string]bool
	buckets map[string]*bucketData
}

// bucketData is one bucket's accumulated samples, plus the pre-verification tally a
// signal may report alongside them.
type bucketData struct {
	spec       plugin.Bucket
	values     map[string]bool
	unverified int
}

func (b *bucketData) add(v string) {
	if b.values == nil {
		b.values = map[string]bool{}
	}
	b.values[v] = true
}

// sink is the EvidenceSink handed to every signal. It is not safe for concurrent use;
// signals run sequentially within a phase.
type sink struct {
	v *vocab.Set

	edges     map[string]*edge
	couplings map[string]*coupling
	coverage  map[string]map[string]*plugin.Coverage // repo -> edgeType -> tally

	// phase is the phase currently running. Via() consults it to record which labels
	// came from a symmetric signal.
	phase plugin.SignalPhase
	// symmetricVias is the set of via labels reported while a PhaseSymmetric signal
	// was running, so DirectedPairs can tell "this edge is oriented" from "this edge
	// carries only symmetric evidence".
	//
	// It is derived from WHEN a label was reported rather than declared by the signal,
	// because a signal may report several labels (the HTTP signal alone reports
	// "http", "http-client" and "grpc") and no single declaration could cover them.
	// Deriving it from the running phase makes a new signal correct by construction —
	// the failure this replaces was a hardcoded list of directional vias that a newly
	// added one was simply missing from.
	symmetricVias map[string]bool
}

func newSink(v *vocab.Set) *sink {
	return &sink{
		v:             v,
		edges:         map[string]*edge{},
		couplings:     map[string]*coupling{},
		coverage:      map[string]map[string]*plugin.Coverage{},
		symmetricVias: map[string]bool{},
	}
}

func (s *sink) Edge(consumer, provider string) plugin.EdgeEvidence {
	key := consumer + "\x00" + provider
	e, ok := s.edges[key]
	if !ok {
		e = &edge{s: s, consumer: consumer, provider: provider,
			via: map[string]bool{}, buckets: map[string]*bucketData{}}
		s.edges[key] = e
	}
	return e
}

func (s *sink) Coupling(a, b string) plugin.CouplingEvidence {
	if b < a {
		a, b = b, a // canonical order: the pair is symmetric, the key must be too
	}
	key := a + "\x00" + b
	c, ok := s.couplings[key]
	if !ok {
		c = &coupling{s: s, repoA: a, repoB: b,
			via: map[string]bool{}, buckets: map[string]*bucketData{}}
		s.couplings[key] = c
	}
	return c
}

func (s *sink) Coverage(repo, edgeType string) *plugin.Coverage {
	if s.coverage[repo] == nil {
		s.coverage[repo] = map[string]*plugin.Coverage{}
	}
	c, ok := s.coverage[repo][edgeType]
	if !ok {
		c = &plugin.Coverage{}
		s.coverage[repo][edgeType] = c
	}
	return c
}

// DirectedPairs reports the orientations directional signals established between two
// repos. It answers only for evidence that establishes a DIRECTION — an edge carrying
// nothing but symmetric evidence is not an answer, it is the question restated.
func (s *sink) DirectedPairs(a, b string) ([][2]string, bool) {
	var pairs [][2]string
	if s.hasDirectionalEvidence(a, b) {
		pairs = append(pairs, [2]string{a, b})
	}
	if s.hasDirectionalEvidence(b, a) {
		pairs = append(pairs, [2]string{b, a})
	}
	return pairs, len(pairs) > 0
}

// noteVia records a via label as symmetric when it was reported during the symmetric
// phase.
func (s *sink) noteVia(label string) {
	if s.phase == plugin.PhaseSymmetric {
		s.symmetricVias[label] = true
	}
}

// hasDirectionalEvidence reports whether a consumer -> provider edge exists and is
// backed by evidence that establishes a direction.
//
// The test is "carries any via other than a symmetric one" rather than an enumeration
// of the directional vias. An enumeration silently mis-classified new signals: it once
// omitted "grpc", so a pair linked only by gRPC calls still got a fabricated reverse
// edge. Anything a signal reports is directional unless a symmetric signal reported it,
// and symmetric signals are exactly the ones that declare PhaseSymmetric.
func (s *sink) hasDirectionalEvidence(consumer, provider string) bool {
	e, ok := s.edges[consumer+"\x00"+provider]
	if !ok {
		return false
	}
	for via := range e.via {
		if !s.symmetricVias[via] {
			return true
		}
	}
	return false
}

func (e *edge) Via(label string) {
	e.via[label] = true
	e.s.noteVia(label)
}

// Confidence keeps the strongest value seen: "verified" wins over "probable".
func (e *edge) Confidence(c string) {
	if e.confidence == "verified" {
		return
	}
	if c == "verified" || e.confidence == "" {
		e.confidence = c
	}
}

func (e *edge) Sample(b plugin.Bucket, value string) { bucketFor(e.buckets, b).add(value) }
func (e *edge) Unverified(b plugin.Bucket, n int)    { bucketFor(e.buckets, b).unverified = n }

func (c *coupling) Via(label string) {
	c.via[label] = true
	c.s.noteVia(label)
}
func (c *coupling) Sample(b plugin.Bucket, value string) { bucketFor(c.buckets, b).add(value) }
func (c *coupling) Unverified(b plugin.Bucket, n int)    { bucketFor(c.buckets, b).unverified = n }

func bucketFor(m map[string]*bucketData, spec plugin.Bucket) *bucketData {
	d, ok := m[spec.Name]
	if !ok {
		d = &bucketData{spec: spec}
		m[spec.Name] = d
	}
	return d
}

// writeBuckets renders every non-empty bucket into props, in bucket-name order so the
// result is deterministic regardless of the order signals ran or reported.
func writeBuckets(props map[string]any, buckets map[string]*bucketData, max int) {
	names := make([]string, 0, len(buckets))
	for n := range buckets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		d := buckets[n]
		if len(d.values) == 0 {
			continue
		}
		vals := sortedKeys(d.values)
		props[d.spec.CountProp] = len(vals)
		props[d.spec.SamplesProp] = capSamples(vals, max)
		// Only meaningful when verification actually narrowed the set; without it the
		// two counts are equal and the extra prop is noise.
		if d.spec.UnverifiedProp != "" && d.unverified > len(vals) {
			props[d.spec.UnverifiedProp] = d.unverified
		}
	}
}

// materialize turns accumulated evidence into facts: one service node per loaded repo,
// one dependency fact per directional edge, and one per symmetric coupling.
//
// Only directional edges attach a depends_on relation to the consumer's service node.
// The detailed per-pair dependency facts carry the evidence and are queryable but hold
// NO relations, so no stray "a -> b" graph node is created.
func materialize(s *sink, allRepos []string) []facts.Fact {
	keys := make([]string, 0, len(s.edges))
	for k := range s.edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	providers := map[string][]string{}
	repoSet := map[string]bool{}

	var depFacts []facts.Fact
	for _, k := range keys {
		e := s.edges[k]
		repoSet[e.consumer] = true
		repoSet[e.provider] = true
		providers[e.consumer] = append(providers[e.consumer], e.provider)

		props := map[string]any{
			"type":      facts.TypeCrossRepo,
			"synthetic": SyntheticMarker,
			"via":       sortedKeys(e.via),
		}
		if e.confidence != "" {
			props["confidence"] = e.confidence
		}
		writeBuckets(props, e.buckets, s.v.Thresholds.MaxSamples)

		depFacts = append(depFacts, facts.Fact{
			Kind:  facts.KindDependency,
			Name:  fmt.Sprintf("%s -> %s", e.consumer, e.provider),
			Repo:  e.consumer,
			Props: props,
		})
	}

	// Couplings are evidence facts only. They are NOT added to providers, so no
	// depends_on relation is created and they stay out of the traversable graph — the
	// whole point of separating them from edges. The name uses "<->" because the
	// relationship is symmetric; there is no consumer or provider.
	couplingKeys := make([]string, 0, len(s.couplings))
	for k := range s.couplings {
		couplingKeys = append(couplingKeys, k)
	}
	sort.Strings(couplingKeys)
	for _, k := range couplingKeys {
		c := s.couplings[k]
		props := map[string]any{
			"type":      facts.TypeCrossRepoSharedCode,
			"synthetic": SyntheticMarker,
			"via":       sortedKeys(c.via),
			"repos":     []string{c.repoA, c.repoB},
		}
		writeBuckets(props, c.buckets, s.v.Thresholds.MaxSamples)
		depFacts = append(depFacts, facts.Fact{
			Kind:  facts.KindDependency,
			Name:  fmt.Sprintf("%s <-> %s", c.repoA, c.repoB),
			Repo:  c.repoA,
			Props: props,
		})
	}

	// Every loaded repo becomes an addressable service node, even with no cross-repo
	// edges, so find_path/traverse can resolve isolated repo labels.
	for _, r := range allRepos {
		repoSet[r] = true
	}
	repos := make([]string, 0, len(repoSet))
	for r := range repoSet {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	out := make([]facts.Fact, 0, len(repos)+len(depFacts))
	for _, r := range repos {
		var rels []facts.Relation
		for _, p := range providers[r] {
			rels = append(rels, facts.Relation{Kind: facts.RelDependsOn, Target: p})
		}
		props := map[string]any{"synthetic": SyntheticMarker}
		if cov := coverageProp(s.coverage[r]); cov != nil {
			props["edge_coverage"] = cov
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindService,
			Name:      r,
			Repo:      r,
			Props:     props,
			Relations: rels,
		})
	}
	out = append(out, depFacts...)
	return out
}

// coverageProp renders a repo's per-edge-type tallies, sorted by edge type, or nil when
// nothing was detected. A node with no outbound edges but a non-zero unresolved count
// reads as a coverage gap rather than a true isolate.
func coverageProp(byType map[string]*plugin.Coverage) []map[string]any {
	var types []string
	for t, c := range byType {
		if c.Detected > 0 {
			types = append(types, t)
		}
	}
	if len(types) == 0 {
		return nil
	}
	sort.Strings(types)
	out := make([]map[string]any, 0, len(types))
	for _, t := range types {
		c := byType[t]
		out = append(out, map[string]any{
			"edge_type":  t,
			"detected":   c.Detected,
			"resolved":   c.Resolved,
			"external":   c.External,
			"declared":   c.Declared,
			"outside":    c.Outside,
			"unresolved": c.Detected - c.Resolved - c.External - c.Declared - c.Outside,
		})
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// capSamples bounds a sample list to max entries.
func capSamples(ss []string, max int) []string {
	if len(ss) > max {
		return ss[:max]
	}
	return ss
}
