// Package moduleedges gives the module layer the edges its symbols imply.
//
// Every module explainer reads one graph, built from dependency facts whose
// imports relations name another module. A language with import statements
// fills that graph from its extractor; Ruby has no such statement, so its
// module facts stood isolated and the readings that walk them (cycles,
// depth, coupling, layers) answered nothing about the language most of this
// estate is written in. The measurement was blunter than the language
// framing: module facts carry no relations in ANY language in the cluster,
// so the reading was blind wherever an extractor did not emit dependency
// facts of its own.
//
// This derives the missing edges from what the graph already resolved. For
// each symbol, every call, dependency, instantiation and injection whose
// target names a symbol the store holds contributes one edge from the module
// declaring the source to the module declaring the target. A target that resolves to
// nothing contributes nothing and is already counted by the extractor's call
// coverage: an unresolved bare name is exactly the case where a rolled-up
// edge would connect two directories on the strength of a shared method
// name.
//
// The edge carries the number of symbol edges behind it, so a reader can
// tell one stray call from a hundred, and its own coupling kind, so the
// readings that already narrow the graph by kind can decide about it. A
// pair an extractor already connected is left alone: this adds the edges
// nobody emitted, it does not restate the ones somebody did.
package moduleedges

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// WeightProp is the number of resolved symbol edges a derived module edge
// stands on.
const WeightProp = "symbol_edges"

// DerivedProp names what the edge was derived from, so a reader can tell it
// from an import an extractor read.
const DerivedProp = "derived"

const derivedFromSymbols = "symbol-rollup"

// rolledRelations are the symbol relations that mean "this code needs that code".
//
// `injects` belongs here for the same reason the others do, and its absence cost
// the most on exactly the codebases this binder was written for. A constructor
// parameter IS how a dependency is declared under a DI container — in Spring, in
// ASP.NET Core, in Angular — and there is frequently no call, instantiation or
// import edge beside it to carry the pair: the container does the constructing, and
// what the file imports is a type name that resolves to an interface or a barrel
// rather than to the module the collaborator lives in. Measured across the corpus,
// rolling injection up adds module pairs nothing else connects: an Angular
// storefront library goes from 3,895 derived edges to 6,128, a Java monolith from
// 1,407 to 1,700, and an ASP.NET Core media server from 68 to 97. Seven extractors
// emit the relation, so this is the general case rather than one language's — the
// oldest fixture in the tree gains the component-to-service and route-to-service
// edges an application of that framework is mostly made of.
//
// It moves values rather than verdicts: on the corpus no explainer's finding COUNT
// changes, while what the findings say does — one storefront's deepest dependency
// chain is 78 rather than 77. That is the expected shape for an edge that was
// always there and simply had no fact.
//
// Nothing else about the derivation changes: an injected type that resolves to no
// known symbol still contributes nothing, a pair an extractor already connected is
// still left alone, and a target in another repository is still the cross-repo
// linker's business.
var rolledRelations = map[string]bool{
	facts.RelCalls:        true,
	facts.RelDependsOn:    true,
	facts.RelInstantiates: true,
	facts.RelInjects:      true,
}

type Binder struct{}

func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "module-edges" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

type edge struct {
	source, target string
	repo           string
}

// unresolvedReason distinguishes resolution work from expected graph boundaries.
// Keep these values stable: they are emitted in coverage diagnostics so runs over
// large repositories can be compared directly.
const (
	reasonKnownUnplaced  = "known_symbol_outside_production_module"
	reasonUniqueShort    = "unique_short_name_match"
	reasonAmbiguousShort = "ambiguous_short_name_match"
	reasonUnknown        = "no_symbol_match"
	// reasonAmbiguousOwner: two production modules of one repository declare the
	// name, so no edge is derived rather than one being picked. It is counted here
	// so a refusal reads differently from an absence.
	reasonAmbiguousOwner = "ambiguous_declaring_module"
)

