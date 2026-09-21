// Package emberresolver binds Ember's convention-named references — .hbs
// template invocations and @service injections — to the symbols that declare
// them.
package emberresolver

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/extcoverage"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Binder resolves Ember resolver-convention names after extraction, where the
// whole store is visible.
//
// WHY A BINDER. A classic Handlebars template invokes `<Core::Modal />` or
// `{{format-date …}}` with no import to anchor on: the name maps to a file by
// Ember's resolver layout (components/core/modal.*, helpers/format-date.*), and
// the symbol declared IN that file is only known once every file has been
// extracted. The same is true of `@service store` — the target is whatever class
// app/services/store.ts actually declares, which need not be named `Store`. The
// per-file extractor records the names; this binder joins them.
//
// WHY IT SKIPS ON AMBIGUITY. Resolution requires exactly one candidate file and
// exactly one plausible symbol in it. Two matches mean the convention alone
// cannot decide — grpcimpl's precedent is to skip rather than guess, and a wrong
// edge here would feed impact_analysis. Misses are recorded on the template's
// file_ref carrier (ember_unresolved), so "not resolved" never reads as "no
// consumers".
//
// It is post-link: all references are within one repo, like http-handler.
type Binder struct{}

// New returns the binder.
func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "ember-resolver" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

// templateProp/invocationsProp/ownerFileProp/servicesProp mirror the constants
// the TypeScript extractor's Ember pass emits. Spelled here rather than imported
// so this package — like every binder — depends only on facts and plugin.
const (
	templateProp    = "ember_template"
	invocationsProp = "ember_invocations"
	// invokedAsProp carries the names a template actually writes for a symbol.
	// A template writes `<Core::Badge />`; the symbol is
	// `ember_app/app/components/hiring-plan.Badge`. The mapping between the two
	// is computed here already — it is how the component edges get built — and
	// was thrown away after use, so the graph knew the answer and could not be
	// asked for it by the name a person would type.
	invokedAsProp      = "ember_invoked_as"
	ownerFileProp      = "ember_owner_file"
	servicesProp       = "ember_injected_services"
	relationshipsProp  = "ember_relationships"
	defaultExportProp  = "ember_default_export"
	routeLinksProp     = "ember_route_links"
	routeNameProp      = "ember_route_name"
	navLinksProp       = "nav_route_links"
	navNameProp        = "nav_route_name"
	dataRoleProp       = "ember_data_role"
	yieldHashProp      = "ember_yield_hash"
	contextualProp     = "ember_contextual"
	attrTransformsProp = "ember_attr_transforms"
	unresolvedProp     = "ember_unresolved"
)

