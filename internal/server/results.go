package server

// Result construction, output modes and the small helpers every tool's renderer
// shares.

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/mcputil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// capitalize returns s with its first letter uppercased.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func errorResult(msg string) *mcp.CallToolResult { return mcputil.ErrorResult(msg) }

func jsonResult(v any) (*mcp.CallToolResult, any, error) { return mcputil.JSONResult(v) }

func textResult(s string) *mcp.CallToolResult { return mcputil.TextResult(s) }

func jsonResultCapped(v any, maxTokens int) (*mcp.CallToolResult, any, error) {
	return mcputil.JSONResultCapped(v, maxTokens)
}

// Output mode names shared across the tools. summary (smallest) → compact → full
// (largest); "names" is query_facts-only and stays local.
const (
	modeSummary = mcputil.ModeSummary
	modeCompact = mcputil.ModeCompact
	modeFull    = mcputil.ModeFull
	modeNames   = "names"
)

func resolveOutputMode(mode, def string) string { return mcputil.ResolveOutputMode(mode, def) }

// wantsFullOutput reports whether the caller asked for the raw JSON graph rather
// than a markdown summary.
func wantsFullOutput(mode string) bool {
	return strings.EqualFold(mode, modeFull)
}

// wantsSummary reports whether the caller asked for the aggregated summary view.
func wantsSummary(mode string) bool {
	return strings.EqualFold(mode, modeSummary)
}

func capTokens(s string, maxTokens int, isJSON bool) string {
	return mcputil.CapTokens(s, maxTokens, isJSON)
}

// moduleOf returns a node's owning module: its file directory when a file is
// known, otherwise the package-ish prefix of its dotted name. Used to aggregate
// traversal/impact nodes by module in the summary views.
func moduleOf(n facts.TraversalNode) string {
	if n.File != "" {
		if i := strings.LastIndexByte(n.File, '/'); i > 0 {
			return n.File[:i]
		}
		return "."
	}
	name := n.Name
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

// topCounts returns the up-to-n highest-count keys from m, sorted by count
// descending then name ascending for stable output.
func topCounts(m map[string]int, n int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	return keys
}