// namespaceOnlyProp is the Ruby extractor's mark on a module or class whose body
// only nests other declarations.
const namespaceOnlyProp = "namespace_only"

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	// Binders run again on every append, over the whole union, so the edges derived
	// last time go first, as the cross-repo linker drops its own. Left in place, each
	// append added every edge again: one copy per repository appended after it.
	store.RemoveWhere(func(f facts.Fact) bool {
		return f.Kind == facts.KindDependency && f.PropAny(DerivedProp) == derivedFromSymbols
	})
	production, test := moduleNames(store)
	if len(production) == 0 {
		return nil
	}

	// Keyed by (repo, name), never by name alone: 82 of this cluster's symbol
	// names are carried by more than one repository, and a name-keyed map
	// resolves each to whichever repo happened to be read last — so the same
	// union produced different rollups depending on fact order, and a
	// baseline replay (whose re-read member sits at the store's tail) diffed
	// against its own pin. Within one repo a name can still collide across
	// files; the smallest module wins, an order-free tie-break rather than a
	// last-write win. nameRepos keeps the count of repos carrying each name,
	// so a target that resolves only outside its caller's repo is still
	// counted as the cross-repo landing it is rather than as unresolved.
	type key struct{ repo, name string }
	type placed struct{ module string }
	symbolModule := map[key]placed{}
	ambiguous := map[key]bool{}
	loadedFrom := map[key]map[string]bool{}
	namespaceModule := map[key]placed{}
	namespaceAmbiguous := map[key]bool{}
	place := func(into map[key]placed, ambiguousInto map[key]bool, k key, module string) {
		if prior, ok := into[k]; ok {
			if prior.module == module {
				return
			}
			ambiguousInto[k] = true
			if prior.module < module {
				return
			}
		}
		into[k] = placed{module: module}
	}
	nameRepos := map[string]int{}
	seenNameInRepo := map[key]bool{}
	knownSymbols := map[string]bool{}
	shortNameCount := map[string]int{}
	// Repositories whose files carry their label, as an appended repository's do. A
	// module name never carries it, so a symbol whose file sits under
	// "<label>/<module>/" is the evidence; the root module "." shows nothing either way.
	labelled := map[string]bool{}
	for _, f := range store.ByKind(facts.KindSymbol) {
		if f.File == "" {
			continue
		}
		knownSymbols[f.Name] = true
		shortNameCount[symbolShortName(f.Name)]++
		module := resolve(f, production, test)
		if module == "" || !production[module] {
			continue
		}
		if f.Repo != "" && module != "." && strings.HasPrefix(f.File, f.Repo+"/"+module+"/") {
			labelled[f.Repo] = true
		}
		k := key{repo: f.Repo, name: f.Name}
		if !seenNameInRepo[k] {
			seenNameInRepo[k] = true
			nameRepos[f.Name]++
		}
		// A namespace opening (`module Spree` wrapping `class Order`) places
		// nothing: the name belongs to whichever file declares it, and a name that
		// only ever appears as an opening is placed from the openings alone below.
		if f.PropBool(namespaceOnlyProp) {
			place(namespaceModule, namespaceAmbiguous, k, module)
			continue
		}
		place(symbolModule, ambiguous, k, module)
		if zeitwerkPath(f) {
			if loadedFrom[k] == nil {
				loadedFrom[k] = map[string]bool{}
			}
			loadedFrom[k][module] = true
		}
	}
	// Zeitwerk loads a constant from the one file its name spells, so when several
	// modules reopen a name and exactly one of them holds that file, the others
	// are reopenings of it and the reference means the owner. Two modules that
	// each hold such a file (an engine's own app/models/work_package.rb beside the
	// host's) are two owners, and the name stays refused.
	for k := range ambiguous {
		if owners := loadedFrom[k]; len(owners) == 1 {
			for module := range owners {
				symbolModule[k] = placed{module: module}
			}
			delete(ambiguous, k)
		}
	}
	for k, p := range namespaceModule {
		if _, declared := symbolModule[k]; declared {
			continue
		}
		symbolModule[k] = p
		if namespaceAmbiguous[k] {
			ambiguous[k] = true
		}
	}
	if len(symbolModule) == 0 {
		return nil
	}

	weights := map[edge]int{}
	unresolved, crossRepo := 0, 0
	unresolvedReasons := map[string]int{}
	unresolvedTargets := map[string]map[string]int{}
	countUnresolved := func(reason string, f facts.Fact, r facts.Relation) {
		unresolved++
		language := f.PropString("language")
		if language == "" {
			language = "unknown"
		}
		unresolvedReasons[language+"/"+r.Kind+"/"+reason]++
		if language == "cpp" && r.Kind == facts.RelCalls {
			if unresolvedTargets[reason] == nil {
				unresolvedTargets[reason] = map[string]int{}
			}
			unresolvedTargets[reason][r.Target]++
		}
	}
	for _, f := range store.ByKind(facts.KindSymbol) {
		if f.File == "" {
			continue
		}
		// The caller is where its own file puts it. Reading it out of the
		// placement map answered a question about the NAME, and a name two
		// modules declare has a winner: a call made from the losing
		// declaration was credited to the winning module, so the binder
		// derived an edge from a directory that never made the call and
		// omitted the one that did.
		sourceModule := resolve(f, production, test)
		if sourceModule == "" || !production[sourceModule] {
			continue
		}
		for _, r := range f.Relations {
			if !rolledRelations[r.Kind] {
				continue
			}
			// The target resolves in the caller's own repository. A directory
			// name is repo-relative, so a rollup that crossed repositories on
			// the strength of a shared name would merge two graphs into one
			// and manufacture dependencies nobody wrote; a dependency that
			// genuinely crosses a repository is the cross-repo linker's,
			// derived from a measured seam rather than from two directories
			// sharing a name.
			targetKey := key{repo: f.Repo, name: r.Target}
			target, known := symbolModule[targetKey]
			if !known {
				if nameRepos[r.Target] > 0 {
					crossRepo++
					continue
				}
				countUnresolved(classifyUnresolvedTarget(r.Target, knownSymbols, shortNameCount), f, r)
				continue
			}
			// Two production modules of this repository declare the name and
			// nothing in the graph says which one the reference means. A
			// missing edge is honest; a picked one is a dependency nobody
			// wrote, stated confidently enough to breach a law.
			if ambiguous[targetKey] {
				countUnresolved(reasonAmbiguousOwner, f, r)
				continue
			}
			if target.module == sourceModule {
				continue
			}
			weights[edge{source: sourceModule, target: target.module, repo: f.Repo}]++
		}
	}
	if len(weights) == 0 {
		return nil
	}

	existing := extractorEdges(store, production, test)
	derived := make([]facts.Fact, 0, len(weights))
	skipped := 0
	for e, weight := range weights {
		if existing[[2]string{e.source, e.target}] {
			skipped++
			continue
		}
		// The edge sits at its source module's directory, named the way every other
		// fact of that repository names a path. Added after the engine prefixed an
		// appended repository's files, a bare module name here was the one path in
		// the repository without its label.
		file := e.source
		if labelled[e.repo] {
			file = e.repo + "/" + e.source
		}
		derived = append(derived, facts.Fact{
			Kind: facts.KindDependency,
			Name: fmt.Sprintf("module-edge: %s -> %s", e.source, e.target),
			File: file,
			Repo: e.repo,
			Props: map[string]any{
				facts.PropCouplingKind: facts.CouplingSymbolRollup,
				WeightProp:             weight,
				DerivedProp:            derivedFromSymbols,
			},
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: e.target}},
		})
	}
	if len(derived) == 0 {
		return nil
	}
	sort.Slice(derived, func(i, j int) bool { return derived[i].Name < derived[j].Name })
	store.Add(derived...)
	log.Printf("[module-edges] derived %d module edge(s) from resolved symbol edges; %d pair(s) an extractor already connected, %d symbol edge(s) not resolving to a production symbol, %d landing in another repository",
		len(derived), skipped, unresolved, crossRepo)
	if unresolved > 0 {
		log.Printf("[module-edges] unresolved symbol edge breakdown: %s", formatBreakdown(unresolvedReasons))
		for _, reason := range []string{reasonKnownUnplaced, reasonUniqueShort, reasonAmbiguousShort, reasonUnknown, reasonAmbiguousOwner} {
			if targets := unresolvedTargets[reason]; len(targets) > 0 {
				log.Printf("[module-edges] top unresolved cpp/calls/%s targets: %s", reason, formatTopTargets(targets, 5))
			}
		}
	}
	return nil
}

