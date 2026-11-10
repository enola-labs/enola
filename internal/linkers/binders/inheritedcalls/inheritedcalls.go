// Package inheritedcalls points a call at the member a type inherits.
//
// An extractor spells a call by the receiver's type as the file shows it, so
// `c.run()` on a Concrete is a call to `Concrete.run`. When run is declared on a
// base class, often in another file, no symbol has that name: the edge dangles,
// impact on Base.run misses the caller, Base.run reads as unused, and a flow stops.
// Only the assembled facts hold the class tree, so this is a binder.
//
// A calls target `T.m` that names no symbol, where T is a type of the caller's
// repository and an ancestor of T declares m, is rewritten to the nearest such
// declaration. The spelling it replaced is recorded on the caller under
// PropInheritedCalls, because the type a call went through is the type a self-call
// inside the inherited member dispatches on, and the edge no longer says it.
//
// A target that names a symbol is never touched, so a second run is a no-op.
package inheritedcalls

import (
	"context"
	"log"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/hierarchy"
	"github.com/enola-labs/enola/pkg/plugin"
)

type Binder struct{}

func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "inherited-calls" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	ix := hierarchy.Build(store.FactsRef())

	// Decided before anything is written: a lookup takes the store's read lock,
	// which UpdateWhere holds for writing. A rewrite depends only on the repository
	// and the target, never on the caller.
	type key = [2]string
	rewrite := map[key]string{}
	decided := map[key]bool{}
	for _, f := range store.FactsRef() {
		if f.Kind != facts.KindSymbol {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls {
				continue
			}
			k := key{f.Repo, r.Target}
			if decided[k] {
				continue
			}
			decided[k] = true
			owner, member := hierarchy.SplitMember(r.Target)
			if owner == "" || !ix.IsType(f.Repo, owner) || namesSymbol(store, f.Repo, r.Target) {
				continue
			}
			if decl, ok := ix.Lookup(f.Repo, owner, member); ok && decl != r.Target {
				rewrite[k] = decl
			}
		}
	}
	if len(rewrite) == 0 {
		return nil
	}

	rewritten, callers := 0, 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindSymbol {
			return
		}
		var via []string
		kept := f.Relations[:0:0]
		changed := false
		for _, r := range f.Relations {
			decl, ok := rewrite[key{f.Repo, r.Target}]
			if r.Kind != facts.RelCalls || !ok {
				kept = append(kept, r)
				continue
			}
			changed = true
			via = append(via, r.Target)
			rewritten++
			if f.HasRelation(facts.RelCalls, decl) || containsCall(kept, decl) {
				continue // the caller already calls the declaration
			}
			kept = append(kept, facts.Relation{Kind: facts.RelCalls, Target: decl})
		}
		if !changed {
			return
		}
		f.Relations = kept
		f.SetProp(facts.PropInheritedCalls, mergeSorted(f.PropAny(facts.PropInheritedCalls), via))
		callers++
	})
	log.Printf("[inherited-calls] %d call(s) from %d caller(s) resolved to an inherited member", rewritten, callers)
	return nil
}

func namesSymbol(store *facts.Store, repo, name string) bool {
	for _, f := range store.LookupByExactName(name) {
		if f.Kind == facts.KindSymbol && f.Repo == repo {
			return true
		}
	}
	return false
}

func containsCall(rels []facts.Relation, target string) bool {
	for _, r := range rels {
		if r.Kind == facts.RelCalls && r.Target == target {
			return true
		}
	}
	return false
}

// mergeSorted adds names to a recorded list, which reads back from JSON as []any.
func mergeSorted(prior any, add []string) []string {
	set := map[string]bool{}
	switch p := prior.(type) {
	case []string:
		for _, s := range p {
			set[s] = true
		}
	case []any:
		for _, s := range p {
			if str, ok := s.(string); ok {
				set[str] = true
			}
		}
	}
	for _, s := range add {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