var sourceExts = []string{".ts", ".js", ".gts", ".gjs", ".hbs"}

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	var resolvedBindings, unresolvedBindings, templatesWithMisses int
	idx := buildIndex(store)
	bound := 0

	type templateWork struct {
		ownerSymbol string
		targets     []string
		linkTargets []string
		unresolved  []string
	}
	work := map[string]*templateWork{}
	invokedAs := newInvocationNames()

	// The router map gives every route a dot-name; a route's class file follows
	// it (catalog.book -> app/routes/catalog/book.*), and templates link to it
	// by exactly that name. Index both directions per repo.
	routeByName := map[string]map[string]string{}
	routeHandlers := map[string]string{}
	for _, r := range store.ByKind(facts.KindRoute) {
		name := r.PropString(routeNameProp)
		if name == "" {
			name = r.PropString(navNameProp)
		}
		if name == "" {
			continue
		}
		if routeByName[r.Repo] == nil {
			routeByName[r.Repo] = map[string]string{}
		}
		if _, taken := routeByName[r.Repo][name]; !taken {
			routeByName[r.Repo][name] = r.Name
		}
		fragment := "routes/" + strings.ReplaceAll(name, ".", "/")
		anchorHint := ""
		if engine := r.PropString("ember_engine"); engine != "" && r.PropString("router") == "engine" {
			anchorHint = "lib/" + engine + "/addon/_"
		}
		if f := idx.uniqueFile(r.Repo, fragment, anchorHint); f != "" {
			if sym := idx.primarySymbolIn(r.Repo, f); sym != "" {
				routeHandlers[r.Repo+"\x00"+r.Name+"\x00"+name] = sym
			}
		}
	}

	// A link may name the implicit `.index` child Ember creates for every
	// nesting level; it renders at the parent's own path, so the lookup strips
	// the suffix rather than requiring a declared index route.
	routeLinks := func(f *facts.Fact) []string {
		names := append(propStrings(f.PropAny(routeLinksProp)), propStrings(f.PropAny(navLinksProp))...)
		if len(names) == 0 {
			return nil
		}
		var targets []string
		for _, name := range names {
			path, ok := routeByName[f.Repo][name]
			if !ok {
				if base, found := strings.CutSuffix(name, ".index"); found {
					path, ok = routeByName[f.Repo][base]
				}
			}
			if ok {
				targets = append(targets, path)
			}
		}
		sort.Strings(targets)
		return targets
	}

	// First pass: owners, and carrier-side yield hashes registered onto them —
	// a classic component's yield map lives in its .hbs, and a consumer in a
	// different template must see it regardless of iteration order.
	for _, f := range store.ByKind(facts.KindFileRef) {
		if !f.PropBool(templateProp) {
			continue
		}
		w := &templateWork{}
		if owner := f.PropString(ownerFileProp); owner != "" {
			w.ownerSymbol = idx.primarySymbolIn(f.Repo, owner)
		} else if sym := idx.primarySymbolIn(f.Repo, f.Name); sym != "" {
			w.ownerSymbol = sym
		} else {
			w.ownerSymbol = idx.resolveRouteOwner(f.Repo, f.Name)
		}
		if w.ownerSymbol != "" {
			if entries := propStrings(f.PropAny(yieldHashProp)); len(entries) > 0 {
				idx.addYieldHash(f.Repo, w.ownerSymbol, entries)
			}
		}
		work[f.Name] = w
	}
	for _, f := range store.ByKind(facts.KindFileRef) {
		if !f.PropBool(templateProp) {
			continue
		}
		w := work[f.Name]
		for _, name := range propStrings(f.PropAny(invocationsProp)) {
			if target := idx.resolveInvocation(f.Repo, name, f.Name); target != "" {
				w.targets = append(w.targets, target)
				invokedAs.record(target, name)
			} else {
				w.unresolved = append(w.unresolved, name)
			}
		}
		for _, pair := range propStrings(f.PropAny(contextualProp)) {
			if target := idx.resolveContextual(f.Repo, pair, f.Name); target != "" {
				w.targets = append(w.targets, target)
			} else {
				w.unresolved = append(w.unresolved, pair)
			}
		}
		w.linkTargets = routeLinks(&f)
	}

	injections := map[string][]string{}
	dataRoleTargets := map[string]string{}
	templateOwners := map[string][]string{}
	for _, s := range store.ByKind(facts.KindSymbol) {
		if names := propStrings(s.PropAny(servicesProp)); len(names) > 0 {
			var targets []string
			for _, name := range names {
				if target := idx.resolveService(s.Repo, name); target != "" {
					targets = append(targets, target)
				}
			}
			if len(targets) > 0 {
				sort.Strings(targets)
				injections[s.Name] = targets
			}
		}
		// An adapter/serializer/transform serves the model its file base names;
		// the reserved `application` base is the app-wide fallback and names none.
		if s.PropString(dataRoleProp) != "" {
			base := strings.TrimSuffix(filepath.Base(s.File), filepath.Ext(s.File))
			if base != "application" {
				if target := idx.resolveModel(s.Repo, base); target != "" {
					dataRoleTargets[s.Name] = target
				}
			}
		}
		// A template-tag route template (app/templates/<path>.gjs) synthesizes its
		// component; the route class that renders it owns an edge to it, exactly as
		// an .hbs route template's edges land on its route class.
		if s.PropString("web_component") == "component" && s.PropString("framework") == "ember" {
			file := filepath.ToSlash(s.File)
			if strings.HasSuffix(file, ".gjs") || strings.HasSuffix(file, ".gts") {
				if owner := idx.resolveRouteOwner(s.Repo, file); owner != "" && owner != s.Name {
					templateOwners[owner] = append(templateOwners[owner], s.Name)
				}
			}
		}
	}
	for owner := range templateOwners {
		sort.Strings(templateOwners[owner])
	}

	ownerTargets := map[string][]string{}
	for _, w := range work {
		if w.ownerSymbol == "" {
			continue
		}
		merged := ownerTargets[w.ownerSymbol]
		merged = append(merged, w.targets...)
		merged = append(merged, w.linkTargets...)
		ownerTargets[w.ownerSymbol] = merged
	}
	for owner := range ownerTargets {
		sort.Strings(ownerTargets[owner])
	}

	relationships := map[string][]string{}
	for _, s := range store.ByKind(facts.KindStorage) {
		entries := propStrings(s.PropAny(relationshipsProp))
		if len(entries) == 0 {
			continue
		}
		var targets []string
		for _, entry := range entries {
			_, name, ok := strings.Cut(entry, ":")
			if !ok {
				continue
			}
			if target := idx.resolveModel(s.Repo, name); target != "" {
				targets = append(targets, target)
			}
		}
		if len(targets) > 0 {
			sort.Strings(targets)
			relationships[s.Name] = targets
		}
	}

	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind == facts.KindRoute {
			if sym, ok := routeHandlers[f.Repo+"\x00"+f.Name+"\x00"+f.PropString(routeNameProp)]; ok {
				if !f.HasRelation(facts.RelHandledBy, sym) {
					f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: sym})
					bound++
				}
			}
			return
		}
		if f.Kind == facts.KindFileRef {
			if w, ok := work[f.Name]; ok && f.PropBool(templateProp) {
				if w.ownerSymbol == "" {
					for _, t := range append(append([]string{}, w.targets...), w.linkTargets...) {
						if !f.HasRelation(facts.RelCalls, t) {
							f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
							bound++
						}
					}
				}
				resolvedBindings += len(w.targets) + len(w.linkTargets)
				if len(w.unresolved) > 0 {
					sort.Strings(w.unresolved)
					f.SetProp(unresolvedProp, w.unresolved)
					unresolvedBindings += len(w.unresolved)
					templatesWithMisses++
				}
			}
			return
		}
		if f.Kind == facts.KindStorage {
			if targets, ok := relationships[f.Name]; ok {
				for _, t := range targets {
					if t != f.Name && !f.HasRelation(facts.RelDependsOn, t) {
						f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: t})
						bound++
					}
				}
			}
			for _, tname := range propStrings(f.PropAny(attrTransformsProp)) {
				if t := idx.resolveTransform(f.Repo, tname); t != "" && !f.HasRelation(facts.RelDependsOn, t) {
					f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: t})
					bound++
				}
			}
			return
		}
		if f.Kind != facts.KindSymbol {
			return
		}
		if names := invokedAs.namesFor(f.Name); len(names) > 0 {
			f.SetProp(invokedAsProp, names)
		}
		if targets, ok := injections[f.Name]; ok {
			for _, t := range targets {
				if t != f.Name && !f.HasRelation(facts.RelInjects, t) {
					f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelInjects, Target: t})
					bound++
				}
			}
		}
		if t, ok := dataRoleTargets[f.Name]; ok {
			if !f.HasRelation(facts.RelDependsOn, t) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: t})
				bound++
			}
		}
		if comps, ok := templateOwners[f.Name]; ok {
			for _, t := range comps {
				if !f.HasRelation(facts.RelCalls, t) {
					f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
					bound++
				}
			}
		}
		for _, t := range routeLinks(f) {
			if !f.HasRelation(facts.RelCalls, t) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
				bound++
			}
		}
		for _, name := range propStrings(f.PropAny(invocationsProp)) {
			if t := idx.resolveInvocation(f.Repo, name, f.File); t != "" && t != f.Name {
				invokedAs.record(t, name)
			}
			if t := idx.resolveInvocation(f.Repo, name, f.File); t != "" && t != f.Name && !f.HasRelation(facts.RelCalls, t) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
				bound++
			}
		}
		for _, pair := range propStrings(f.PropAny(contextualProp)) {
			if t := idx.resolveContextual(f.Repo, pair, f.File); t != "" && t != f.Name && !f.HasRelation(facts.RelCalls, t) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
				bound++
			}
		}
		for _, t := range ownerTargets[f.Name] {
			if t != f.Name && !f.HasRelation(facts.RelCalls, t) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
				bound++
			}
		}
	})

	if bound > 0 {
		log.Printf("[binder:ember-resolver] bound %d Ember reference(s)", bound)
	}

	// Report the misses in aggregate, not only as a prop on each template.
	// `ember_unresolved` has existed since this binder was written and read 0
	// over 1,363 unexamined templates, because nothing aggregated it — the
	// original evidence for building a coverage mechanism at all. Nineteen
	// extractors later it had exactly one adopter, and this is the second.
	if fact, ok := extcoverage.Fact(repoRoot(store), "ember:templates", "ember_binding",
		resolvedBindings, map[string]int{"unresolved_binding": unresolvedBindings}); ok {
		if templatesWithMisses > 0 {
			fact.SetProp("templates_with_misses", templatesWithMisses)
		}
		store.Add(fact)
	}
	return nil
}

