package plugin

import (
	"context"

	"github.com/enola-labs/enola/internal/facts"
)

// BindStage is when a Binder runs relative to cross-repo linking. It is a declared
// property of the binder rather than an argument the engine passes, because the
// ordering it encodes is a correctness constraint, not a scheduling preference:
// a binder that rewrites route identities MUST run before the linker matches on
// them, and one that reads the linker's verdicts MUST run after. Both used to be
// enforced only by the order of five calls in a function body, where nothing
// recorded which of them mattered.
type BindStage string

const (
	// StagePreLink runs before cross-repo linking. Use it for a binder that
	// changes what the linker will MATCH ON — a provisional route name resolved to
	// its true wire path, say. Running such a binder after the linker would leave
	// the link computed against the un-rewritten value.
	StagePreLink BindStage = "pre-link"
	// StagePostLink runs after cross-repo linking, before the graph index is built.
	// Use it for a binder that reads the linker's output, or that only resolves
	// references WITHIN a repo and so has no ordering relationship to linking at
	// all. When in doubt this is the right stage: it is the more constrained one.
	StagePostLink BindStage = "post-link"
)

// Binder resolves references across an assembled fact set that no single extractor
// could resolve alone.
//
// An extractor sees one file at a time, in one language. Some edges are only
// discoverable once everything is in one store: a gRPC route read from a .proto and
// the method implementing it in Go or Python are extracted by different extractors
// that never see each other's output, and the convention tying them together
// (protoc's generated base type) is visible only in the assembled graph. That is the
// work a Binder does — and why it is a distinct plugin kind from an Extractor, which
// cannot see across files, and an Explainer, which may not modify facts.
//
// CONTRACT. Bind runs after extraction, in the stage the binder declares, before the
// graph index is built. Implementations:
//
//   - may only ADD OR UPDATE relations and props on facts already in the store.
//     Adding or removing FACTS would make the graph depend on which binders were
//     enabled, and two snapshots taken with different sets would no longer be
//     comparable — the same rule Annotator carries, one step wider because a Binder
//     may also add relations. (The one binder that rewrites a fact's Name does so via
//     remove-and-re-add to keep the store's name index consistent; that is a rewrite
//     of an existing fact, not a new one.)
//   - MUST be idempotent. Every binder re-runs on every snapshot and every append, so
//     a second run over its own output must be a no-op — check for the relation
//     before appending it.
//   - MUST be deterministic, and must not depend on the order binders were
//     registered in. Two binders in the same stage may run in any order, so a binder
//     that would behave differently depending on whether another has already run is
//     using the wrong stage, or the wrong gate.
//
// Determinism is the load-bearing one: facts.jsonl is hashed into the snapshot ID, so
// a binder whose output depends on registration order would make an unchanged tree
// produce a different snapshot from run to run.
type Binder interface {
	// Name identifies the binder (e.g. "grpc-impl", "http-handler").
	Name() string
	// Stage reports when this binder runs relative to cross-repo linking.
	Stage() BindStage
	// Bind resolves references over the assembled store, in place.
	Bind(ctx context.Context, store *facts.Store) error
}

// ---------------------------------------------------------------------------
// Cross-repo signals
// ---------------------------------------------------------------------------

// SignalPhase is when a CrossRepoSignal runs. There are exactly two, and the split
// is a correctness constraint rather than a convenience.
type SignalPhase string

const (
	// PhaseDirectional signals establish a DIRECTION: one repo calls, imports, or
	// consumes from another, so consumer and provider are distinguishable. They run
	// first and may not observe each other's evidence — which is what makes their
	// registration order unable to affect the output.
	PhaseDirectional SignalPhase = "directional"
	// PhaseSymmetric signals find a relationship with no inherent direction (two
	// repos sharing code). They run after every directional signal and MAY read the
	// accumulated evidence, because the only honest way to orient a symmetric signal
	// is to defer to a direction something else established. A symmetric signal that
	// found no such direction must record a coupling, not invent an edge.
	PhaseSymmetric SignalPhase = "symmetric"
)

