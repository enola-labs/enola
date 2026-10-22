package server

// The tools that read what the explainers concluded: query_insights and
// coverage_report.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/coverage"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// filterInsights returns the insights matching all of the supplied filters.
// explainer is matched case-insensitively against Insight.Source; repo matches
// the repo-prefix path segment of each insight's evidence files (insights have
// no structured repo field) — see insightBelongsToRepo; minConfidence keeps
// insights at or above the bar. multiRepo reports whether the snapshot spans
// more than one repo, which selects strict vs. legacy repo matching. moduleRepos
// names the repos declaring each module, for evidence that cites a module by its
// bare name (see insightBelongsToRepo); nil when no repo filter is asked for.
func filterInsights(insights []facts.Insight, explainer, repo string, minConfidence float64, multiRepo bool, moduleRepos map[string]map[string]bool) []facts.Insight {
	repoLC := strings.ToLower(strings.TrimSpace(repo))
	var out []facts.Insight
	for _, in := range insights {
		if explainer != "" && !strings.EqualFold(in.Source, strings.TrimSpace(explainer)) {
			continue
		}
		if in.Confidence < minConfidence {
			continue
		}
		if repoLC != "" && !insightBelongsToRepo(in, repoLC, multiRepo, moduleRepos) {
			continue
		}
		out = append(out, in)
	}
	return out
}

// crossRepoExplainer reports whether name identifies an explainer whose findings
// depend on KindService nodes — the cross-repo "graph of graphs" the linker builds
// only for a multi-repo (append-mode) snapshot. On a single-repo snapshot these
// explainers return nothing not because the code is clean but because they could
// not run; the response must say which. Keep in sync with crossrepo.ComputeLinks,
// which returns nil below two repos.
func crossRepoExplainer(name string) bool {
	switch name {
	case "unused-routes", "coverage", "crossrepo":
		return true
	}
	return false
}

