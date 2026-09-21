package dartextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// This file holds the passes that run after every file has been walked, because none of
// them can be decided from one file alone.

// resolveCallTargets rewrites bare reference targets to the canonical symbol they name.
//
// The walker emits a call target as it is WRITTEN (`fetch`, `User.named`) because a
// receiver's static type is not tracked and a file cannot see another file's
// declarations. This pass binds those against the assembled symbol set.
//
// The binding rule is deliberately narrow: a name is rewritten only when the project
// declares EXACTLY ONE symbol with it. An ambiguous short name is left bare, which is
// the outcome that matters — a bare target still lets dead-code matching see the symbol
// used, while a target picked from several candidates would be a fabricated edge into
// whichever module happened to sort first. `build`, `dispose`, `toJson` and `copyWith`
// are declared by hundreds of types in any Flutter app, so this is the common case and
// not the corner one.
func resolveCallTargets(all []facts.Fact) []facts.Fact {
	// short name -> canonical names of CALLABLE symbols declaring it.
	//
	// Callable-only is load-bearing. A bare call target is resolved by unique short
	// name, and Dart's short names collide across kinds: immich declares an enum
	// constant `LogLevel.severe` and also calls `log.severe(...)` on a logger. With
	// every symbol kind in the index the constant was the unique `severe`, so 117 call
	// sites bound to it — and god-class then reported a data constant as a high-fan-in
	// symbol with 117 dependents. A call resolves to something callable or it stays
	// bare, which dead-code matching still handles.
	byShort := map[string][]string{}
	// bare type name -> canonical names, for implements/injects targets
	byType := map[string][]string{}
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		short := f.Name
		if i := strings.LastIndexByte(short, '.'); i >= 0 {
			short = short[i+1:]
		}

		switch f.PropString("symbol_kind") {
		case facts.SymbolMethod, facts.SymbolFunc:
			byShort[short] = append(byShort[short], f.Name)
		case facts.SymbolClass, facts.SymbolInterface, facts.SymbolEnum, facts.SymbolType:
			byType[short] = append(byType[short], f.Name)
		}
	}

	unique := func(idx map[string][]string, name string) (string, bool) {
		if cands := idx[name]; len(cands) == 1 {
			return cands[0], true
		}
		return "", false
	}

	for i := range all {
		f := &all[i]
		for j := range f.Relations {
			r := &f.Relations[j]
			// An already-canonical target contains the module path; leave it.
			if strings.Contains(r.Target, "/") {
				continue
			}
			switch r.Kind {
			case facts.RelImplements, facts.RelInjects, facts.RelInstantiates:
				if canon, ok := unique(byType, r.Target); ok {
					r.Target = canon
				}
			case facts.RelCalls:
				if canon, ok := unique(byShort, r.Target); ok {
					r.Target = canon
					continue
				}
				// `Foo.named` — try the member under its declaring type.
				if typeName, member, found := strings.Cut(r.Target, "."); found {
					if owner, ok := unique(byType, typeName); ok {
						if canon, ok := unique(byShort, owner+"."+member); ok {
							r.Target = canon
						} else {
							r.Target = owner + "." + member
						}
					}
				}
			}
		}
	}
	return all
}

// attributeSymbolsToModules gives every symbol a leading `declares` edge to the module
// that declares it.
//
// This is the convention the Go extractor established and that the package-metrics
// explainer depends on: it reads a symbol's FIRST `declares` target as
// the package the symbol belongs to. A Dart class already carries `declares` edges to
// its own members, so without this the explainer read the first MEMBER name as a
// package — minting one phantom package per class. Measured on drift: 1,746 "packages"
// against 199 real modules, which is the same failure the exported-surface explainer
// documents for .NET (`MediaBrowser.Controller/*` collapsing into a phantom
// `MediaBrowser`).
//
// Prepended rather than appended so the module edge reads first, matching the shape
// the Go extractor emits. It is no longer load-bearing: the explainer now resolves a
// symbol's declaring module against the known module set rather than taking the first
// `declares` target, so a member edge arriving first can no longer be mistaken for a
// package. Verified by emitting this edge LAST and confirming the metrics are
// unchanged. The member edges are kept either way: they are what makes a type's own
// surface walkable without relying on the graph's synthesized has_method.
func attributeSymbolsToModules(all []facts.Fact) []facts.Fact {
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol || f.File == "" {
			continue
		}
		dir := factpath.Dir(f.File)
		rels := make([]facts.Relation, 0, len(f.Relations)+1)
		rels = append(rels, facts.Relation{Kind: facts.RelDeclares, Target: dir})
		rels = append(rels, f.Relations...)
		f.Relations = rels
	}
	return all
}

