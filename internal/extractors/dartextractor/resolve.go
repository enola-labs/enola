package dartextractor

import (
	"slices"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// This file holds the passes that run after every file has been walked, because none of
// them can be decided from one file alone.

// resolveCallTargets rewrites bare reference targets to the canonical symbol they name.
//
// The walker preserves receiver paths and substitutes explicitly declared types.
// This pass binds typed members and top-level functions visible in the library.
// Instance methods without receiver evidence remain unresolved, even if their
// short name happens to be unique elsewhere in the repository.
func resolveCallTargets(all []facts.Fact) []facts.Fact {
	disambiguateDartGetters(all)
	all, count := withSDKIndex(all)
	r := newDartMemberResolver(all)
	for i := 0; i < count; i++ {
		f := &all[i]
		r.currentValues, _ = f.Props["_dart_value_bindings"].(map[string]dartLocalValue)
		implicit := dartCallSet(f.Props["_dart_implicit_calls"])
		instances := dartCallSet(f.Props["_dart_instance_calls"])
		constructed := map[string]*facts.Fact{}
		if names, ok := f.Props["_dart_constructed_types"].([]string); ok {
			for _, name := range names {
				if typ := r.typeNamed(name, f.File); typ != nil {
					constructed[name+"()"] = typ
				}
			}
		}
		var resolved []string
		for j, rel := range f.Relations {
			if rel.Kind != facts.RelCalls {
				continue
			}
			if resolved == nil {
				resolved = make([]string, len(f.Relations))
			}
			target := r.call(rel.Target, f, implicit[rel.Target], instances[rel.Target], nil)
			resolved[j] = target
			if ctor := r.members[target].fact; ctor != nil && ctor.PropBool("constructor") {
				if typ := r.lexicalOwner(ctor); typ != nil {
					constructed[rel.Target+"()"] = typ
				}
			}
		}
		var replacements map[string]string
		for j := range f.Relations {
			rel := &f.Relations[j]
			switch rel.Kind {
			case facts.RelImplements, facts.RelInjects, facts.RelInstantiates:
				if typ := r.typeNamed(rel.Target, f.File); typ != nil {
					rel.Target = typ.Name
				} else if len(r.canonicalTypes[rel.Target]) > 0 {
					rel.Target = "(" + rel.Target + ")"
				}
			case facts.RelCalls:
				before := rel.Target
				rel.Target = resolved[j]
				// Only calls on a verified constructor result depend on the completed
				// construction index. Ordinary calls reuse the first pass's result.
				if dot := strings.LastIndexByte(before, '.'); dot >= 0 && constructed[before[:dot]] != nil {
					rel.Target = r.call(before, f, implicit[before], instances[before], constructed)
				}
				if rel.Target != before {
					if replacements == nil {
						replacements = map[string]string{}
					}
					replacements[before] = rel.Target
					recordInheritedCall(r, f, before, rel.Target)
				}
			}
		}
		rewriteDartCallMetrics(f, replacements)
		if reads, ok := f.Props["_dart_getter_reads"].(map[string]dartGetterRead); ok {
			candidates := make([]string, 0, len(reads))
			for candidate := range reads {
				candidates = append(candidates, candidate)
			}
			sort.Strings(candidates)
			for _, candidate := range candidates {
				read := reads[candidate]
				target := r.call(candidate, f, false, true, constructed)
				entry := r.members[target]
				if entry.fact == nil || entry.fact.PropString("accessor") != "getter" {
					continue
				}
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
				if read.Loop {
					calls, _ := f.Props["calls_in_loop"].([]string)
					f.Props["calls_in_loop"] = append(calls, target)
				}
				if read.Scaling {
					calls, _ := f.Props["calls_in_scaling_loop"].([]string)
					f.Props["calls_in_scaling_loop"] = append(calls, target)
				}
			}
		}
		delete(f.Props, "_dart_value_bindings")
		delete(f.Props, "_dart_getter_reads")
		delete(f.Props, "_dart_implicit_calls")
		delete(f.Props, "_dart_instance_calls")
		delete(f.Props, "_dart_constructed_types")
		// Different source spellings can resolve to the same declaration.
		seen := map[facts.Relation]bool{}
		relations := f.Relations[:0]
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelCalls {
				if seen[rel] {
					continue
				}
				seen[rel] = true
			}
			relations = append(relations, rel)
		}
		f.Relations = relations
		for _, prop := range []string{"calls_in_loop", "calls_in_scaling_loop", "io_calls"} {
			if calls, ok := f.Props[prop].([]string); ok {
				sort.Strings(calls)
				f.Props[prop] = slices.Compact(calls)
			}
		}
	}
	return all[:count]
}