// repoRoot names the repository this binder ran over, for the coverage fact's
// file field. A multi-repo store reports the first label, which is the same
// convention the cross-repo coverage already uses.
func repoRoot(store *facts.Store) string {
	if labels := store.RepoLabels(); len(labels) > 0 {
		return labels[0]
	}
	return "."
}

// index holds, per repo, the file paths that can back a resolver name and the
// symbols each file declares.
type index struct {
	files       map[string]map[string]bool
	byBase      map[string]map[string][]string
	fileSymbols map[string]map[string][]symbolInfo
	fileStorage map[string]map[string][]string
	reexports   map[string]map[string]string
	yieldHashes map[string]map[string]map[string]string
}

type symbolInfo struct {
	name          string
	exported      bool
	component     bool
	class         bool
	receiver      bool
	defaultExport bool
}

func buildIndex(store *facts.Store) *index {
	idx := &index{
		files:       map[string]map[string]bool{},
		byBase:      map[string]map[string][]string{},
		fileSymbols: map[string]map[string][]symbolInfo{},
		fileStorage: map[string]map[string][]string{},
		reexports:   map[string]map[string]string{},
		yieldHashes: map[string]map[string]map[string]string{},
	}
	addFile := func(repo, file string) {
		if file == "" {
			return
		}
		slashed := filepath.ToSlash(file)
		if idx.files[repo] == nil {
			idx.files[repo] = map[string]bool{}
			idx.byBase[repo] = map[string][]string{}
		}
		if !idx.files[repo][slashed] {
			idx.files[repo][slashed] = true
			base := slashed[strings.LastIndexByte(slashed, '/')+1:]
			idx.byBase[repo][base] = append(idx.byBase[repo][base], slashed)
		}
	}
	for _, s := range store.ByKind(facts.KindSymbol) {
		addFile(s.Repo, s.File)
		if idx.fileSymbols[s.Repo] == nil {
			idx.fileSymbols[s.Repo] = map[string][]symbolInfo{}
		}
		file := filepath.ToSlash(s.File)
		idx.fileSymbols[s.Repo][file] = append(idx.fileSymbols[s.Repo][file], symbolInfo{
			name:          s.Name,
			exported:      s.PropBool("exported"),
			component:     s.PropString("web_component") == "component",
			class:         s.PropString("symbol_kind") == facts.SymbolClass,
			receiver:      s.PropString("receiver") != "",
			defaultExport: s.PropBool(defaultExportProp),
		})
	}
	for _, f := range store.ByKind(facts.KindFileRef) {
		if f.PropBool(templateProp) {
			addFile(f.Repo, f.File)
		}
	}
	// A v1 addon publishes through bare re-export stubs that declare no
	// symbols; the extractor records each as a reexport-flagged dependency
	// whose imports relation carries the resolved target. Indexing those files
	// and their targets is what lets the primary-symbol pick chase the chain.
	for _, d := range store.ByKind(facts.KindDependency) {
		if !d.PropBool("reexport") || d.File == "" {
			continue
		}
		target := ""
		for _, r := range d.Relations {
			if r.Kind == facts.RelImports {
				target = r.Target
			}
		}
		if target == "" || strings.Contains(target, "://") {
			continue
		}
		stub := filepath.ToSlash(d.File)
		target = filepath.ToSlash(target)
		// Inside lib/<addon>/, the addon's OWN name is a module prefix for its
		// addon/ tree — `badge-kit/components/x` from within lib/badge-kit/ is
		// lib/badge-kit/addon/components/x. That is the v1 addon module-naming
		// rule, not a guess.
		if libIdx := strings.Index(stub, "lib/"); libIdx >= 0 {
			rest := stub[libIdx+4:]
			if slash := strings.IndexByte(rest, '/'); slash > 0 {
				addonName := rest[:slash]
				if cut, ok := strings.CutPrefix(target, addonName+"/"); ok {
					target = stub[:libIdx] + "lib/" + addonName + "/addon/" + cut
				}
			}
		}
		addFile(d.Repo, d.File)
		if idx.reexports[d.Repo] == nil {
			idx.reexports[d.Repo] = map[string]string{}
		}
		idx.reexports[d.Repo][stub] = target
	}
	for _, sym := range store.ByKind(facts.KindSymbol) {
		if entries := propStrings(sym.PropAny(yieldHashProp)); len(entries) > 0 {
			idx.addYieldHash(sym.Repo, sym.Name, entries)
		}
	}
	for _, s := range store.ByKind(facts.KindStorage) {
		if s.PropString("framework") != "ember-data" {
			continue
		}
		addFile(s.Repo, s.File)
		if idx.fileStorage[s.Repo] == nil {
			idx.fileStorage[s.Repo] = map[string][]string{}
		}
		file := filepath.ToSlash(s.File)
		idx.fileStorage[s.Repo][file] = append(idx.fileStorage[s.Repo][file], s.Name)
	}
	for repo := range idx.fileStorage {
		for file := range idx.fileStorage[repo] {
			sort.Strings(idx.fileStorage[repo][file])
		}
	}
	return idx
}

