// Package cli renders what an enola binary prints about itself: the `--list`
// tool catalogue and the `--help` text.
//
// It is a public package rather than internal/ so that anything wanting only the
// help or the catalogue can reach them without pulling in the engine.
package cli

import (
	"fmt"
	"strings"
)

// ToolEntry is one row of the `--list` catalogue: a tool name and a one-line
// summary. The summaries here are deliberately terminal-sized; the MCP tool
// descriptions registered in internal/server are multi-paragraph agent prompts
// and unusable in a list.
type ToolEntry struct {
	Name        string
	Description string
}

// OSSTools returns the catalogue of tools the engine's MCP server registers.
//
// This list must stay in step with Server.registerTools in internal/server —
// TestToolCatalogueMatchesRegisteredTools enforces that, so adding a tool
// without cataloguing it fails the build.
func OSSTools() []ToolEntry {
	return []ToolEntry{
		{Name: "generate_snapshot", Description: "Index a repository and extract its architecture as queryable facts."},
		{Name: "explore", Description: "Primary exploration tool — use this first after generate_snapshot."},
		{Name: "query_facts", Description: "Precision filter over extracted facts."},
		{Name: "show_symbol", Description: "Return the source code implementation of a named symbol."},
		{Name: "traverse", Description: "Walk the dependency/call graph from a starting node."},
		{Name: "find_path", Description: "Find the shortest path between two nodes in the architectural graph."},
		{Name: "impact_analysis", Description: "Compute the blast radius of changing a target node."},
		{Name: "endpoint_impact", Description: "Who calls one HTTP endpoint, and which screens sit behind them — the cross-repo question the route facts already answer."},
		{Name: "governing_intent", Description: "Which knowledge pages govern this code — and which code a page governs. The reverse query, without the blast radius."},
		{Name: "constraints_for", Description: "The pre-edit contract for a file or fact — its components, the rules binding them, and current violations naming it."},
		{Name: "plan_check", Description: "Plan an intended change: governing constraints, blast radius, and — for a patch — the counterfactual constraint verdicts, before any edit exists."},
		{Name: "coverage_report", Description: "Per-service edge coverage — tell a genuinely isolated service from one whose edges could not be resolved."},
		{Name: "query_insights", Description: "Fetch the architectural findings explainers computed during generate_snapshot (cycles, god-class, unused routes, …)."},
		{Name: "set_baseline", Description: "Pin the current snapshot as the baseline for diff_snapshot."},
		{Name: "diff_snapshot", Description: "Show what changed in the architecture between the baseline snapshot and the current one."},
		{Name: "snapshot_receipt", Description: "Show the receipt for the current snapshot — a compact manifest of what the graph was generated over and how complete extraction was."},
		{Name: "compare_receipts", Description: "Compare the current snapshot's receipt against a baseline's to check they are comparable before trusting a diff."},
		// The three analyzers. Each owns a tool as well as an explainer: the tool
		// answers on demand, the explainer files findings during a snapshot.
		{Name: "package_metrics", Description: "Robert C. Martin / JDepend package metrics (Ca, Ce, instability, abstractness, distance)."},
		{Name: "find_orphans", Description: "Find unreferenced symbols (dead code) in the codebase."},
		{Name: "analyze_performance", Description: "Estimate per-function Big-O complexity and rank performance risks (nested loops, calls-in-loops/N+1, recursion)."},
		// The only two that answer about the PAST. Everything above describes the tree as
		// it is now — diff_snapshot included, which compares two nows.
		{Name: "architecture_history", Description: "Show how the architecture changed over time — one entry per recorded snapshot, with what moved since the previous one."},
		{Name: "architecture_blame", Description: "Find when something entered the architecture and when it left — \"when did this module start importing that one?\"."},
	}
}

// nameColumn is the minimum width of the tool-name column. A longer name widens
// it for every group, so the descriptions stay in one line.
const nameColumn = 22

// RenderToolList renders the `--list` tool catalogue.
func RenderToolList() string {
	var b strings.Builder
	tools := OSSTools()
	b.WriteString("Available tools:\n\n")
	writeEntries(&b, tools, nameWidth(tools))
	return b.String()
}

// nameWidth returns the tool-name column width for the given entries.
func nameWidth(entries []ToolEntry) int {
	width := nameColumn
	for _, t := range entries {
		if len(t.Name) > width {
			width = len(t.Name)
		}
	}
	return width
}

// writeEntries renders one group of catalogue rows at the given name width.
func writeEntries(b *strings.Builder, entries []ToolEntry, width int) {
	for _, t := range entries {
		fmt.Fprintf(b, "  %-*s  %s\n", width, t.Name, t.Description)
	}
}
