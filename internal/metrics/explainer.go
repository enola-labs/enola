package metrics

import (
	"context"
	"fmt"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
)

// maxMetricInsights bounds how many off-main-sequence insights the explainer
// emits, so a large repo does not flood query_insights. The remainder is reported
// as a single rollup; package_metrics returns the full ranked list.
const maxMetricInsights = 50

// Explainer adapts package metrics to enola's explainer subsystem so its findings
// surface via the OSS query_insights tool (explainer="package-metrics"), alongside
// the OSS explainers. It runs during generate_snapshot and reuses the same
// collect/compute core as the package_metrics MCP tool.
type Explainer struct{}

// NewExplainer creates a package-metrics Explainer.
func NewExplainer() *Explainer { return &Explainer{} }

// Name is the explainer identifier used as the insight Source and the
// query_insights explainer= filter value.
func (e *Explainer) Name() string { return "package-metrics" }

// Explain emits insights for packages far from the main sequence plus the
// most-depended-upon (god) package.
func (e *Explainer) Explain(_ context.Context, store *facts.Store) ([]facts.Insight, error) {
	pkgs, edges, _ := collect(store)
	return metricsToInsights(compute(pkgs, edges)), nil
}

// metricsToInsights flags packages with distance > painfulDistance (worst first)
// and the highest-Ca package as a change-risk concentrator.
func metricsToInsights(results []PackageMetric) []facts.Insight {
	off := make([]PackageMetric, 0)
	var mostCoupled PackageMetric
	for _, m := range results {
		if isOffMainSequence(m) {
			off = append(off, m)
		}
		if m.Ca > mostCoupled.Ca {
			mostCoupled = m
		}
	}
	sort.SliceStable(off, func(i, j int) bool {
		if off[i].Distance != off[j].Distance {
			return off[i].Distance > off[j].Distance
		}
		return off[i].Package < off[j].Package
	})

	out := make([]facts.Insight, 0, len(off)+1)
	dropped := 0
	for _, m := range off {
		if len(out) >= maxMetricInsights {
			dropped++
			continue
		}
		// A package is only in this list when D > painfulDistance (0.7), which forces
		// A+I<0.3 (both low → zone of pain: stable + concrete) or A+I>1.7 (both high →
		// zone of uselessness: unstable + abstract). So instability alone cleanly
		// separates the two corners; an unstable+concrete package can never have
		// D>0.7 and thus never reaches here. (Data/model packages that are mostly value
		// carriers are concrete BY DESIGN and are filtered upstream by isOffMainSequence,
		// so they never reach this list.)
		kind := "rigid (zone of pain: stable + concrete — many packages depend on it, so it is hard to change without editing dependents)"
		// "Extract interfaces" is Martin's classic remedy for statically-typed OO, but
		// abstractness is only an approximation for dynamically-typed languages (Python
		// duck typing / plain base classes yield A≈0), where a stable+concrete hub is
		// usually correct by design. Frame the action as contract-stability guidance
		// rather than prescribing interface extraction.
		action := "Stable hub: keep its public contract small, cohesive and stable; split it if it spans multiple responsibilities. " +
			"(Abstractness is only an approximation for dynamically-typed languages, so \"extract interfaces\" may not apply.)"
		conf := 0.6 + (m.Distance - painfulDistance)
		if conf > 0.9 {
			conf = 0.9
		}
		// Every m here is off-main-sequence (from the `off` list), so Classify returns
		// ZonePain or ZoneUseless — the same split as `m.Instability >= 0.5`, shared so
		// the dashboard and this explainer can never label a package differently.
		if Classify(m) == ZoneUseless {
			kind = "useless (zone of uselessness: unstable + abstract — abstractions almost nothing depends on)"
			action = "Useless: remove the unused abstraction, or find concrete consumers for it."
		}
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Off main sequence: %s (D=%.2f)", m.Package, m.Distance),
			Description: fmt.Sprintf("Package %q sits %.2f from the main sequence (I=%.2f, A=%.2f) — %s.",
				m.Package, m.Distance, m.Instability, m.Abstractness, kind),
			Confidence: conf,
			Evidence: []facts.Evidence{{
				Fact:   m.Package,
				Detail: fmt.Sprintf("Ca=%d Ce=%d N=%d I=%.2f A=%.2f D=%.2f", m.Ca, m.Ce, m.ClassesInterfaces, m.Instability, m.Abstractness, m.Distance),
			}},
			Actions: []string{action},
		})
	}
	if dropped > 0 {
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Off main sequence: %d more package(s)", dropped),
			Description: "Additional off-main-sequence packages were omitted from individual insights. " +
				"Use the package_metrics tool (sort_by=distance) for the full ranked list.",
			Confidence: 0.4,
			Actions:    []string{"Run package_metrics with sort_by=distance"},
		})
	}
	if mostCoupled.Package != "" && mostCoupled.Ca >= 5 {
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Most depended-upon package: %s (Ca=%d)", mostCoupled.Package, mostCoupled.Ca),
			Description: fmt.Sprintf("%q is depended upon by %d other packages — a change-risk concentrator. "+
				"Edits ripple across dependents, so keep its public contract stable.", mostCoupled.Package, mostCoupled.Ca),
			Confidence: 0.6,
			Evidence: []facts.Evidence{{
				Fact:   mostCoupled.Package,
				Detail: fmt.Sprintf("Ca=%d I=%.2f", mostCoupled.Ca, mostCoupled.Instability),
			}},
			Actions: []string{"Stabilize the package's public contract; split it if it spans multiple responsibilities"},
		})
	}
	return out
}