// CrossRepoSignal derives evidence that one repository depends on another.
//
// Each signal reads the whole multi-repo fact set through SignalInput and reports what
// it found to an EvidenceSink; it never builds facts itself. That split is what lets a
// signal be added without touching the code that turns evidence into the graph — the
// four built-in signals (HTTP, imports, Kafka topics, shared code) previously had a
// dedicated field on one struct and a dedicated block in one materializer, so a fifth
// meant editing both.
//
// CONTRACT. Implementations must be deterministic and must not depend on registration
// order within their phase. A signal that needs to see another's evidence belongs in
// PhaseSymmetric; one that would need to see a symmetric signal's evidence is asking
// for something the model does not provide, and is probably really a directional
// signal.
//
// The governing bias is that a MISSING edge beats a WRONG one. Every gate in the
// built-in signals exists because a real false positive was found, and a signal that
// cannot confidently attribute a relationship should record nothing: an absent edge
// shows up as a coverage gap someone can go and look at, while a fabricated one is
// invisible and gets acted on.
type CrossRepoSignal interface {
	// Name identifies the signal (e.g. "http", "import", "kafka", "shared-code").
	Name() string
	// Phase reports when this signal runs.
	Phase() SignalPhase
	// Contribute reads the fact set and reports evidence.
	Contribute(in SignalInput, out EvidenceSink)
}

// SignalInput is the read side: the multi-repo fact set plus the derived indexes a
// signal would otherwise have to compute itself.
//
// The derived accessors exist because every one of them used to be a package-level
// function that walked the entire fact set on each call, several of them more than
// once per link. Behind this interface they are computed lazily and at most once per
// snapshot, so adding a signal that needs the same index costs nothing.
type SignalInput interface {
	// Facts returns the whole multi-repo fact set.
	Facts() []facts.Fact
	// Repos returns every loaded repo label, sorted.
	Repos() []string
	// ResolveRepo maps a candidate name (an import scope, a topic-name prefix) to a
	// loaded repo label, comparing normalized. Reports false when nothing matches.
	ResolveRepo(candidate string) (string, bool)
	// PrimaryLanguage returns a repo's dominant source language, or "" if unknown.
	PrimaryLanguage(repo string) string
	// TopDirs returns a repo's own top-level source directories.
	TopDirs(repo string) map[string]bool
	// OwnScopes returns the npm @scopes a repo publishes under.
	OwnScopes(repo string) map[string]bool
	// ModuleNames returns a repo's module names, longest first.
	ModuleNames(repo string) []string
	// HasSource reports whether source is available at all. It is distinct from a
	// failed ReadSource: "no reader was supplied" means verification cannot run and
	// must be skipped entirely, while "this one file would not read" means this one
	// candidate is unverifiable. Collapsing the two would silently turn every
	// name-matched candidate into a rejected one whenever source is absent.
	HasSource() bool
	// ReadSource returns the text of the file a fact came from. Reports false when
	// source is unavailable, which every caller must treat as "cannot verify" rather
	// than "does not match".
	ReadSource(f facts.Fact) (string, bool)
}

// Bucket names one class of evidence an edge can carry, together with the props it
// materializes into. It is what lets a new signal introduce its own evidence without
// editing the accumulator or the materializer: declare a Bucket, report samples into
// it, and the props appear.
type Bucket struct {
	// Name is the bucket's identity, and the sort key that makes materialization
	// order deterministic.
	Name string
	// CountProp receives the number of distinct samples.
	CountProp string
	// SamplesProp receives the samples themselves, sorted and capped.
	SamplesProp string
	// UnverifiedProp, when set, receives the signal's PRE-verification tally — but
	// only when it exceeds the number of samples that survived verification. It is
	// how a signal reports the gap between "matched by name" and "confirmed", which
	// is meaningless to hide and noise to always show.
	UnverifiedProp string
}

