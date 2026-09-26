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
)

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

	type placed struct{ module, repo string }
	symbolModule := map[string]placed{}
	knownSymbols := map[string]bool{}
	shortNameCount := map[string]int{}
	for _, f := range store.ByKind(facts.KindSymbol) {
		if f.File == "" {
			continue
		}
		knownSymbols[f.Name] = true
		shortNameCount[symbolShortName(f.Name)]++
		if module := resolve(f, production, test); module != "" && production[module] {
			symbolModule[f.Name] = placed{module: module, repo: f.Repo}
		}
	}
	if len(symbolModule) == 0 {
		return nil
	}

	weights := map[edge]int{}
	unresolved, crossRepo := 0, 0
	unresolvedReasons := map[string]int{}
	unresolvedTargets := map[string]map[string]int{}
	for _, f := range store.ByKind(facts.KindSymbol) {
		source, ok := symbolModule[f.Name]
		if !ok {
			continue
		}
		for _, r := range f.Relations {
			if !rolledRelations[r.Kind] {
				continue
			}
			target, known := symbolModule[r.Target]
			if !known {
				unresolved++
				reason := classifyUnresolvedTarget(r.Target, knownSymbols, shortNameCount)
				language := f.PropString("language")
				if language == "" {
					language = "unknown"
				}
				category := language + "/" + r.Kind + "/" + reason
				unresolvedReasons[category]++
				if language == "cpp" && r.Kind == facts.RelCalls {
					if unresolvedTargets[reason] == nil {
						unresolvedTargets[reason] = map[string]int{}
					}
					unresolvedTargets[reason][r.Target]++
				}
				continue
			}
			// A directory name is repo-relative, and 82 of this cluster's names
			// are carried by more than one repository (`.` by nine of them), so
			// a rollup keyed on the name alone merges repositories into one node
			// and manufactures dependencies nobody wrote. A dependency that
			// genuinely crosses a repository is the cross-repo linker's, derived
			// from a measured seam rather than from two directories sharing a
			// name.
			if target.repo != source.repo {
				crossRepo++
				continue
			}
			if target.module == source.module {
				continue
			}
			weights[edge{source: source.module, target: target.module, repo: source.repo}]++
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
		derived = append(derived, facts.Fact{
			Kind: facts.KindDependency,
			Name: fmt.Sprintf("module-edge: %s -> %s", e.source, e.target),
			File: e.source,
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
		for _, reason := range []string{reasonKnownUnplaced, reasonUniqueShort, reasonAmbiguousShort, reasonUnknown} {
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
