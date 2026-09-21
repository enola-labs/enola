package perf

import (
	"context"

	"github.com/enola-labs/enola/internal/facts"
)

// PropPerfRisk marks a symbol this analyzer reports a HIGH-severity finding
// against, and says which kind. Presence is the boolean, as with orphan_class: a
// separate `perf_risk: true` beside it would be two props for one fact that could
// disagree.
//
// A symbol with more than one high-severity kind carries the worst one, ranked the
// way analyze() ranks them, so the prop is a single stable string rather than a set
// whose order could churn a diff.
const PropPerfRisk = "perf_risk"

// Annotate marks high-severity performance risks on their symbol facts, so a diff
// between two snapshots can say WHICH function a change made expensive.
//
// It exists because this explainer caps its insight list at 50 plus a rollup, and
// a diff identifies findings by title: past the cap, a newly slow function moved
// only the rollup's number and the diff could not name it. On enola's own
// repository the analyzer produces 1,261 findings and publishes 51, so that is
// 1,210 findings a diff could not see. Props are diffed as fact attributes rather
// than as insights, which is the channel that reaches past the cap — the same
// reason orphans stamps orphan_class and metrics stamps the Martin measures.
//
// # Why only the high tier
//
// High severity means evidence rather than a name: a confirmed I/O call (a storage
// fact, the extractor's performs_io, an unambiguous DB or network primitive) or a
// route handler, which is always hot. Medium is a curated-keyword match and low is
// a cold path. Annotating those would put "your change made this slow" into a diff
// on evidence that does not support the sentence, which is the false-positive
// problem arriving through a different door. Both tiers stay available through
// analyze_performance, which presents them with their caveats attached.
//
// # Scope caveat
//
// The label is an estimate of structural worst case from parser facts — loop
// nesting and call shape — and not a measurement. Nothing here knows whether the
// loop is hot. It is more misleading as a persisted prop than as a finding, which
// is why it carries the kind rather than a bare severity: `call-in-loop` says what
// was seen, where `slow: true` would say what was concluded.
func (e *Explainer) Annotate(_ context.Context, store *facts.Store) error {
	// Keyed by (repo, name) for the reason orphans is: symbol names are unique only
	// within a repo, and a multi-repo snapshot can hold same-named symbols in
	// several of them.
	type key struct{ repo, name string }
	risky := make(map[key]string)
	for _, f := range Analyze(store) {
		if f.Severity != "high" {
			continue
		}
		// analyze() returns findings worst-first, so the first one seen for a symbol
		// is the one worth recording; later kinds for the same symbol do not overwrite.
		k := key{f.Repo, f.Symbol}
		if _, seen := risky[k]; !seen {
			risky[k] = f.Kind
		}
	}
	if len(risky) == 0 {
		return nil
	}

	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindSymbol {
			return
		}
		kind, ok := risky[key{f.Repo, f.Name}]
		if !ok {
			// Written ONLY for a symbol that carries a risk. Marking every other
			// symbol would add a prop to every symbol fact in the graph, and a diff
			// against a baseline taken without annotations would report thousands of
			// changed facts instead of the few that moved.
			return
		}
		if f.Props == nil {
			f.Props = make(map[string]any, 1)
		}
		f.SetProp(PropPerfRisk, kind)
	})
	return nil
}
