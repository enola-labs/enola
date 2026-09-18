package explain

import (
	"fmt"
	"strings"

	"github.com/enola-labs/enola/internal/metrics"
	"github.com/enola-labs/enola/internal/perf"
)

// Render returns the human-readable report as a single string, ready to print to
// a terminal. Sections are plain aligned text (not markdown) so they read well
// directly in a shell.
func (r *Report) Render() string {
	var b strings.Builder

	repo := r.RepoPath
	if repo == "" {
		repo = "(unknown)"
	}
	rule := strings.Repeat("═", 60)
	fmt.Fprintf(&b, "%s\n", rule)
	fmt.Fprintf(&b, " Repository explanation: %s\n", repo)
	fmt.Fprintf(&b, "%s\n\n", rule)

	// Overview
	b.WriteString("Overview\n")
	if r.GeneratedAt != "" {
		kv(&b, "Generated", r.GeneratedAt)
	}
	if r.Duration != "" {
		kv(&b, "Analysis time", r.Duration)
	}
	// Prefer actual source languages (from per-fact language props); fall back to
	// extractor names for pre-language snapshots whose facts lack the prop.
	langs := r.Languages
	if len(langs) == 0 {
		langs = r.Extractors
	}
	if len(langs) > 0 {
		kv(&b, "Languages", strings.Join(langs, ", "))
	}
	kv(&b, "Total facts", fmt.Sprintf("%d", r.TotalFacts))
	b.WriteString("\n")

	// Architectural kinds
	b.WriteString("Architectural kinds\n")
	if len(r.KindCounts) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, kc := range r.KindCounts {
		countRow(&b, kc.Label, kc.Count)
	}
	b.WriteString("\n")

	// Relations
	if len(r.RelationCounts) > 0 {
		b.WriteString("Relations\n")
		for _, rc := range r.RelationCounts {
			countRow(&b, rc.Label, rc.Count)
		}
		b.WriteString("\n")
	}

	// Symbol breakdown
	if len(r.SymbolKinds) > 0 {
		b.WriteString("Symbol breakdown\n")
		for _, sk := range r.SymbolKinds {
			countRow(&b, sk.Label, sk.Count)
		}
		b.WriteString("\n")
	}

	// API surface
	b.WriteString("API & data surface\n")
	countRow(&b, "routes", r.Routes)
	for _, m := range r.RoutesByMethod {
		countRow(&b, "  "+m.Label, m.Count)
	}
	countRow(&b, "storage", r.Storage)
	b.WriteString("\n")

	// Dependencies
	if len(r.DepSources) > 0 {
		b.WriteString("Dependencies\n")
		for _, d := range r.DepSources {
			countRow(&b, d.Label, d.Count)
		}
		b.WriteString("\n")
	}

	// Architecture insights
	b.WriteString("Architecture\n")
	if len(r.Architectures) > 0 {
		// One line per statement. A repository with two cohorts has two layer
		// orders in force, and printing only the stronger one reads as a claim
		// that the other half of the repository has none.
		for _, a := range r.Architectures {
			kv(&b, "Pattern", fmt.Sprintf("%s (%.0f%% confidence)", a.Pattern, a.Confidence*100))
		}
	} else if r.Architecture != "" {
		kv(&b, "Pattern", fmt.Sprintf("%s (%.0f%% confidence)", r.Architecture, r.ArchConfidence*100))
	} else {
		kv(&b, "Pattern", "(none detected)")
	}
	countRow(&b, "cyclic dependencies", r.Cycles)
	countRow(&b, "layer violations", r.LayerViolations)
	if r.CrossRepoEdges > 0 {
		countRow(&b, "cross-repo edges", r.CrossRepoEdges)
	}
	b.WriteString("\n")

	// Impact / hotspots
	b.WriteString("Impact analysis (hotspots)\n")
	countRow(&b, "coupled modules", r.HighCriticality+r.MediumCriticality)
	countRow(&b, "  high criticality", r.HighCriticality)
	countRow(&b, "  medium criticality", r.MediumCriticality)
	if r.CouplingUnresolved {
		b.WriteString("  Note: coupling could not be resolved from the import graph\n")
		b.WriteString("        (imports did not match any module).\n")
	}
	if len(r.Hotspots) > 0 {
		b.WriteString("  Top hotspots (by coupling):\n")
		fmt.Fprintf(&b, "    %-32s %7s %8s %-8s %s\n", "module", "fan-in", "fan-out", "crit", "blast radius")
		for _, h := range r.Hotspots {
			fmt.Fprintf(&b, "    %-32s %7d %8d %-8s %d\n",
				truncate(h.Module, 32), h.FanIn, h.FanOut, h.Criticality, h.BlastRadius)
		}
	}
	b.WriteString("\n")

	// Package metrics — the same module coupling the section above reports as
	// fan-in and fan-out, read as Martin's model. Deliberately adjacent: Ca IS the
	// fan-in, and a reader who sees them apart tends to treat them as two findings.
	if m := r.PackageMetrics; m != nil {
		b.WriteString("Package metrics\n")
		fmt.Fprintf(&b, "  %-24s %6d%s\n", "packages analyzed", m.Analyzed, metrics.ExcludedNote(m.Typeless, m.ExcludedTestTooling))
		fmt.Fprintf(&b, "  %-24s %6.2f\n", "avg instability (I)", m.AvgI)
		fmt.Fprintf(&b, "  %-24s %6.2f\n", "avg distance (D)", m.AvgD)
		fmt.Fprintf(&b, "  %-24s %6d  (D > %.1f, N >= %d, coupled — rigid or useless)\n",
			"off main sequence", m.OffMain, m.PainfulDistance, m.MinPainfulTypes)
		if m.MostCoupledPackage != "" {
			fmt.Fprintf(&b, "  %-24s %s (Ca=%d)\n", "most depended-upon", m.MostCoupledPackage, m.MostCoupledCa)
		}
		b.WriteString("\n")
	}

	// Code health — symbol/module-level findings from the god-class, hotspots,
	// dependency-depth, exported-surface and complexity explainers. Distinct from
	// the module-coupling "Impact analysis (hotspots)" section above.
	if len(r.CodeHealth) > 0 {
		b.WriteString("Code health\n")
		for _, g := range r.CodeHealth {
			countRow(&b, g.Label, g.Count)
			for _, it := range g.Top {
				fmt.Fprintf(&b, "    %-44s %s\n", truncate(it.Name, 44), it.Detail)
			}
		}
		b.WriteString("\n")
	}

	// Dead code and Performance — symbol-level findings, so they sit with Code
	// health rather than after the module-level sections.
	if d := r.DeadCode; d != nil {
		b.WriteString("Dead code\n")
		fmt.Fprintf(&b, "  %-22s %6d  (of %d symbols)\n", "potential dead code", d.Total, d.Candidates)
		// The tiers partition the set, listed first so the eye starts at the small
		// actionable bucket rather than the larger cross-cutting counts below.
		fmt.Fprintf(&b, "    %-20s %6d   (%s)\n", "high confidence", d.High, "functions — incoming calls tracked; safest to remove first")
		fmt.Fprintf(&b, "    %-20s %6d   (%s)\n", "medium confidence", d.Medium, "structs/classes/interfaces — usage tracked via instantiate/inject/implements")
		fmt.Fprintf(&b, "    %-20s %6d   (%s)\n", "low confidence", d.Low, "methods & types — dispatch/reflection & type usage not edge-tracked; verify each")
		// Orthogonal cuts of the SAME set, not further buckets.
		fmt.Fprintf(&b, "    %-20s %6d   (%s)\n", "isolated", d.Isolated, "breakdown: no references in or out")
		fmt.Fprintf(&b, "    %-20s %6d   (%s)\n", "unreferenced", d.Unreferenced, "breakdown: unreferenced, but references others")
		fmt.Fprintf(&b, "    %-20s %6d   (%s)\n", "exported", d.Exported, "breakdown: public API — may be used outside this snapshot")
		b.WriteString("  (heuristic — references matched by short name; verify before removal)\n")
		b.WriteString("\n")
	}

	if p := r.Performance; p != nil {
		b.WriteString("Performance\n")
		fmt.Fprintf(&b, "  %-24s %6d\n", "functions analyzed", p.FunctionsAnalyzed)
		fmt.Fprintf(&b, "  %-24s %6d   (high %d / medium %d / low %d)\n",
			"performance findings", p.Total, p.High, p.Medium, p.Low)
		if line := bucketLine(p.ByKind); line != "" {
			fmt.Fprintf(&b, "  %-24s %s\n", "by kind", line)
		}
		if line := bucketLine(p.ByComplexity); line != "" {
			fmt.Fprintf(&b, "  %-24s %s\n", "by complexity", line)
		}
		if len(p.Top) > 0 {
			b.WriteString("  top findings:\n")
			for _, f := range p.Top {
				loc := f.Symbol
				if f.File != "" {
					loc = fmt.Sprintf("%s (%s:%d)", f.Symbol, f.File, f.Line)
				}
				fmt.Fprintf(&b, "    [%s] %s %s — %s\n", f.Severity, f.BigO, loc, f.Kind)
			}
			if p.Total > len(p.Top) {
				fmt.Fprintf(&b, "  %d more — analyze_performance filters by package and severity.\n", p.Total-len(p.Top))
			}
		}
		b.WriteString("\n")
	}

	// Vendored candidates — a scope note, deliberately after Code health and
	// deliberately worded as a question rather than a defect. Nothing here was
	// excluded from the snapshot, and the reader is the one who decides.
	if v := r.Vendored; v != nil && v.Count > 0 {
		b.WriteString("Vendored candidates (nothing excluded)\n")
		fmt.Fprintf(&b, "  %d director%s carrying their own licence under a dependency-named parent (%d files)\n",
			v.Count, vendoredPlural(v.Count), v.Files)
		for _, it := range v.Top {
			fmt.Fprintf(&b, "    %-44s %s\n", truncate(it.Name, 44), it.Detail)
		}
		if v.Omitted > 0 {
			fmt.Fprintf(&b, "    %-44s %s\n", fmt.Sprintf("… and %d more", v.Omitted), "see .enola/insights.json")
		}
		b.WriteString("    Add any you agree are vendored to `ignore:` in your enola config.\n")
		b.WriteString("\n")
	}

	return b.String()
}

func kv(b *strings.Builder, key, val string) {
	fmt.Fprintf(b, "  %-20s %s\n", key+":", val)
}

func countRow(b *strings.Builder, label string, n int) {
	fmt.Fprintf(b, "  %-22s %6d\n", label, n)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

// vendoredPlural gives the English plural ending of "directory" for n.
func vendoredPlural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// bucketLine renders an ordered distribution as "label n · label n". Empty when
// there is nothing in it, so the caller can drop the row rather than print a
// heading with nothing after it.
func bucketLine(bs []perf.Bucket) string {
	parts := make([]string, 0, len(bs))
	for _, b := range bs {
		parts = append(parts, fmt.Sprintf("%s %d", b.Label, b.Count))
	}
	return strings.Join(parts, " · ")
}