// resolveRoutePathRefs dereferences route paths declared as a constant.
//
// `GoRoute(path: WorkspaceStartScreen.routeName)` is how a large Flutter app keeps a
// screen's path beside the screen, and it means the router file holds no literal at
// all — appflowy declares 30-odd routes this way and exactly one with a literal. The
// constant lives in another file, so this can only be done once every file has been
// walked.
//
// A reference that does not resolve takes its route WITH it. That is the deliberate
// half: the fact's Name is the route path, so an unresolved one would put a node named
// `WorkspaceStartScreen.routeName` into the graph — a path no app navigates to, which
// would then be matched against real routes by the cross-repo linker.
func resolveRoutePathRefs(all []facts.Fact, consts map[string]string) []facts.Fact {
	out := all[:0]
	for _, f := range all {
		ref := f.PropString(pathRefProp)
		if f.Kind != facts.KindRoute || ref == "" {
			out = append(out, f)
			continue
		}
		literal, ok := consts[ref]
		if !ok {
			// Try the bare member name: a route constant is frequently declared on a
			// mixin or a base the reference names differently.
			if _, member, cut := strings.Cut(ref, "."); cut {
				literal, ok = consts[member]
			}
		}
		if !ok || literal == "" {
			continue
		}
		f.Name = composePath(f.PropString(pathPrefixProp), literal)
		f.DelProp(pathRefProp)
		f.DelProp(pathPrefixProp)
		out = append(out, f)
	}
	return out
}

// computeDartPerformsIO propagates io_direct up the call graph into a transitive
// performs_io prop.
//
// The seed is a body that invokes an I/O API directly; the closure is what makes a
// per-iteration network call detectable when the I/O sits two wrapper layers down,
// which in a Flutter app it almost always does — a widget calls a repository which
// calls a data source which calls http. Without the closure the performance analyzer
// sees a loop calling `loadPage` and has no way to know that `loadPage` reaches the
// network.
//
// The fixpoint is monotone and therefore cycle-safe: a prop is only ever turned on, so
// a mutually recursive pair converges instead of oscillating.
func computeDartPerformsIO(all []facts.Fact) {
	idx := make(map[string]*facts.Fact, len(all))
	for i := range all {
		if all[i].Kind == facts.KindSymbol {
			idx[all[i].Name] = &all[i]
		}
	}
	// Short-name index for the targets resolution left bare, so the closure crosses an
	// ambiguous hop by name the way the Swift and TypeScript closures do. It is bounded
	// to unique names for the same reason the resolver is.
	byShort := map[string][]*facts.Fact{}
	for name, f := range idx {
		short := name
		if i := strings.LastIndexByte(short, '.'); i >= 0 {
			short = short[i+1:]
		}
		byShort[short] = append(byShort[short], f)
	}

	performsIO := func(f *facts.Fact) bool {
		if f.Props == nil {
			return false
		}
		if v, _ := f.PropAny("io_direct").(bool); v {
			return true
		}
		v, _ := f.PropAny("performs_io").(bool)
		return v
	}

	for changed := true; changed; {
		changed = false
		for i := range all {
			f := &all[i]
			if f.Kind != facts.KindSymbol || performsIO(f) {
				continue
			}
			for _, r := range f.Relations {
				if r.Kind != facts.RelCalls {
					continue
				}
				var target *facts.Fact
				if t, ok := idx[r.Target]; ok {
					target = t
				} else if cands := byShort[r.Target]; len(cands) == 1 {
					target = cands[0]
				}
				if target != nil && performsIO(target) {
					if f.Props == nil {
						f.Props = map[string]any{}
					}
					f.SetProp("performs_io", true)
					changed = true
					break
				}
			}
		}
	}
}
