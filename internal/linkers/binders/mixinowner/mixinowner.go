// Package mixinowner gives a class the members it mixed in.
//
// A Ruby `include`, `extend` or `prepend` is extracted as a dependency fact
// whose implements relation names the module, and the module's own methods
// are symbols under the module's name. Nothing connected the two: the
// includer did not own what it included, so every question keyed on
// ownership (impact on a concern method, a class's member set, dead-methods'
// surface) stopped at the module. This binder follows each literal mixin to
// the module fact of the same name and writes a has_method relation from the
// includer to every member the module declares, keeping the member one node;
// the includer records each projected member and the mixin kind it arrived
// through under MembersProp, so a reader can tell a declared member from a
// projected one.
//
// A module name resolves the way Ruby resolves a constant: first inside the
// includer's own namespace, then each enclosing namespace outward, then at
// the top level. A mixin whose constant names no module fact on that walk
// derives nothing and is counted, never guessed; a nested module's members
// belong to the nested module, not to whoever included its parent.
package mixinowner

import (
	"context"
	"log"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// MembersProp is the prop on an includer naming each projected member and the
// mixin kind (include, extend, prepend) that brought it.
const MembersProp = "mixin_members"

const mixinKindProp = "mixin_kind"

type Binder struct{}

func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "mixin-owner" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	modules := moduleFacts(store)
	members := membersByModule(store, modules)

	projected := map[string]map[string]string{}
	var unresolved, mixins int
	for _, dep := range store.ByKind(facts.KindDependency) {
		kind := dep.PropString(mixinKindProp)
		if kind == "" {
			continue
		}
		mixins++
		includer, module := splitMixin(dep.Name)
		if includer == "" || module == "" {
			continue
		}
		module = resolveLexically(includer, module, modules)
		if module == "" {
			unresolved++
			continue
		}
		for _, member := range members[module] {
			if projected[includer] == nil {
				projected[includer] = map[string]string{}
			}
			if _, already := projected[includer][member]; !already {
				projected[includer][member] = kind
			}
		}
	}
	if len(projected) == 0 {
		if mixins > 0 {
			log.Printf("[mixin-owner] %d mixin(s), %d naming no module fact, nothing projected", mixins, unresolved)
		}
		return nil
	}

	relations := 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindSymbol {
			return
		}
		members, ok := projected[f.Name]
		if !ok {
			return
		}
		existing := map[string]bool{}
		for _, r := range f.Relations {
			if r.Kind == facts.RelHasMethod {
				existing[r.Target] = true
			}
		}
		names := make([]string, 0, len(members))
		for name := range members {
			names = append(names, name)
		}
		sort.Strings(names)
		recorded := map[string]any{}
		for _, name := range names {
			recorded[name] = members[name]
			if existing[name] {
				continue
			}
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHasMethod, Target: name})
			relations++
		}
		if f.Props == nil {
			f.Props = map[string]any{}
		}
		f.SetProp(MembersProp, recorded)
	})
	log.Printf("[mixin-owner] %d includer(s) own %d projected member(s) through %d mixin(s); %d mixin(s) name no module fact", len(projected), relations, mixins, unresolved)
	return nil
}

func moduleFacts(store *facts.Store) map[string]bool {
	out := map[string]bool{}
	for _, f := range store.ByKind(facts.KindSymbol) {
		if f.PropString("symbol_kind") == facts.SymbolInterface && f.PropString("language") == "ruby" {
			out[f.Name] = true
		}
	}
	return out
}

func membersByModule(store *facts.Store, modules map[string]bool) map[string][]string {
	out := map[string][]string{}
	for _, f := range store.ByKind(facts.KindSymbol) {
		owner := memberOwner(f.Name)
		if owner == "" || !modules[owner] {
			continue
		}
		out[owner] = append(out[owner], f.Name)
	}
	for owner := range out {
		sort.Strings(out[owner])
	}
	return out
}

func memberOwner(name string) string {
	if hash := strings.LastIndex(name, "#"); hash > 0 {
		return name[:hash]
	}
	if dot := strings.LastIndex(name, "."); dot > 0 && !strings.Contains(name[dot:], "::") {
		return name[:dot]
	}
	return ""
}

func resolveLexically(includer, module string, modules map[string]bool) string {
	if strings.HasPrefix(module, "::") {
		module = strings.TrimPrefix(module, "::")
		if modules[module] {
			return module
		}
		return ""
	}
	scope := includer
	for scope != "" {
		if candidate := scope + "::" + module; modules[candidate] {
			return candidate
		}
		if idx := strings.LastIndex(scope, "::"); idx >= 0 {
			scope = scope[:idx]
		} else {
			scope = ""
		}
	}
	if modules[module] {
		return module
	}
	return ""
}

func splitMixin(name string) (includer, module string) {
	parts := strings.SplitN(name, " -> ", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}