// producingExplainers returns the sorted, de-duplicated set of explainer names
// that actually produced at least one insight in snap, read from the Insight.Source
// the engine stamps. Unlike SnapshotMeta.Explainers — the ran-without-error set,
// which includes explainers gated out to zero insights — this lists only the
// genuine sources, so it never implies an insight came from an explainer that
// produced none.
func producingExplainers(snap *facts.Snapshot) []string {
	seen := map[string]struct{}{}
	for _, in := range snap.Insights {
		if in.Source != "" {
			seen[in.Source] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// noMatchInsightsMessage builds the query_insights response for the case where no
// insight matched the filter. It separates three situations the old single string
// conflated: nothing was produced at all; a cross-repo explainer could not run
// because the snapshot is single-repo (no KindService nodes); and a genuine
// no-match. The middle wording mirrors coverage_report's, so the two surfaces read
// the same for the same underlying cause.
func noMatchInsightsMessage(explainer, repo string, minConfidence float64, snap *facts.Snapshot, store *facts.Store) string {
	if len(snap.Insights) == 0 {
		return "No insights were produced for this snapshot."
	}
	if crossRepoExplainer(explainer) && len(store.ByKind(facts.KindService)) == 0 {
		return fmt.Sprintf(
			"Explainer %q did not run: it needs a multi-repo (append-mode) snapshot. "+
				"Snapshot the backend, then each client with append=true.", explainer)
	}
	return fmt.Sprintf(
		"No insights matched (explainer=%q, repo=%q, min_confidence=%.2f). The snapshot has %d insight(s) produced by: %v. "+
			"Call query_insights without filters to list them.",
		explainer, repo, minConfidence, len(snap.Insights), producingExplainers(snap))
}

// singleRepoServiceHint returns an append-mode hint (and true) when a
// query_facts(kind=service) call returned nothing because the snapshot is
// single-repo — service nodes exist only in multi-repo (append-mode) snapshots, so
// a bare marshalled null here reads like an absence of edges rather than an absence
// of the whole cross-repo graph. Mirrors coverage_report's wording.
func singleRepoServiceHint(kind string, resultCount int, store *facts.Store) (string, bool) {
	if kind == facts.KindService && resultCount == 0 && len(store.RepoLabels()) <= 1 {
		return "No service nodes found: query_facts(kind=\"service\") needs a multi-repo (append-mode) snapshot. " +
			"Snapshot the backend, then each client with append=true.", true
	}
	return "", false
}

// pathInRepo reports whether a repo-prefixed evidence path (e.g.
// "golf/internal/x.go") belongs to repo (given lowercased). Matching is on the
// first path segment, so "golf" does not match "golf-ui/..." or
// "my-golf-journal-*".
func pathInRepo(path, repoLC string) bool {
	p := strings.ToLower(strings.TrimSpace(path))
	return p == repoLC || strings.HasPrefix(p, repoLC+"/")
}

// insightBelongsToRepo reports whether an insight is about repo (given
// lowercased). In multi-repo snapshots evidence paths are repo-prefixed, so we
// match the path-segment of each evidence File/Fact exactly. The title
// substring match is dropped there because titles aren't reliably repo-qualified
// and over-match shared tokens (e.g. "golf" in "golf-ui"). Single-repo snapshots
// don't prefix evidence paths, so there we keep the legacy substring heuristic —
// it can't leak across repos because there are no siblings.
func insightBelongsToRepo(in facts.Insight, repoLC string, multiRepo bool, moduleRepos map[string]map[string]bool) bool {
	for _, ev := range in.Evidence {
		if pathInRepo(ev.File, repoLC) || pathInRepo(ev.Fact, repoLC) {
			return true
		}
	}
	if multiRepo {
		return citesOnlyModulesOf(in, repoLC, moduleRepos)
	}
	// Single-repo legacy fallback (unchanged behavior).
	if strings.Contains(strings.ToLower(in.Title), repoLC) {
		return true
	}
	for _, ev := range in.Evidence {
		if strings.Contains(strings.ToLower(ev.File), repoLC) ||
			strings.Contains(strings.ToLower(ev.Fact), repoLC) {
			return true
		}
	}
	return false
}

// citesOnlyModulesOf reports whether every piece of an insight's evidence names a
// module the repo declares. A cycle's evidence cites its modules by their bare
// names (`src/components`), which carry no repo prefix, so a repo filter on a
// multi-repo snapshot used to drop every cycle. All of them must belong to the
// repo, not one: two repositories may each have a `src/components`, and a cycle
// never spans repositories.
func citesOnlyModulesOf(in facts.Insight, repoLC string, moduleRepos map[string]map[string]bool) bool {
	if len(in.Evidence) == 0 || moduleRepos == nil {
		return false
	}
	for _, ev := range in.Evidence {
		if ev.File != "" || !moduleRepos[ev.Fact][repoLC] {
			return false
		}
	}
	return true
}

// moduleReposOf maps each module name to the lowercased labels of the repos that
// declare it.
func moduleReposOf(store *facts.Store) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, m := range store.ByKind(facts.KindModule) {
		if out[m.Name] == nil {
			out[m.Name] = map[string]bool{}
		}
		out[m.Name][strings.ToLower(m.Repo)] = true
	}
	return out
}

// renderInsightsSummary lists one row per insight (explainer, confidence, title)
// with a by-explainer tally — the cheapest way to size and triage findings.
func renderInsightsSummary(insights []facts.Insight) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found **%d** insight(s).\n\n", len(insights))

	bySource := map[string]int{}
	for _, in := range insights {
		bySource[insightSource(in)]++
	}
	if len(bySource) > 0 {
		sb.WriteString("## By explainer\n\n")
		for _, s := range topCounts(bySource, len(bySource)) {
			fmt.Fprintf(&sb, "- %s: %d\n", s, bySource[s])
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Insights\n\n")
	sb.WriteString("| Explainer | Confidence | Title |\n|---|---|---|\n")
	for _, in := range insights {
		fmt.Fprintf(&sb, "| %s | %.2f | %s |\n", insightSource(in), in.Confidence, oneLine(in.Title))
	}
	sb.WriteString("\n_Use output_mode='compact' for descriptions, evidence, and suggested actions, or 'full' for complete JSON._\n")
	return sb.String()
}

// renderInsightsCompact renders each insight with its description, an evidence
// sample (capped), and suggested actions.
func renderInsightsCompact(insights []facts.Insight) string {
	const evidenceSample = 10
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found **%d** insight(s).\n\n", len(insights))
	for i, in := range insights {
		fmt.Fprintf(&sb, "### %d. %s\n", i+1, in.Title)
		fmt.Fprintf(&sb, "- explainer: %s · confidence: %.2f\n", insightSource(in), in.Confidence)
		if in.Description != "" {
			fmt.Fprintf(&sb, "- %s\n", in.Description)
		}
		if len(in.Evidence) > 0 {
			fmt.Fprintf(&sb, "- evidence (%d):\n", len(in.Evidence))
			shown := len(in.Evidence)
			if shown > evidenceSample {
				shown = evidenceSample
			}
			for _, ev := range in.Evidence[:shown] {
				fmt.Fprintf(&sb, "    - %s\n", formatEvidence(ev))
			}
			if len(in.Evidence) > shown {
				fmt.Fprintf(&sb, "    - … and %d more (output_mode='full' for all)\n", len(in.Evidence)-shown)
			}
		}
		if len(in.Actions) > 0 {
			sb.WriteString("- suggested actions:\n")
			for _, a := range in.Actions {
				fmt.Fprintf(&sb, "    - %s\n", a)
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// insightSource returns the producing explainer name, or a placeholder when unset.
func insightSource(in facts.Insight) string {
	if in.Source == "" {
		return "—"
	}
	return in.Source
}

// formatEvidence joins an evidence record's non-empty fields into one line.
func formatEvidence(ev facts.Evidence) string {
	var parts []string
	for _, p := range []string{ev.Fact, ev.Symbol, ev.File} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	s := strings.Join(parts, " ")
	if ev.Detail != "" {
		if s != "" {
			s += " — " + ev.Detail
		} else {
			s = ev.Detail
		}
	}
	return s
}

// oneLine collapses newlines and escapes pipes so a string is safe inside a
// single markdown table cell.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", "\\|")
}

// explainerHints annotates the explainer names whose purpose is not obvious from the
// name alone. Names without an entry are rendered bare.
var explainerHints = map[string]string{
	"unused-routes":      "dead/uncalled HTTP routes, routes called only from client code that never runs, and a route registered in two files",
	"messaging-coverage": "undeclared messaging calls and AsyncAPI operations with no detected implementation",
	"intent":             "declared architecture verdicted against the measured graph",
	"constraints":        "declared component rules, and the edges that breach them",
	"domain":             "what a framework's declarations say about its data and API",
	"query-loops":        "a database query issued once per loop iteration",
	"entry-points":       "the symbols a framework invokes directly",
	"packwerk":           "package.yml dependencies declared but unused, or used but undeclared",
}

// explainerHintList renders the annotated explainers for the query_insights tool
// description, in the order the engine runs them. The full explainer= vocabulary lives
// once, on the parameter's schema (held complete by TestQueryInsightsNamesEveryExplainer);
// the description only glosses the names that do not explain themselves.
func explainerHintList() string {
	parts := make([]string, 0, len(explainerHints))
	for _, name := range config.KnownExplainers {
		if hint := explainerHints[name]; hint != "" {
			parts = append(parts, name+" ("+hint+")")
		}
	}
	return strings.Join(parts, ", ")
}

// registerCoverageReportTool registers the coverage_report tool.
func (s *Server) registerCoverageReportTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "coverage_report",
		Description: "Tells a genuinely isolated service from one whose edges enola could not resolve; use it before concluding a service is isolated. " +
			"Per service: resolved outbound dependencies and, per edge type (currently http_client), outbound call sites detected, resolved and unresolved, " +
			"classified connected, coverage_gap (likely NOT isolated, verify against source) or isolated. " +
			"Multi-repo snapshots only; a single-repo snapshot has no service nodes.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args coverageReportArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		report := coverage.Build(store, args.Repo)
		if len(report) == 0 {
			if args.Repo != "" {
				return errorResult(fmt.Sprintf("No service node named %q. coverage_report needs a multi-repo (append-mode) snapshot.", args.Repo)), nil, nil
			}
			return textResult("No service nodes found. coverage_report needs a multi-repo (append-mode) snapshot." +
				coverage.RenderClientsMarkdown(coverage.BuildClients(store))), nil, nil
		}

		if resolveOutputMode(args.OutputMode, modeSummary) == modeFull {
			return jsonResult(report)
		}
		return textResult(report.RenderMarkdown() + coverage.RenderClientsMarkdown(coverage.BuildClients(store))), nil, nil
	})
}

// registerQueryInsightsTool registers the query_insights tool.
func (s *Server) registerQueryInsightsTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "query_insights",
		Description: "Findings the explainers computed during generate_snapshot: the direct answer to \"which routes are unused?\", \"where are the dependency cycles?\", \"which modules are god-classes?\". Prefer it to hand-diffing query_facts results. " +
			"Each insight has a title, its explainer, a description, a confidence (below 1 is a candidate to verify, not a verdict), evidence (files, symbols, routes) and suggested actions. " +
			"Route and cross-repo findings (unused-routes, crossrepo, coverage) appear only in multi-repo snapshots of a backend plus its clients. " +
			"Explainers whose name is not self-explanatory: " + explainerHintList() + ".",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args queryInsightsArgs) (*mcp.CallToolResult, any, error) {
		snap := s.eng.Snapshot()
		if snap == nil {
			return errorResult("No snapshot available. Run generate_snapshot first."), nil, nil
		}

		repos := map[string]struct{}{}
		for _, repo := range s.eng.Store().RepoLabels() {
			repos[strings.ToLower(repo)] = struct{}{}
		}
		var moduleRepos map[string]map[string]bool
		if strings.TrimSpace(args.Repo) != "" && len(repos) > 1 {
			moduleRepos = moduleReposOf(s.eng.Store())
		}
		matched := filterInsights(snap.Insights, args.Explainer, args.Repo, args.MinConfidence, len(repos) > 1, moduleRepos)
		if len(matched) == 0 {
			return textResult(noMatchInsightsMessage(args.Explainer, args.Repo, args.MinConfidence, snap, s.eng.Store())), nil, nil
		}

		switch resolveOutputMode(args.OutputMode, modeSummary) {
		case modeFull:
			return jsonResultCapped(matched, args.MaxTokens)
		case modeCompact:
			return textResult(capTokens(renderInsightsCompact(matched), args.MaxTokens, false)), nil, nil
		default:
			return textResult(capTokens(renderInsightsSummary(matched), args.MaxTokens, false)), nil, nil
		}
	})
}

