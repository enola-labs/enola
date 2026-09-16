// Package mcputil holds the shared MCP tool-output helpers used by enola's own
// server and by tools outside this module: result builders, the
// summary→compact→full output-mode ladder, and a token cap. Centralizing them
// keeps every tool on one verbosity model and output shape.
package mcputil

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Output mode names. Every tool that varies its verbosity accepts a subset so the
// caller has one mental model: summary (smallest) → compact → full (largest).
const (
	ModeSummary = "summary"
	ModeCompact = "compact"
	ModeFull    = "full"
)

// ErrorResult returns msg as an error tool result.
func ErrorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}
}

// TextResult returns markdown/plain text as a (non-error) tool result.
func TextResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: s}},
	}
}

// JSONResult marshals v as indented JSON into a tool result, returning an error
// result if marshaling fails.
func JSONResult(v any) (*mcp.CallToolResult, any, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to marshal results: %v", err)), nil, nil
	}
	return TextResult(string(data)), nil, nil
}

// JSONResultCapped marshals v as indented JSON and applies a max_tokens cap.
// Truncating JSON breaks its validity, so the cap notice says so explicitly.
func JSONResultCapped(v any, maxTokens int) (*mcp.CallToolResult, any, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to marshal results: %v", err)), nil, nil
	}
	return TextResult(CapTokens(string(data), maxTokens, true)), nil, nil
}

// ResolveOutputMode normalises a caller-supplied output_mode, falling back to def
// when empty. Unknown values are returned lowercased so the per-tool switch can
// decide how to treat them (generally: fall through to def).
func ResolveOutputMode(mode, def string) string {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		return def
	}
	return m
}

// approxTokensPerChar is the rough characters-per-token ratio used to translate a
// max_tokens budget into a character budget. English/code averages ~4 chars per
// token; this is an estimate, not an exact count.
const approxTokensPerChar = 4

// CapTokens truncates s to roughly maxTokens tokens (~chars/4), cutting on a line
// boundary and appending a notice that tells the caller how to narrow. maxTokens
// <= 0 disables the cap. isJSON marks output whose structure truncation breaks
// (full mode), so the notice makes that explicit.
func CapTokens(s string, maxTokens int, isJSON bool) string {
	if maxTokens <= 0 {
		return s
	}
	limit := maxTokens * approxTokensPerChar
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	// Prefer cutting on the last newline so we don't truncate mid-line.
	if nl := strings.LastIndexByte(cut, '\n'); nl > 0 {
		cut = cut[:nl]
	}
	notice := fmt.Sprintf("\n\n[truncated: output exceeded max_tokens=%d. "+
		"Re-run with output_mode=summary, tighter filters, or a lower max_depth/max_nodes/limit.]", maxTokens)
	if isJSON {
		notice = fmt.Sprintf("\n\n[truncated: JSON output exceeded max_tokens=%d and is no longer valid JSON. "+
			"Re-run with output_mode=summary/compact, or raise max_tokens / narrow the query.]", maxTokens)
	}
	return cut + notice
}

// IsGeneratedPath reports whether a repo-relative path is build/generated/vendored
// output or a machine-generated source file that should not be reported as source.
// Conservative: exact path-segment matches and explicit, unambiguous filename
// suffixes only, so legitimately-named packages are never hidden.
func IsGeneratedPath(p string) bool {
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "build", "Pods", "node_modules", "kspCaches", "generated", "gen",
			"__pycache__", "vendor", "openapi-gen", "__generated__", "third_party",
			// Python virtual environments and installed dependencies — never source.
			".venv", "venv", "site-packages":
			return true
		}
	}
	base := p
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	// Explicit code-generation / bundle suffixes. Kept unambiguous (a two-part
	// suffix or a well-known tool marker) so a hand-written file is never matched:
	// `.gen.ts` is emitted by openapi/graphql codegen, `.pb.go`/`_pb2.py` by
	// protoc, `.min.js` is a minified bundle whose loop nesting is meaningless.
	for _, suf := range []string{
		".gen.ts", ".gen.tsx", ".gen.js", ".gen.go", ".generated.ts", ".generated.go",
		".min.js", ".min.css", ".pb.go", ".pb.gw.go", ".pb.rs", ".g.dart",
		"_pb2.py", "_pb2_grpc.py",
	} {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	return false
}