// The built-in buckets. The prop names are load-bearing: they are the queryable
// surface of a cross-repo dependency fact, so changing one changes what
// query_facts(kind=dependency) returns.
var (
	BucketEndpoints = Bucket{Name: "endpoints", CountProp: "endpoint_count", SamplesProp: "endpoints"}
	BucketImports   = Bucket{Name: "imports", CountProp: "import_count", SamplesProp: "import_samples"}
	BucketTopics    = Bucket{Name: "topics", CountProp: "topic_count", SamplesProp: "topic_samples"}
	BucketSymbols   = Bucket{Name: "symbols", CountProp: "symbol_count", SamplesProp: "symbol_samples", UnverifiedProp: "name_match_count"}

	// BucketParamEndpoints holds the endpoints of an HTTP edge that matched only because a
	// server path parameter absorbed a literal client segment (the opt-in
	// linking.match_literal_against_params). Each is in BucketEndpoints too; this names the
	// subset resting on the looser rule, so a reader can weigh it.
	BucketParamEndpoints = Bucket{Name: "param_segment_endpoints", CountProp: "param_segment_endpoint_count", SamplesProp: "param_segment_endpoints"}
)

// Coverage tallies, per consumer repo, how many outbound call sites a signal detected
// and how many it resolved. The difference is the blind spot — call sites enola saw
// but could not attribute — so a repo with no outbound edges but a non-zero unresolved
// count is a coverage gap rather than a genuine isolate. Reporting it is what keeps a
// signal's misses visible instead of silent.
type Coverage struct {
	Detected int
	Resolved int
	// External counts call sites aimed at a hardcoded third-party host, or ones
	// whose host-derived target hint resolves to no loaded repo. They are
	// expected non-matches, so they are bucketed apart from the blind spot.
	External int
	// Declared counts unmatched call sites attributed to the repo's single
	// declared http-client seam — intent supplying the attribution measurement
	// could not. Labeled, never resolved into an edge.
	Declared int
}

// EvidenceSink is the write side: what a signal reports into.
type EvidenceSink interface {
	// Edge returns the evidence accumulator for a directional consumer -> provider
	// dependency, creating it on first use.
	Edge(consumer, provider string) EdgeEvidence
	// Coupling returns the accumulator for a SYMMETRIC relationship between two
	// repos — recorded as queryable evidence that carries no relation, so it never
	// enters the traversable graph. Shared code is not a dependency, and unlike a
	// dependency it does not compose across hops.
	Coupling(a, b string) CouplingEvidence
	// Coverage returns the mutable coverage tally for one repo and one class of
	// outbound edge ("http_client", …). Keyed by edge type because a repo can have a
	// blind spot in one transport and full coverage in another, and collapsing them
	// would hide exactly the gap the tally exists to expose.
	Coverage(repo, edgeType string) *Coverage
	// DirectedPairs reports the consumer -> provider orientations already established
	// between two repos by directional signals, and whether any exists. A symmetric
	// signal uses it to decide between annotating a real dependency and recording a
	// standalone coupling. It is meaningful only in PhaseSymmetric; during
	// PhaseDirectional the answer is still being computed.
	DirectedPairs(a, b string) (pairs [][2]string, ok bool)
}

// EdgeEvidence accumulates the justification for one directional dependency.
type EdgeEvidence interface {
	// Via records HOW this dependency was observed ("http", "import", "kafka", …).
	// One edge may carry several.
	Via(label string)
	// Confidence records how trustworthy the match was; the strongest seen wins.
	Confidence(c string)
	// Sample records one piece of evidence in a bucket. Duplicates collapse.
	Sample(b Bucket, value string)
	// Unverified records the pre-verification tally for a bucket — see
	// Bucket.UnverifiedProp.
	Unverified(b Bucket, n int)
}

// CouplingEvidence accumulates a symmetric relationship. It deliberately has no
// Confidence: with no consumer and no provider there is no match to be confident
// about, only the samples themselves. It does carry Via, because which signal
// observed the coupling is still worth recording.
type CouplingEvidence interface {
	Via(label string)
	Sample(b Bucket, value string)
	Unverified(b Bucket, n int)
}