type queryInsightsArgs struct {
	Explainer     string  `json:"explainer,omitempty" jsonschema:"Filter to insights produced by this explainer. One of: cycles, layers, crossrepo, coverage, unused-routes, messaging-coverage, god-class, hotspots, dependency-depth, exported-surface, complexity-outliers, intent, constraints, domain, query-loops, entry-points, dead-methods, vendored-candidates, import-closure, package-metrics, dead-code, performance, packwerk. Empty = all."`
	Repo          string  `json:"repo,omitempty" jsonschema:"Repo label. In multi-repo snapshots it matches the repo path segment of each insight's evidence ('golf' matches golf/... but not golf-ui/...), or a finding whose evidence names only that repo's modules (a cycle); in single-repo snapshots it is a substring match. Empty = all."`
	MinConfidence float64 `json:"min_confidence,omitempty" jsonschema:"Minimum confidence (0.0-1.0). Default 0. unused-routes findings are emitted at 0.6, as review candidates."`
	OutputMode    string  `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT, one row per insight: explainer, confidence, title), 'compact' (adds description, evidence sample, actions), or 'full' (complete JSON)."`
	MaxTokens     int     `json:"max_tokens,omitempty" jsonschema:"Approximate token cap; output is truncated with a notice. Default: no cap."`
}

