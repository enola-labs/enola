// Package complexity flags functions and methods whose cyclomatic complexity is
// a statistical outlier for the repo. High-complexity functions concentrate
// branching logic, are hard to test exhaustively, and are common defect sites.
//
// It uses the "cyclomatic" prop that every language extractor records, so it
// works across all supported languages.
package complexity

import (
	"context"
	"fmt"
	"sort"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

const (
	// minComplexity is the floor below which a function is never reported, even
	// if it is the statistical max in a simple repo.
	minComplexity = 10
	// stdDevK is how many standard deviations above the mean a function's
	// complexity must sit to qualify as an outlier.
	stdDevK = 2.0
	// maxInsights caps how many outliers are reported, most complex first.
	maxInsights = 15
)

// ComplexityExplainer detects cyclomatic-complexity outliers.
type ComplexityExplainer struct{}

// New creates a new ComplexityExplainer.
func New() *ComplexityExplainer {
	return &ComplexityExplainer{}
}

func (e *ComplexityExplainer) Name() string {
	return "complexity-outliers"
}

// Explain reads cyclomatic complexity from function/method symbols and reports
// the statistical outliers above a sensible floor.
func (e *ComplexityExplainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	// Collapse #if/#else duplicates: a method declared once per branch is emitted as
	// two same-name facts, which would each enter the outlier distribution and could
	// each be flagged. Genuine overloads (not tagged conditional) are preserved.
	symbols := facts.CanonicalSymbols(store.ByKind(facts.KindSymbol))
	if len(symbols) == 0 {
		return nil, nil
	}

	type entry struct {
		fact       facts.Fact
		complexity int
	}
	var entries []entry
	values := make([]float64, 0, len(symbols))
	for _, s := range symbols {
		// Test code leaves BOTH the candidate set and the distribution, and that is a
		// deliberate divergence from god-class, which filters candidates only.
		//
		// The difference is in the metric, not in taste. God-class reads fan-in off the
		// reverse edge index, so dropping test-sourced edges would rewrite a retained
		// PRODUCTION symbol's reported number — a test caller is still a caller. Here
		// the metric is `cyclomatic`, read off the symbol's own props: nothing about
		// one symbol can change another's complexity, so removing test rows narrows the
		// comparison population without falsifying a single retained value.
		//
		// And the population is the claim. This explainer reports "well above the repo
		// average"; in the languages with no test-ignore glob (Swift, Kotlin, Java,
		// C/C++, PHP, Rust) that average otherwise includes every trivial linear test
		// method, which depresses the mean, compresses sigma and over-reports real
		// functions against scaffolding.
		if facts.IsTestPath(s.File) {
			continue
		}
		if !isCallable(s) {
			continue
		}
		c, ok := intProp(s.Props, "cyclomatic")
		if !ok {
			continue
		}
		entries = append(entries, entry{fact: s, complexity: c})
		values = append(values, float64(c))
	}
	if len(entries) == 0 {
		return nil, nil
	}

	threshold := common.OutlierThreshold(values, stdDevK)

	var flagged []entry
	for _, en := range entries {
		if en.complexity < minComplexity || float64(en.complexity) <= threshold {
			continue
		}
		flagged = append(flagged, en)
	}

	sort.Slice(flagged, func(i, j int) bool {
		if flagged[i].complexity != flagged[j].complexity {
			return flagged[i].complexity > flagged[j].complexity
		}
		return flagged[i].fact.Name < flagged[j].fact.Name
	})

	var insights []facts.Insight
	for i, en := range flagged {
		if i >= maxInsights {
			break
		}
		insights = append(insights, facts.Insight{
			// Title format is parsed by pkg/explain (Code health section); keep stable.
			Title: fmt.Sprintf("High cyclomatic complexity: %s (%d)", en.fact.Name, en.complexity),
			Description: fmt.Sprintf(
				"%q has a cyclomatic complexity of %d — well above the repo average. "+
					"Highly branched functions are hard to test fully and are frequent defect sites.",
				en.fact.Name, en.complexity,
			),
			Confidence: 0.7,
			Evidence: []facts.Evidence{{
				Symbol: en.fact.Name,
				File:   en.fact.File,
				Detail: fmt.Sprintf("cyclomatic complexity %d at line %d", en.complexity, en.fact.Line),
			}},
			Actions: []string{
				"Extract guard clauses and nested branches into helper functions",
				"Replace deep conditionals with table/strategy dispatch where possible",
				"Add tests covering the distinct branches before refactoring",
			},
		})
	}

	return insights, nil
}

// isCallable reports whether a symbol is a function or method (the only kinds
// for which cyclomatic complexity is meaningful). Symbols without a recorded
// kind are allowed through so a missing prop doesn't silently drop functions.
func isCallable(s facts.Fact) bool {
	kind, ok := s.PropAny("symbol_kind").(string)
	if !ok {
		return true
	}
	return kind == facts.SymbolFunc || kind == facts.SymbolMethod || kind == facts.SymbolGetter
}

// intProp coerces a Props value to int. Extractors store ints in memory, but a
// snapshot reloaded from JSONL decodes numbers as float64, so both are handled.
func intProp(props map[string]any, key string) (int, bool) {
	v, ok := props[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}
