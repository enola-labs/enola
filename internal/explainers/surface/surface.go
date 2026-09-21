// Package surface flags modules with an oversized public surface: large modules
// that export almost all of their symbols, encapsulating little.
//
// Two things keep this from flooding — which a naive ratio test does, because in
// Go ("Capitalized == public") and Ruby ("public is the default") most packages
// legitimately export the bulk of their symbols:
//
//   - mock/test/generated packages (which export everything) are skipped, and a
//     meaningful size + near-total export ratio is required; and
//   - only the worst offenders are reported (top N by public-surface size), so a
//     large repo surfaces a digestible shortlist rather than hundreds of hits.
//
// It is a heuristic shortlist of modules worth a visibility review, not a list of
// definite defects.
package surface

import (
	"context"
	"fmt"
	"sort"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/mcputil"
)

// noSurfaceLanguages are languages whose symbols carry no visibility signal, so an
// exported/total ratio over them measures the language rather than the module.
//
//   - Ruby is public-by-default: every class and method is "exported", the ratio is
//     ~100% for every module, and the reading floods with namespace modules. "Too
//     big/central" is already covered by the god-class and complexity explainers.
//   - Markdown symbols are a document and its headings, and a heading is exported by
//     construction — there is no private one. A document is not a module with an API,
//     and reading it as one produced this estate's most confident nonsense: a
//     changelog "exports 1,405 of 1,405 symbols (100%)". Twenty-two of 323 findings
//     across the corpus named a document before this skip existed.
//
// Other languages in a multi-repo snapshot are unaffected.
var noSurfaceLanguages = map[string]bool{
	"ruby":     true,
	"markdown": true,
}

const (
	// minSymbols is the smallest module (by symbol count) considered; below this,
	// exporting everything is unremarkable.
	minSymbols = 15
	// minExportedRatio is the exported/total ratio at or above which a module
	// counts as near-fully-exported.
	minExportedRatio = 0.85
	// maxInsights caps how many modules are reported, largest public surface
	// first, so the output stays a shortlist on big repos.
	maxInsights = 20
	// maxEvidence caps how many exported symbols are listed per insight.
	maxEvidence = 8
)

// (The old excludedSegments map lived here. It has been replaced by the shared
// facts.IsTestPath — see excludedModule.)

// SurfaceExplainer detects modules with an oversized exported surface.
type SurfaceExplainer struct{}

// New creates a new SurfaceExplainer.
func New() *SurfaceExplainer {
	return &SurfaceExplainer{}
}

func (e *SurfaceExplainer) Name() string {
	return "exported-surface"
}

type moduleSurface struct {
	total    int
	exported []string
}

// Explain tallies exported vs total symbols per module and reports the modules
// with the largest near-fully-exported public surface.
func (e *SurfaceExplainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	// Collapse #if/#else duplicates so a type declared once per branch is not counted
	// twice toward a module's exported/total surface. Genuine overloads (same base
	// name, not tagged conditional) are preserved.
	symbols := facts.CanonicalSymbols(store.ByKind(facts.KindSymbol))
	if len(symbols) == 0 {
		return nil, nil
	}

	// Resolve each symbol against the modules that actually exist rather than
	// guessing from its name. See common.SymbolModuleIn: a .NET directory contains
	// dots, so the guess split every jellyfin project at its first one.
	declared := make(map[string]bool)
	for _, m := range store.ByKind(facts.KindModule) {
		declared[m.Name] = true
	}

	mods := make(map[string]*moduleSurface)
	for _, s := range symbols {
		if lang, _ := s.PropAny("language").(string); noSurfaceLanguages[lang] {
			continue
		}
		exported, ok := s.PropAny("exported").(bool)
		if !ok {
			// Extractor didn't record visibility; ignore so it doesn't distort the ratio.
			continue
		}
		mod := common.SymbolModuleIn(s.Name, declared)
		ms := mods[mod]
		if ms == nil {
			ms = &moduleSurface{}
			mods[mod] = ms
		}
		ms.total++
		if exported {
			ms.exported = append(ms.exported, s.Name)
		}
	}

	type candidate struct {
		module string
		ms     *moduleSurface
		ratio  float64
	}
	var candidates []candidate
	for mod, ms := range mods {
		if ms.total < minSymbols || excludedModule(mod) {
			continue
		}
		ratio := float64(len(ms.exported)) / float64(ms.total)
		if ratio < minExportedRatio {
			continue
		}
		candidates = append(candidates, candidate{module: mod, ms: ms, ratio: ratio})
	}

	// Largest public surface first (the most worth reviewing), then by ratio,
	// then name — so the top-N cap keeps the most significant modules.
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i].ms.exported) != len(candidates[j].ms.exported) {
			return len(candidates[i].ms.exported) > len(candidates[j].ms.exported)
		}
		if candidates[i].ratio != candidates[j].ratio {
			return candidates[i].ratio > candidates[j].ratio
		}
		return candidates[i].module < candidates[j].module
	})

	var insights []facts.Insight
	for i, c := range candidates {
		if i >= maxInsights {
			break
		}
		exportedCount := len(c.ms.exported)
		sorted := append([]string(nil), c.ms.exported...)
		sort.Strings(sorted)
		evidence := make([]facts.Evidence, 0, maxEvidence)
		for j, sym := range sorted {
			if j >= maxEvidence {
				break
			}
			evidence = append(evidence, facts.Evidence{Symbol: sym, Detail: "exported"})
		}

		insights = append(insights, facts.Insight{
			// Title format is parsed by pkg/explain (Code health section); keep stable.
			Title: fmt.Sprintf("Large public surface: %s exports %d of %d symbols (%.0f%%)", c.module, exportedCount, c.ms.total, c.ratio*100),
			Description: fmt.Sprintf(
				"Module %q exports %d of its %d symbols (%.0f%%). A large, near-fully-exported module hides "+
					"little of its implementation, so consumers can couple to internals and it is hard to "+
					"change without breaking them. Worth a visibility review — though a deliberate library "+
					"API may legitimately export most of its surface.",
				c.module, exportedCount, c.ms.total, c.ratio*100,
			),
			Confidence: 0.6,
			Evidence:   evidence,
			Actions: []string{
				"Unexport symbols that are only used within the module",
				"Define a small, intentional public API and hide the rest",
				"Split the module if it has several independent public responsibilities",
			},
		})
	}

	return insights, nil
}

// excludedModule reports whether a module is test scaffolding or generated output:
// both naturally export everything and are not hand-maintained public APIs.
//
// It used to carry its own segment list, lowercasing the module path to match it.
// That covered `Tests/` (via `tests`) but not `androidTest` — which lowercases to
// `androidtest` — nor `__tests__` nor the Kotlin-Multiplatform trees, so
// a large Android app reported "Large public surface:
// app/src/androidTest/java/…/ui/feature/compose" as a finding. Both questions now have
// exactly one owner each: facts.IsTestPath and mcputil.IsGeneratedPath. Note
// IsTestPath is case-SENSITIVE and carries the real-world spellings (`Tests`,
// `Mocks`, `androidTest`, `__tests__`), so the lowercasing is gone with the list.
func excludedModule(mod string) bool {
	return facts.IsTestPath(mod) || mcputil.IsGeneratedPath(mod)
}