// resolveInvocation maps one template invocation name to a symbol: the name
// dasherizes to a resolver path fragment, the fragment must match exactly one
// file (the invoking template itself excluded), and that file must yield exactly
// one primary symbol.
func (idx *index) resolveInvocation(repo, name, selfFile string) string {
	var fragments []string
	if kind, lit, ok := strings.Cut(name, ":"); ok && (kind == "component" || kind == "helper" || kind == "modifier") {
		fragments = []string{kind + "s/" + lit}
		if kind == "component" {
			fragments = append(fragments, "pods/"+lit+"/component")
		}
	} else if name[0] >= 'A' && name[0] <= 'Z' {
		segs := strings.Split(name, "::")
		for i, s := range segs {
			segs[i] = dasherize(s)
		}
		joined := strings.Join(segs, "/")
		fragments = []string{"components/" + joined, "pods/" + joined + "/component"}
	} else {
		fragments = []string{"components/" + name, "helpers/" + name, "modifiers/" + name,
			"pods/" + name + "/component"}
	}
	for _, frag := range fragments {
		if file := idx.uniqueFile(repo, frag, selfFile); file != "" {
			if sym := idx.primarySymbolIn(repo, file); sym != "" {
				return sym
			}
		}
	}
	return ""
}

// engineTreeOf returns the "lib/<engine>/addon" prefix owning file, or "".
// An engine's templates resolve against its own isolated tree, never the
// host's — sharing is explicit in Ember, and a cross-tree match would be a
// wrong edge.
func engineTreeOf(file string) string {
	parts := strings.Split(filepath.ToSlash(file), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "lib" && parts[i+2] == "addon" {
			return strings.Join(parts[:i+3], "/")
		}
	}
	return ""
}

