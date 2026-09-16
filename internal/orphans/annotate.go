package orphans

import (
	"context"

	"github.com/enola-labs/enola/internal/facts"
)

// PropOrphanClass marks a symbol fact the analyzer found unreferenced, and says which
// kind of orphan it is: "isolated" (nothing references it and it references nothing) or
// "unreferenced" (nothing calls it, but it still calls others).
//
// Presence IS the boolean. A separate `orphan: true` alongside it would be two props for
// one fact, and they could disagree.
const PropOrphanClass = "orphan_class"

// Annotate marks HIGH-CONFIDENCE orphans on their symbol facts, so a diff between two
// snapshots reports code this change made dead — attributed to the symbol, not to a
// counter.
//
// Without this, dead code reaches a diff only as insights, which are capped at 50 per
// repository plus a rollup. Nine of the ten corpus repositories exceed that cap (superset
// has 4,191 orphans), so past the cap a newly-dead symbol moved only the rollup's number
// and the diff could not say which symbol it was.
//
// # Why only the high tier
//
// `high` means the kind's incoming usage is reliably captured as edges — plain function
// calls. `medium` (struct/class/interface) and `low` (method/type/const/var) are leads:
// type usage and dynamic dispatch are not edge-tracked, so a symbol can look unreferenced
// while being used as a field type or reached through an interface. Annotating those
// would put "your change made this dead" into a diff on evidence that does not support
// the sentence — the false-positive problem, arriving through a different door.
//
// It is also an order of magnitude of volume: 1,373 high-confidence orphans across the
// corpus against 12,678 total. The other tiers remain available through find_orphans,
// which presents them as leads with their caveats attached — the right frame for them.
//
// # Scope caveat
//
// Orphan status is SNAPSHOT-SCOPED, and more misleading as a persisted prop than as a
// finding: a symbol with no references in a single-repo snapshot may be referenced once
// another repo is appended to the graph. The prop describes the graph that was loaded,
// not the world.
func (e *Explainer) Annotate(_ context.Context, store *facts.Store) error {
	// Keyed by (repo, name): symbol names are unique only within a repo, and a multi-repo
	// snapshot can hold same-named symbols in several of them. Keying on the name alone
	// would mark one repo's live symbol dead because another repo's namesake is not
	// called.
	type key struct{ repo, name string }
	dead := make(map[key]string)
	for _, o := range Detect(store) {
		if o.Confidence != confHigh {
			continue
		}
		dead[key{o.Repo, o.Name}] = o.Class
	}
	if len(dead) == 0 {
		return nil
	}

	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindSymbol {
			return
		}
		class, ok := dead[key{f.Repo, f.Name}]
		if !ok {
			// Written ONLY when the symbol is an orphan. Marking every live symbol
			// `orphan_class: ""` would add a prop to every symbol fact in the graph, and
			// a diff against a baseline taken without annotations would then report
			// thousands of changed facts instead of the handful that actually moved.
			return
		}
		if f.Props == nil {
			f.Props = make(map[string]any, 1)
		}
		f.Props[PropOrphanClass] = class
	})
	return nil
}