func recordInheritedCall(r *dartMemberResolver, f *facts.Fact, spelled, target string) {
	dot := strings.LastIndexByte(spelled, '.')
	tdot := strings.LastIndexByte(target, '.')
	if dot < 0 || tdot < 0 || spelled[dot+1:] != target[tdot+1:] {
		return
	}
	receiver := spelled[:dot]
	var typ *facts.Fact
	if strings.HasPrefix(receiver, "@value:") {
		if vt := r.expressionType(receiver, f, r.currentValues, map[string]bool{}); vt.Name != "" && !vt.Namespace {
			typ = r.valueOwner(vt)
		}
	} else {
		typ = r.typeNamed(strings.TrimSuffix(receiver, "()"), f.File)
	}
	if typ != nil && typ.Name != target[:tdot] {
		f.AddInheritedCalls(typ.Name + spelled[dot:])
	}
}

func dartCallSet(value any) map[string]bool {
	names, _ := value.([]string)
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
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

func seedRetrofitIO(all []facts.Fact) {
	type member struct{ file, name string }
	declared := make(map[member]bool)
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindRoute || f.PropString(facts.PropRole) != facts.RoleClient ||
			f.PropString(facts.PropSource) != facts.RouteSourceDartHTTPClient {
			continue
		}
		if h := f.PropString("handler"); h != "" {
			declared[member{f.File, h}] = true
		}
	}
	if len(declared) == 0 {
		return
	}
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if abstract, _ := f.PropAny("abstract_member").(bool); !abstract {
			continue
		}
		name := f.Name[strings.LastIndexByte(f.Name, '.')+1:]
		if declared[member{f.File, name}] {
			f.SetProp("io_direct", true)
			f.SetProp("performs_io", true)
		}
	}
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
	seedRetrofitIO(all)
	for i := range all {
		if all[i].Kind == facts.KindSymbol && all[i].PropBool("io_direct") {
			all[i].SetProp("performs_io", true)
		}
	}
	idx := map[string]dartMemberEntry{}
	for i := range all {
		if all[i].Kind == facts.KindSymbol {
			idx[all[i].Name] = idx[all[i].Name].add(&all[i])
		}
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
				target := idx[r.Target].readable()

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

// Loop metrics use the same target as the call edge, so downstream analysis does
// not recover a different callee by matching only its method segment.
// Rewrite each original target once. Sequential substitutions both scan every
// metric repeatedly and can incorrectly remap a canonical name a second time.
func rewriteDartCallMetrics(f *facts.Fact, replacements map[string]string) {
	if len(replacements) == 0 {
		return
	}
	if paths, ok := f.Props["call_receivers"].(map[string][]string); ok {
		rewritten := make(map[string][]string, len(paths))
		for target, aliases := range paths {
			if replacement, ok := replacements[target]; ok {
				target = replacement
			}
			rewritten[target] = append(rewritten[target], aliases...)
		}
		for target, aliases := range rewritten {
			sort.Strings(aliases)
			rewritten[target] = slices.Compact(aliases)
		}
		f.Props["call_receivers"] = rewritten
	}
	for _, prop := range []string{"calls_in_loop", "calls_in_scaling_loop", "io_calls"} {
		if calls, ok := f.Props[prop].([]string); ok {
			for i, before := range calls {
				if after, ok := replacements[before]; ok {
					calls[i] = after
				}
			}
		}
	}
}

// Newly represented top-level getters must not make an existing function's
// name ambiguous. Use the getter's file namespace only for these collisions.
func disambiguateDartGetters(all []facts.Fact) {
	files := map[string]string{}
	collisions := map[string]bool{}
	for _, f := range all {
		if f.Kind != facts.KindSymbol {
			continue
		}
		if prior, ok := files[f.Name]; ok && prior != f.File {
			collisions[f.Name] = true
		} else {
			files[f.Name] = f.File
		}
	}
	for i := range all {
		f := &all[i]
		if collisions[f.Name] && f.PropString("symbol_kind") == facts.SymbolFunc && f.PropString("accessor") == "getter" {
			short := f.Name[strings.LastIndexByte(f.Name, '.')+1:]
			f.Name = strings.TrimSuffix(f.File, ".dart") + "." + short
		}
	}
}
