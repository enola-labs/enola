// Package crossrepo links the per-repo architectural graphs of an appended multi-repo
// fact set into a single cross-repo "graph of graphs".
//
// In multi-repo (append) mode, facts from several repositories live in one store, each
// tagged with a Repo label, but the graph only ever connects facts within a single
// repo. This package derives the edges BETWEEN repos — not by knowing how, but by
// running registered plugin.CrossRepoSignal implementations and materializing whatever
// evidence they report.
//
// The signals themselves live in signals/: HTTP route role matching, import references,
// Kafka topic ownership, and shared code. Each is independent, and a new one is a new
// package plus a registration line. This package owns only the parts that cannot belong
// to any single signal:
//
//   - the two-phase run (directional signals first, then symmetric ones, which may read
//     the direction the first phase established — see plugin.SignalPhase),
//   - evidence accumulation and materialization (evidence.go),
//   - the derived indexes signals share, computed lazily and at most once (input.go).
//
// The result is expressed as synthetic facts: one KindService node per repo and one
// KindDependency fact per pair. Only the directional ones (type="cross_repo") attach a
// depends_on relation to the consumer's service node; because these are ordinary facts,
// they flow into Store.BuildGraph and make every traversal tool (traverse, find_path,
// impact_analysis, query_facts) cross-repo aware with no per-tool changes. Keeping the
// symmetric signal out of that relation matters: traversal composes depends_on across
// hops, and shared code does not compose — a repo calling one side of a copy-paste pair
// does not thereby reach the other.
package crossrepo

import (
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// SyntheticMarker tags every fact this package emits, so the engine can drop and
// recompute them idempotently on each append.
const SyntheticMarker = "crossrepo"

// SourceReader returns the text of the file a fact was extracted from, and whether it
// could be read. It is how a signal — which otherwise only ever sees facts — gets at
// source, so a shared type NAME can be checked against the actual code behind it.
// Callers that cannot supply source pass nil, and matching falls back to names alone.
// ComputeLinks calls it from several goroutines at once when given more than one job.
type SourceReader func(f facts.Fact) (string, bool)

// ComputeLinks runs the given signals over a multi-repo fact set and returns the
// synthetic facts connecting the repositories.
//
// It is pure and deterministic: the same facts and the same signal set always yield the
// same output in a stable, sorted order, so callers may recompute it idempotently after
// removing the prior synthetic facts. Signal registration order cannot affect the
// result — within a phase signals never observe each other, and evidence accumulation
// is commutative.
//
// src supplies file contents so shared type names can be verified against the code
// behind them; pass nil to match on names alone. v is the tuning vocabulary; it is
// passed rather than read from a global so two snapshots taken under different
// vocabularies cannot interfere, and so the value is visibly part of the input whose
// fingerprint the snapshot's config hash covers.
//
// jobs is how many source files a signal may read at once (below 1 counts as 1). The
// result does not depend on it.
func ComputeLinks(all []facts.Fact, src SourceReader, signals []plugin.CrossRepoSignal, v *vocab.Set, jobs int) []facts.Fact {
	in := newInput(all, src, jobs)
	if len(in.Repos()) < 2 {
		return nil // need at least two repos to have a cross-repo edge
	}

	out := newSink(v)

	// Two phases, in order. Within a phase the order is whatever the registry hands
	// back and must not matter; between phases it is a correctness constraint. The
	// sink tracks the running phase so it can tell evidence that establishes a
	// direction from evidence that does not, without either side declaring it.
	for _, phase := range []plugin.SignalPhase{plugin.PhaseDirectional, plugin.PhaseSymmetric} {
		out.phase = phase
		for _, s := range signals {
			if s.Phase() == phase {
				s.Contribute(in, out)
			}
		}
	}

	return materialize(out, in.Repos())
}
