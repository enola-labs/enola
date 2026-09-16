package perf

import (
	"context"
	"fmt"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

// Explainer adapts the performance analysis to enola's explainer subsystem so its
// findings surface via the OSS query_insights tool (explainer="performance"),
// alongside the OSS explainers. It runs during generate_snapshot and reuses the
// same collect/analyze core as the analyze_performance MCP tool.
type Explainer struct{}

// NewExplainer creates a performance Explainer.
func NewExplainer() *Explainer { return &Explainer{} }

// Name is the explainer identifier used as the insight Source and the
// query_insights explainer= filter value.
func (e *Explainer) Name() string { return "performance" }

// maxIndividualInsights caps the findings reported one by one; the remainder
// becomes a single rollup, and analyze_performance still returns the full set. The
// number matches the dead-code explainer's, deliberately: the two are read in the
// same list and a cap that differed between them would rank one above the other for
// no reason a reader could see.
//
// It is not cosmetic. Uncapped, this explainer produced 1,254 of the 1,385 findings
// on enola's own repository — ninety per cent of every finding in the snapshot, from
// one of twenty-two explainers, in a tool whose documentation says a list of
// everything wrong is not a list of anything you did. It also became gateable in the
// same change, and a gate nobody can read is a gate that gets switched off.
const maxIndividualInsights = common.MaxIndividualInsights

// Explain produces one insight per medium-or-higher-severity finding, capped.
func (e *Explainer) Explain(_ context.Context, store *facts.Store) ([]facts.Insight, error) {
	funcs, storage, routeHandlers, assoc := collect(store)
	return findingsToInsights(analyze(funcs, storage, routeHandlers, assoc)), nil
}

// findingsToInsights maps ranked performance findings to insights. Low-severity
// findings are omitted to keep query_insights focused on actionable risks; the
// analyze_performance tool still returns the full set.
//
// analyze() already returns findings ranked worst-first, so the budget is spent on
// the most severe rather than on whatever sorted first.
func findingsToInsights(findings []Finding) []facts.Insight {
	out := make([]facts.Insight, 0, len(findings))
	var dropped int
	// Per repository: one slow repo in a cluster must not spend the whole budget and
	// leave the others reading as clean when nobody looked at them.
	spent := map[string]int{}
	for _, f := range findings {
		if severityRank(f.Severity) < severityRank("medium") {
			continue
		}
		if spent[f.Repo] >= maxIndividualInsights {
			dropped++
			continue
		}
		spent[f.Repo]++
		// Prefer the finding's own confidence (decays with Big-O exponent / bounded-loop
		// discount / cold path); fall back to the severity-based default for any finding
		// that predates confidence scoring.
		conf := f.Confidence
		if conf == 0 {
			conf = 0.65
			if f.Severity == "high" {
				conf = 0.85
			}
		}
		ev := []facts.Evidence{{Symbol: f.Symbol, File: f.File, Detail: f.Why}}
		for _, e := range f.Evidence {
			ev = append(ev, facts.Evidence{Symbol: f.Symbol, Detail: e})
		}
		out = append(out, facts.Insight{
			Title:       fmt.Sprintf("Performance risk (%s, %s): %s", f.Kind, f.BigO, f.Symbol),
			Description: f.Why + " Big-O is a deterministic estimate of structural worst case from parser facts, not a proof — treat as a lead to verify.",
			Confidence:  conf,
			Evidence:    ev,
			Actions: []string{
				"Confirm the hot path with a profiler or benchmark before optimizing",
				"Batch or hoist per-iteration I/O out of loops; add memoization or an iterative rewrite for recursion",
			},
		})
	}
	if dropped > 0 {
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Additional performance risks: %d more (see analyze_performance)", dropped),
			Description: fmt.Sprintf(
				"%d further findings at medium severity or above are not listed individually. "+
					"They are ranked below those above, not dismissed: call analyze_performance "+
					"for the full set, with min_severity and package/repo filters.", dropped),
			Confidence: 0.5,
			Actions:    []string{"Call analyze_performance with min_severity=high to see the worst of the remainder"},
		})
	}
	return out
}
