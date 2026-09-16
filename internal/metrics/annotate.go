package metrics

import (
	"context"

	"github.com/enola-labs/enola/internal/facts"
)

// Prop keys written onto module facts. Named here rather than inline so the diff, the
// dashboard and anything else reading them share one spelling.
//
// These are the five Martin metrics and nothing else. DataHolderRatio is deliberately
// absent: it exists to soften the "extract interfaces" advice in a finding, not to be
// tracked over time, and every prop written here is a prop every future diff has to
// carry and explain.
const (
	PropAfferent     = "afferent"
	PropEfferent     = "efferent"
	PropInstability  = "instability"
	PropAbstractness = "abstractness"
	PropDistance     = "distance"
)

// Annotate writes each package's computed metrics onto its module fact.
//
// This is what makes coupling and instability DIFFABLE. The explainer can only publish
// findings — packages far enough off the main sequence to be worth flagging — which on a
// typical repo is a handful out of dozens. Everything else it computed was discarded, so
// a diff could never report that a package's instability moved 0.30 -> 0.72: the number
// existed only inside one snapshot's analysis and was never written down.
//
// As props they are part of the snapshot, so the existing fact-level diff reports their
// movement for free, attributed to the module that moved. No new diff machinery, and no
// flood of one-finding-per-package insights to achieve it.
//
// Determinism: the values come from compute(), which already rounds to three decimals —
// necessary because these are written into facts.jsonl and hashed into the snapshot id,
// where an unrounded float would let an unchanged tree produce a different snapshot on
// every run.
func (e *Explainer) Annotate(_ context.Context, store *facts.Store) error {
	pkgs, edges, _ := collect(store)

	// Keyed by (repo, package): module names are only unique WITHIN a repo, and a
	// multi-repo snapshot routinely holds several packages called "src" or "internal".
	// Keying on the name alone would write one repo's coupling onto another's module.
	type key struct{ repo, pkg string }
	byPkg := make(map[key]PackageMetric, len(pkgs))
	for _, m := range compute(pkgs, edges) {
		byPkg[key{m.Repo, m.Package}] = m
	}

	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindModule {
			return
		}
		m, ok := byPkg[key{f.Repo, f.Name}]
		if !ok {
			return
		}
		// A package with NO COUNTED TYPES is excluded, exactly as the aggregate
		// reporting excludes it: abstractness is forced to 0 for want of anything to
		// divide by, so distance = |0 + I - 1| is arithmetic rather than a measurement.
		// Annotating it anyway would publish a number the tool itself declines to
		// average, and every diff would then carry a "distance" movement for packages
		// where distance means nothing.
		//
		// Ca and Ce ARE meaningful for such a package, but they are not written either:
		// a fact carrying two of the five metrics invites a reader to compare it with
		// one carrying all five, and the pair that is missing is the pair that would
		// tell them not to.
		if m.ClassesInterfaces == 0 {
			return
		}
		if f.Props == nil {
			f.Props = make(map[string]any, 5)
		}
		f.Props[PropAfferent] = m.Ca
		f.Props[PropEfferent] = m.Ce
		f.Props[PropInstability] = m.Instability
		f.Props[PropAbstractness] = m.Abstractness
		f.Props[PropDistance] = m.Distance
	})
	return nil
}
