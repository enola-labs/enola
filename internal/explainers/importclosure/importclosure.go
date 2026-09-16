package importclosure

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

// minDominated is how many modules a package __init__.py must be solely responsible
// for before it is worth reporting.
//
// Depth was tried first and does not discriminate: on a real package 31 of 36
// third-party arrivals sit three or more hops in, and most are core dependencies that
// legitimately load. What separates a barrel worth splitting from ordinary layering is
// not how far away it is but how much it ALONE brings — the modules that become
// unreachable if the entry point stops going through it.
const minDominated = 10

// maxReported caps the barrels reported per entry point, largest first.
const maxReported = 5

// Explainer reports what importing a Python package actually executes.
//
// The question it answers is not one the coupling explainers can: they measure which
// packages depend on which, whereas this measures what a single import STATEMENT
// costs — the modules Python runs before it returns, including the package
// __init__.py files no import statement names. A barrel that re-exports for
// convenience is invisible to every file-by-file tool, because each import in the
// chain is individually reasonable; only the closure shows that importing the
// package's public API loads a database driver.
type Explainer struct{}

func New() *Explainer { return &Explainer{} }

func (e *Explainer) Name() string { return "import-closure" }

func (e *Explainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	g := Build(store)
	if len(g.Edges) == 0 {
		return nil, nil
	}
	externals := externalImports(store)

	var out []facts.Insight
	for _, entry := range g.EntryPoints() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		closure := g.Closure(entry)
		if len(closure) < 2 {
			continue // a package that loads nothing but itself says nothing
		}
		pkg := fileDir(entry)
		total := g.packageFileCount(pkg)

		// Which third-party packages the closure reaches, and how far in.
		arrival := map[string]int{}
		for file, depth := range closure {
			for _, ext := range externals[file] {
				if d, ok := arrival[ext]; !ok || depth+1 < d {
					arrival[ext] = depth + 1
				}
			}
		}

		out = append(out, e.summary(entry, pkg, closure, total, arrival))
		out = append(out, e.dominatingBarrels(g, entry, pkg, closure, externals)...)
	}
	// Ranked before capping, and by CONFIDENCE rather than by title: a package whose
	// __init__ dominates its closure (0.7) is the finding worth the budget, and the
	// per-package summaries (informational) are the part a rollup can stand in for.
	// Sorted by title within a rank so the output stays deterministic.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Title < out[j].Title
	})
	out, omitted := common.Cap(out)
	if omitted > 0 {
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Additional import closures: %d more", omitted),
			Description: fmt.Sprintf(
				"%d further packages were measured and are not listed individually. They rank below "+
					"those above, which are the initialisations dominating the most of their closure.",
				omitted),
			Confidence:    0.5,
			Informational: true,
		})
	}
	return out, nil
}

// summary states what the entry point loads. It is Informational: it describes the
// graph rather than complaining about it, and a package that legitimately loads all
// of itself must not fail a gate for doing so.
func (e *Explainer) summary(entry, pkg string, closure map[string]int, total int, arrival map[string]int) facts.Insight {
	loaded := len(closure)
	// The closure leaves the package whenever it imports a sibling top-level one, so
	// the share of THIS package that loads has to count only the files inside it —
	// dividing the whole closure by the package's own file count reported 150%.
	inPkg := 0
	for f := range closure {
		if f == pkg+"/__init__.py" || strings.HasPrefix(f, pkg+"/") {
			inPkg++
		}
	}
	pct := 0
	if total > 0 {
		pct = inPkg * 100 / total
	}
	deepest := 0
	for _, d := range closure {
		if d > deepest {
			deepest = d
		}
	}
	ev := []facts.Evidence{{File: entry, Detail: fmt.Sprintf("entry point: %d modules load when this package is imported", loaded)}}
	for _, name := range sortedKeys(arrival) {
		ev = append(ev, facts.Evidence{Fact: name, Detail: fmt.Sprintf("third-party package on the import path, %d hop(s) in", arrival[name])})
	}
	return facts.Insight{
		Title: fmt.Sprintf("Importing %s loads %d module(s)", pkg, loaded),
		Description: fmt.Sprintf(
			"Importing %s executes %d module(s): %d of the %d in the package (%d%%), plus %d from outside it. It reaches a depth of %d and puts %d third-party package(s) on the import path. Imports that do not run at import time (function-local, TYPE_CHECKING) are excluded.",
			pkg, loaded, inPkg, total, pct, loaded-inPkg, deepest, len(arrival)),
		Confidence:    1.0,
		Informational: true,
		Evidence:      ev,
		Metrics: map[string]any{
			"modules_loaded": loaded, "modules_in_package": inPkg, "modules_total": total, "percent_loaded": pct,
			"max_depth": deepest, "third_party_count": len(arrival),
		},
	}
}

