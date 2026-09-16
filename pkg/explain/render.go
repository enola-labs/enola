package explain

import (
	"fmt"
	"strings"
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
	if r.ContractRoutes > 0 {
		countRow(&b, "  contract operations", r.ContractRoutes)
	}
	if r.ImplementedRoutes > 0 {
		countRow(&b, "  implemented routes", r.ImplementedRoutes)
	}
	if r.ClientRouteCalls > 0 {
		countRow(&b, "  outbound client calls", r.ClientRouteCalls)
	}
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

	// Enterprise / extra sections
	for _, s := range r.ExtraSections {
		fmt.Fprintf(&b, "%s\n", s.Title)
		body := s.Body
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		b.WriteString(body)
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
