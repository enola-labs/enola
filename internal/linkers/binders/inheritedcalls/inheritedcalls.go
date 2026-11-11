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
//
// The binder also resolves the calls an extractor could not, on the object a method
// runs on: PropMemberCalls lists them (`common` for self.common(), `helper.work` for
// self.helper.work()), because what they name is declared in another file, on a base
// class. Each is looked up from the method's type through its ancestors, an
// attribute's type read from PropFieldTypes on the type or ancestor declaring it,
// and becomes a calls relation only when it lands on a symbol. Both props are
// working data and are removed once read.
package inheritedcalls

import (
	"context"
	"log"
	"slices"
	"strings"

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
	members := memberEdges(store.FactsRef(), ix)
	if len(rewrite) == 0 && len(members) == 0 && !hasWorkingProps(store.FactsRef()) {
		return nil
	}

	rewritten, callers, added := 0, 0, 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindSymbol {
			return
		}
		f.DelProp(facts.PropMemberCalls)
		f.DelProp(facts.PropFieldTypes)
		for _, t := range members[key{f.Repo, f.Name}] {
			if !f.HasRelation(facts.RelCalls, t) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
				added++
			}
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
		f.AddInheritedCalls(via...)
		callers++
	})
	log.Printf("[inherited-calls] %d call(s) from %d caller(s) resolved to an inherited member; %d call(s) on self or an attribute resolved", rewritten, callers, added)
	return nil
}

// memberEdges resolves every PropMemberCalls entry to the declaration it reaches,
// per calling symbol.
func memberEdges(fs []facts.Fact, ix *hierarchy.Index) map[[2]string][]string {
	fields := map[[2]string]map[string]string{}
	for _, f := range fs {
		if f.Kind != facts.KindSymbol {
			continue
		}
		switch m := f.PropAny(facts.PropFieldTypes).(type) {
		case map[string]string:
			fields[[2]string{f.Repo, f.Name}] = m
		case map[string]any:
			out := make(map[string]string, len(m))
			for k, v := range m {
				if s, ok := v.(string); ok {
					out[k] = s
				}
			}
			fields[[2]string{f.Repo, f.Name}] = out
		}
	}
	out := map[[2]string][]string{}
	for _, f := range fs {
		calls := f.PropStrings(facts.PropMemberCalls)
		if f.Kind != facts.KindSymbol || len(calls) == 0 {
			continue
		}
		owner, _ := hierarchy.SplitMember(f.Name)
		if owner == "" || !ix.IsType(f.Repo, owner) {
			continue
		}
		for _, c := range calls {
			typ, method := owner, c
			if attr, m, ok := strings.Cut(c, "."); ok {
				if strings.Contains(m, ".") {
					continue
				}
				typ, method = fieldType(ix, fields, f.Repo, owner, attr), m
			}
			if typ == "" {
				continue
			}
			decl, ok := ix.Lookup(f.Repo, typ, method)
			if !ok || decl == f.Name {
				continue
			}
			k := [2]string{f.Repo, f.Name}
			if !slices.Contains(out[k], decl) {
				out[k] = append(out[k], decl)
			}
		}
	}
	return out
}

// fieldType is the repository type an attribute of a type holds: as declared on the
// type, else on the nearest ancestor declaring it.
func fieldType(ix *hierarchy.Index, fields map[[2]string]map[string]string, repo, typ, attr string) string {
	for _, t := range append([]string{typ}, ix.Ancestors(repo, typ)...) {
		if raw := fields[[2]string{repo, t}][attr]; raw != "" {
			return ix.ResolveType(repo, t, raw)
		}
	}
	return ""
}

func hasWorkingProps(fs []facts.Fact) bool {
	for _, f := range fs {
		if _, ok := f.Prop(facts.PropMemberCalls); ok {
			return true
		}
		if _, ok := f.Prop(facts.PropFieldTypes); ok {
			return true
		}
	}
	return false
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