// externalImports maps each file to the third-party packages it imports at import
// time. Deferred imports are excluded for the same reason they carry no edge.
func externalImports(store *facts.Store) map[string][]string {
	out := map[string][]string{}
	seen := map[[2]string]bool{}
	for _, dep := range store.ByKind(facts.KindDependency) {
		if lang, _ := dep.Props["language"].(string); lang != "python" {
			continue
		}
		if deferred, _ := dep.Props["deferred"].(bool); deferred {
			continue
		}
		if src, _ := dep.Props[facts.PropSource].(string); src != facts.DepSourceExternal {
			continue
		}
		for _, rel := range dep.Relations {
			if rel.Kind != facts.RelImports {
				continue
			}
			name := rel.Target
			if i := strings.IndexByte(name, '.'); i > 0 {
				name = name[:i] // the distributed package, not the submodule
			}
			if name == "" {
				continue
			}
			key := [2]string{dep.File, name}
			if seen[key] {
				continue
			}
			seen[key] = true
			out[dep.File] = append(out[dep.File], name)
		}
	}
	return out
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dominatingBarrels reports the package __init__.py files that are solely responsible
// for a large part of what an entry point loads.
//
// A package's __init__.py runs whenever anything beneath it is imported, and a barrel
// that re-exports for convenience therefore loads its whole subtree for every
// importer — including one that wanted a single leaf module. The measurement is a
// dominator: how much of the closure disappears if the entry point stops reaching it
// through this file. That is what makes the finding actionable, because it is exactly
// what splitting the barrel would give back.
func (e *Explainer) dominatingBarrels(g *Graph, entry, pkg string, closure map[string]int, externals map[string][]string) []facts.Insight {
	type hit struct {
		file    string
		lost    []string
		lostExt []string
	}
	var hits []hit
	for file := range closure {
		if file == entry || !strings.HasSuffix(file, "/__init__.py") {
			continue
		}
		without := g.ClosureWithout(entry, file)
		var lost []string
		for f := range closure {
			if f == file {
				continue // the excluded file is trivially unreachable; it is not a cost it imposes
			}
			if _, still := without[f]; !still {
				lost = append(lost, f)
			}
		}
		if len(lost) < minDominated {
			continue
		}
		sort.Strings(lost)
		// The barrel is left out of the module count above, but its OWN third-party
		// imports are still something reaching it pays for.
		extSeen := map[string]bool{}
		var lostExt []string
		for _, f := range append([]string{file}, lost...) {
			for _, x := range externals[f] {
				if !extSeen[x] {
					extSeen[x] = true
					lostExt = append(lostExt, x)
				}
			}
		}
		sort.Strings(lostExt)
		hits = append(hits, hit{file, lost, lostExt})
	}
	sort.Slice(hits, func(i, j int) bool {
		if len(hits[i].lost) != len(hits[j].lost) {
			return len(hits[i].lost) > len(hits[j].lost)
		}
		return hits[i].file < hits[j].file
	})
	if len(hits) > maxReported {
		hits = hits[:maxReported]
	}

	var out []facts.Insight
	for _, h := range hits {
		ev := []facts.Evidence{{File: h.file, Detail: fmt.Sprintf("package initialisation that %d loaded module(s) depend on reaching", len(h.lost))}}
		for i, f := range g.Path(entry, h.file) {
			ev = append(ev, facts.Evidence{File: f, Detail: fmt.Sprintf("step %d of the chain that puts it on the import path", i)})
		}
		for i, f := range h.lost {
			if i >= 8 {
				break
			}
			ev = append(ev, facts.Evidence{File: f, Detail: "loaded only because that package initialises"})
		}
		for _, x := range h.lostExt {
			ev = append(ev, facts.Evidence{Fact: x, Detail: "third-party package that comes with it"})
		}
		desc := fmt.Sprintf(
			"Importing %s runs %s, and %d of the module(s) it loads are reachable only through it. A package __init__.py executes whenever anything beneath it is imported, so a re-export written for convenience is paid for by every importer, including one that wanted a single leaf module.",
			pkg, h.file, len(h.lost))
		if len(h.lostExt) > 0 {
			desc += fmt.Sprintf(" It also brings %d third-party package(s): %s.", len(h.lostExt), strings.Join(h.lostExt, ", "))
		}
		out = append(out, facts.Insight{
			Title:       fmt.Sprintf("%s loads %d module(s) only via %s", pkg, len(h.lost), h.file),
			Description: desc,
			Confidence:  0.7,
			Evidence:    ev,
			Actions: []string{
				"Import the leaf modules directly, so the package __init__.py is not on the path",
				"Narrow the __init__.py to the names its own importers actually use",
				"Defer the heavy re-exports into the functions that need them",
			},
			Metrics: map[string]any{"modules_dominated": len(h.lost), "third_party_dominated": len(h.lostExt)},
		})
	}
	return out
}