func (idx *index) resolveService(repo, name string) string {
	for _, frag := range []string{"services/" + name, "pods/" + name + "/service"} {
		if file := idx.uniqueFile(repo, frag, ""); file != "" {
			if sym := idx.primarySymbolIn(repo, file); sym != "" {
				return sym
			}
		}
	}
	return ""
}

// resolveRouteOwner maps a route template — app/templates/<path>.hbs, outside
// the components tree — to the route class (or, failing that, the controller)
// that renders it, per the router's naming convention.
func (idx *index) resolveRouteOwner(repo, templateFile string) string {
	file := filepath.ToSlash(templateFile)
	marker := "app/templates/"
	pos := strings.Index(file, marker)
	if pos < 0 || (pos > 0 && file[pos-1] != '/') {
		return ""
	}
	fragment := file[pos+len(marker):]
	if ext := filepath.Ext(fragment); ext != "" {
		fragment = strings.TrimSuffix(fragment, ext)
	}
	if fragment == "" || strings.HasPrefix(fragment, "components/") {
		return ""
	}
	fragments := []string{fragment}
	// Substate templates (foo-loading, foo-error) belong to foo's route; an
	// index template belongs to the implicit index route, falling back to the
	// parent when no index class exists.
	for _, suffix := range []string{"-loading", "-error"} {
		if base, found := strings.CutSuffix(fragment, suffix); found && base != "" {
			fragments = append(fragments, base)
		}
	}
	if base, found := strings.CutSuffix(fragment, "/index"); found && base != "" {
		fragments = append(fragments, base)
	}
	for _, frag := range fragments {
		for _, owner := range []string{"routes/" + frag, "controllers/" + frag,
			"pods/" + frag + "/route", "pods/" + frag + "/controller"} {
			if f := idx.uniqueFile(repo, owner, templateFile); f != "" {
				if sym := idx.primarySymbolIn(repo, f); sym != "" {
					return sym
				}
			}
		}
	}
	return ""
}