type coverageReportArgs struct {
	Repo       string `json:"repo,omitempty" jsonschema:"One service (repo label). Default: all services."`
	OutputMode string `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT, markdown table) or 'full' (JSON)."`
}

// insightsFor returns the architectural insights (cycles, layer violations,
// cross-repo, etc.) computed at snapshot time whose title or evidence references
// focus. It lets the live tools surface what the explainers already found instead
// of recomputing it. Returns nil when no snapshot or no matching insights.
func (s *Server) insightsFor(focus string) []facts.Insight {
	if s.eng == nil || focus == "" {
		return nil
	}
	snap := s.eng.Snapshot()
	if snap == nil || len(snap.Insights) == 0 {
		return nil
	}
	var out []facts.Insight
	for _, in := range snap.Insights {
		if strings.Contains(in.Title, focus) || strings.Contains(in.Description, focus) {
			out = append(out, in)
			continue
		}
		for _, ev := range in.Evidence {
			if (ev.Fact != "" && strings.Contains(ev.Fact, focus)) ||
				(ev.File != "" && strings.Contains(ev.File, focus)) ||
				(ev.Symbol != "" && strings.Contains(ev.Symbol, focus)) {
				out = append(out, in)
				break
			}
		}
	}
	return out
}

// writeInsightList appends a compact bullet list of insights (title + one-line
// description) under the given heading. No-op when insights is empty.
func writeInsightList(sb *strings.Builder, heading string, insights []facts.Insight) {
	if len(insights) == 0 {
		return
	}
	fmt.Fprintf(sb, "## %s (%d)\n\n", heading, len(insights))
	for _, in := range insights {
		fmt.Fprintf(sb, "- **%s** — %s\n", in.Title, in.Description)
	}
	sb.WriteString("\n")
}