func formatTopTargets(counts map[string]int, limit int) string {
	type item struct {
		name  string
		count int
	}
	items := make([]item, 0, len(counts))
	for name, count := range counts {
		items = append(items, item{name: name, count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		return items[i].name < items[j].name
	})
	if len(items) > limit {
		items = items[:limit]
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%s=%d", item.name, item.count))
	}
	return strings.Join(parts, ", ")
}

func symbolShortName(name string) string {
	for _, sep := range []string{"#", "::", "."} {
		if i := strings.LastIndex(name, sep); i >= 0 {
			name = name[i+len(sep):]
		}
	}
	return name
}

func classifyUnresolvedTarget(target string, known map[string]bool, shortNames map[string]int) string {
	if known[target] {
		return reasonKnownUnplaced
	}
	switch shortNames[symbolShortName(target)] {
	case 1:
		return reasonUniqueShort

	case 0:
		return reasonUnknown
	default:
		return reasonAmbiguousShort
	}
}

func formatBreakdown(counts map[string]int) string {
	type item struct {
		name  string
		count int
	}
	items := make([]item, 0, len(counts))
	for name, count := range counts {
		items = append(items, item{name: name, count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		return items[i].name < items[j].name
	})
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%s=%d", item.name, item.count))
	}
	return strings.Join(parts, ", ")
}

func moduleNames(store *facts.Store) (production, test map[string]bool) {
	production, test = map[string]bool{}, map[string]bool{}
	for _, m := range store.ByKind(facts.KindModule) {
		if common.IsTestModule(m) {
			test[m.Name] = true
			continue
		}
		production[m.Name] = true
	}
	return production, test
}

// resolve names the module a fact's file belongs to, through the same
// candidate walk the module graph builder uses, so an edge this binder
// derives and an edge the builder attributes cannot land on different nodes.
func resolve(f facts.Fact, production, test map[string]bool) string {
	for _, candidate := range common.ModuleDirCandidates(f) {
		if production[candidate] || test[candidate] {
			return candidate
		}
	}
	for _, candidate := range common.ModuleDirCandidates(f) {
		for cur := candidate; ; {
			if production[cur] || test[cur] {
				return cur
			}
			i := strings.LastIndex(cur, "/")
			if i < 0 {
				break
			}
			cur = cur[:i]
		}
	}
	return ""
}

// extractorEdges is the set of module pairs some extractor already connected,
// so a derived edge never restates one that was read from source.
func extractorEdges(store *facts.Store, production, test map[string]bool) map[[2]string]bool {
	out := map[[2]string]bool{}
	for _, dep := range store.ByKind(facts.KindDependency) {
		source := resolve(dep, production, test)
		if source == "" {
			continue
		}
		for _, r := range dep.Relations {
			if r.Kind == facts.RelImports {
				out[[2]string{source, r.Target}] = true
			}
		}
	}
	return out
}

// zeitwerkPath reports a Ruby declaration of a constant sitting in the file
// Zeitwerk would load for that name, `spree.rb` for Spree and `active_support/
// cache.rb` for ActiveSupport::Cache. A name whose inflection the loader
// overrides spells no such file here, and so is never tied-broken: that errs
// toward refusing.
func zeitwerkPath(f facts.Fact) bool {
	if f.PropString("language") != "ruby" || !strings.HasSuffix(f.File, ".rb") {
		return false
	}
	parts := strings.Split(f.Name, "::")
	for _, part := range parts {
		if part == "" || part[0] < 'A' || part[0] > 'Z' {
			return false
		}
		for _, c := range part {
			if c != '_' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
				return false
			}
		}
	}
	for i, part := range parts {
		parts[i] = underscore(part)
	}
	want := strings.Join(parts, "/") + ".rb"
	return f.File == want || strings.HasSuffix(f.File, "/"+want)
}

// underscore is ActiveSupport's String#underscore for one constant segment.
func underscore(name string) string {
	var out []rune
	runes := []rune(name)
	for i, r := range runes {
		upper := r >= 'A' && r <= 'Z'
		if upper && i > 0 {
			prev := runes[i-1]
			prevLowerOrDigit := prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9'
			prevUpper := prev >= 'A' && prev <= 'Z'
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			if prevLowerOrDigit || prevUpper && nextLower {
				out = append(out, '_')
			}
		}
		if upper {
			r += 'a' - 'A'
		}
		out = append(out, r)
	}
	return string(out)
}