func (idx *index) addYieldHash(repo, symbolName string, entries []string) {
	if idx.yieldHashes[repo] == nil {
		idx.yieldHashes[repo] = map[string]map[string]string{}
	}
	m := map[string]string{}
	for _, e := range entries {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	idx.yieldHashes[repo][symbolName] = m
}

// resolveContextual maps a "Comp#Key" consumption pair to the component the
// yielding template bound under Key. Comp is either an already-resolved symbol
// name (strict-mode consumer) or a bare invocation name (.hbs consumer).
func (idx *index) resolveContextual(repo, pair, selfFile string) string {
	comp, key, ok := strings.Cut(pair, "#")
	if !ok {
		return ""
	}
	compSym := comp
	if !strings.ContainsAny(comp, "./") {
		compSym = idx.resolveInvocation(repo, comp, selfFile)
	}
	if compSym == "" {
		return ""
	}
	name := idx.yieldHashes[repo][compSym][key]
	if name == "" {
		return ""
	}
	if direct, ok := strings.CutPrefix(name, "@"); ok {
		return direct
	}
	return idx.resolveInvocation(repo, "component:"+name, selfFile)
}

func (idx *index) resolveTransform(repo, name string) string {
	for _, frag := range []string{"transforms/" + name, "pods/" + name + "/transform"} {
		if file := idx.uniqueFile(repo, frag, ""); file != "" {
			if sym := idx.primarySymbolIn(repo, file); sym != "" {
				return sym
			}
		}
	}
	return ""
}

// resolveModel maps an ember-data relationship's model name to the storage fact
// its app/models/<name> file declares.
func (idx *index) resolveModel(repo, name string) string {
	if file := idx.uniqueFile(repo, "models/"+name, ""); file != "" {
		if names := idx.fileStorage[repo][filepath.ToSlash(file)]; len(names) == 1 {
			return names[0]
		}
	}
	return ""
}

// uniqueFile returns the single file backing "app/<fragment><ext>" (or the
// folder-index form) across the repo's files, or "" when zero or several match.
// Anchoring on the `app/` tree is Ember's own resolver rule — a lookalike path
// elsewhere (a style-guide template under app/templates/…/components/, an
// in-repo addon) is not resolvable by name and must not shadow the real one.
// Several matches can still occur in a monorepo holding more than one Ember
// app; that is a genuine ambiguity and skips.
// Extensions are tried in priority order and the first that matches wins
// outright: a classic component is a co-located pair (field.ts + field.hbs) that
// backs ONE resolver name, so the class file must not be read as ambiguous with
// its own template. Two matches at the SAME extension — two Ember apps in one
// monorepo — are a genuine ambiguity and skip.
// The basename index narrows each lookup to the handful of same-named files
// before the anchored-suffix check — the linear scan this replaces was
// quadratic at monolith scale.
func (idx *index) uniqueFile(repo, fragment, selfFile string) string {
	anchorRoot := "app/"
	if tree := engineTreeOf(selfFile); tree != "" {
		anchorRoot = tree + "/"
	}
	lastSeg := fragment[strings.LastIndexByte(fragment, '/')+1:]
	for _, ext := range sourceExts {
		for _, form := range []struct{ base, anchored string }{
			{lastSeg + ext, anchorRoot + fragment + ext},
			{"index" + ext, anchorRoot + fragment + "/index" + ext},
		} {
			found := ""
			ambiguous := false
			for _, file := range idx.byBase[repo][form.base] {
				if file == selfFile {
					continue
				}
				if file != form.anchored && !strings.HasSuffix(file, "/"+form.anchored) {
					continue
				}
				if found != "" && found != file {
					ambiguous = true
				}
				found = file
			}
			if ambiguous {
				return ""
			}
			if found != "" {
				return found
			}
		}
	}
	return ""
}

// primarySymbolIn picks the one symbol a resolver name means in a file: the
// default export when the extractor marked one (Ember resolution IS
// default-export resolution — a module exporting a Base class beside its
// default component is not ambiguous), else the single exported component,
// else the single exported top-level class, else the single exported top-level
// symbol. Anything still plural is ambiguous and skipped.
func (idx *index) primarySymbolIn(repo, file string) string {
	return idx.primarySymbolChase(repo, file, 0)
}

// primarySymbolChase follows a declaration-less re-export stub to the file it
// republishes, bounded so a cycle terminates. Depth 3 covers addon app-tree
// stubs and one barrel hop beyond them.
func (idx *index) primarySymbolChase(repo, file string, depth int) string {
	slashed := filepath.ToSlash(file)
	if len(idx.fileSymbols[repo][slashed]) == 0 {
		if target, ok := idx.reexports[repo][slashed]; ok && depth < 3 {
			for _, ext := range sourceExts {
				for _, candidate := range []string{target + ext, target + "/index" + ext} {
					if idx.files[repo][candidate] {
						if sym := idx.primarySymbolChase(repo, candidate, depth+1); sym != "" {
							return sym
						}
					}
				}
			}
		}
		return ""
	}
	syms := idx.fileSymbols[repo][slashed]
	pick := func(match func(symbolInfo) bool) (string, int) {
		name, n := "", 0
		for _, s := range syms {
			if s.receiver || !match(s) {
				continue
			}
			if name != s.name {
				n++
				name = s.name
			}
		}
		return name, n
	}
	if name, n := pick(func(s symbolInfo) bool { return s.defaultExport }); n == 1 {
		return name
	}
	if name, n := pick(func(s symbolInfo) bool { return s.exported && s.component }); n == 1 {
		return name
	}
	if name, n := pick(func(s symbolInfo) bool { return s.exported && s.class }); n == 1 {
		return name
	}
	if name, n := pick(func(s symbolInfo) bool { return s.exported }); n == 1 {
		return name
	}
	return ""
}

func propStrings(v any) []string {
	switch vv := v.(type) {
	case []string:
		return vv
	case []any:
		out := make([]string, 0, len(vv))
		for _, x := range vv {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func dasherize(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// invocationNames collects the template-side names each symbol answers to, so
// the binder that already resolves them is the one place that writes them down.
// Resolving the same convention a second time in the query path would give two
// components an opinion about one name, and when they disagree the graph
// answers differently depending on which door the question came in.
type invocationNames struct {
	byTarget map[string]map[string]bool
}

func newInvocationNames() *invocationNames {
	return &invocationNames{byTarget: map[string]map[string]bool{}}
}

func (n *invocationNames) record(target, name string) {
	name = strings.TrimPrefix(name, "component:")
	if target == "" || name == "" {
		return
	}
	if n.byTarget[target] == nil {
		n.byTarget[target] = map[string]bool{}
	}
	n.byTarget[target][name] = true
}

func (n *invocationNames) namesFor(target string) []string {
	set := n.byTarget[target]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
