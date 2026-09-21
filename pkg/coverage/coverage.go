// Package coverage reports which cross-repo edges enola resolved and which it did
// not, per service.
//
// It exists so the same answer reaches an agent and a person. The report was
// originally computed inside the MCP server, which meant only an agent could ask —
// and someone deciding whether to trust enola's cross-repo linking, the person who
// most needs it, had no way to see it. A command-line report must not have to import
// a stdio JSON-RPC server to reach its own data, so the logic lives here and both
// surfaces call it.
//
// The unresolved counts are the point of the report, not a footnote. Cross-repo
// linking is the claim hardest to verify from the outside, and a report that showed
// only what resolved would be advertising. Showing the misses is what makes the
// resolved number mean anything — and it is what tells a genuinely isolated service
// apart from one whose edges enola simply could not follow.
package coverage

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// EdgeCoverage is the detected/resolved/unresolved tally for one edge type on one
// service.
type EdgeCoverage struct {
	EdgeType   string `json:"edge_type"`
	Detected   int    `json:"detected"`
	Resolved   int    `json:"resolved"`
	Unresolved int    `json:"unresolved"`
	External   int    `json:"external,omitempty"`
}

// Service is the coverage picture for one service node.
type Service struct {
	Service        string `json:"service"`
	Classification string `json:"classification"` // connected | coverage_gap | isolated

	OutboundEdges   int            `json:"outbound_edges"`
	EdgeCoverage    []EdgeCoverage `json:"edge_coverage,omitempty"`
	UnresolvedTotal int            `json:"unresolved_total"`
	ExternalTotal   int            `json:"external_total,omitempty"`
}

// Detected and Resolved sum this service's per-edge-type tallies.
func (s Service) Detected() int { return s.sum(func(c EdgeCoverage) int { return c.Detected }) }
func (s Service) Resolved() int { return s.sum(func(c EdgeCoverage) int { return c.Resolved }) }

func (s Service) sum(f func(EdgeCoverage) int) int {
	n := 0
	for _, c := range s.EdgeCoverage {
		n += f(c)
	}
	return n
}

// Report is the per-service coverage picture, sorted by service name so two runs over
// the same graph render identically.
type Report []Service

// Gaps counts services classified as a coverage gap: they look isolated, but enola
// detected outbound call sites it could not resolve. That combination is the one a
// reader must not mistake for "this service depends on nothing".
func (r Report) Gaps() int {
	n := 0
	for _, s := range r {
		if s.Classification == facts.ServiceCoverageGap {
			n++
		}
	}
	return n
}

// Build derives the report from the service nodes in store. A non-empty service name
// limits it to that one service.
//
// Returns nil for a single-repo snapshot, which has no service nodes at all — callers
// must say so rather than render an empty table, since running this on one repository
// is the most likely first attempt.
func Build(store *facts.Store, service string) Report {
	var out Report
	for _, svc := range store.ByKind(facts.KindService) {
		if service != "" && svc.Name != service {
			continue
		}
		outbound := facts.DependsOnCount(svc)

		cov := readEdgeCoverage(svc)
		detected, unresolved, external := 0, 0, 0
		for _, c := range cov {
			detected += c.Detected
			unresolved += c.Unresolved
			external += c.External
		}

		out = append(out, Service{
			Service:         svc.Name,
			Classification:  facts.ClassifyService(outbound, detected, unresolved),
			OutboundEdges:   outbound,
			EdgeCoverage:    cov,
			UnresolvedTotal: unresolved,
			ExternalTotal:   external,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// readEdgeCoverage extracts the edge_coverage entries from a service node's props,
// tolerating both the in-memory shape ([]map[string]any, int values) and the shape
// that survives a facts.jsonl JSON round-trip ([]any of map[string]any, float64).
func readEdgeCoverage(svc facts.Fact) []EdgeCoverage {
	if svc.Props == nil {
		return nil
	}
	var raw []map[string]any
	switch v := svc.PropAny("edge_coverage").(type) {
	case []map[string]any:
		raw = v
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				raw = append(raw, m)
			}
		}
	default:
		return nil
	}
	out := make([]EdgeCoverage, 0, len(raw))
	for _, m := range raw {
		et, _ := m["edge_type"].(string)
		out = append(out, EdgeCoverage{
			EdgeType:   et,
			Detected:   readInt(m["detected"]),
			Resolved:   readInt(m["resolved"]),
			Unresolved: readInt(m["unresolved"]),
			External:   readInt(m["external"]),
		})
	}
	return out
}

// readInt reads an int-valued prop, tolerating the float64 form that survives a JSON
// round-trip through facts.jsonl.
func readInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// RenderMarkdown is the agent-facing view: a table, with coverage gaps called out
// above it.
func (r Report) RenderMarkdown() string {
	var sb strings.Builder
	sb.WriteString("# Coverage Report\n\n")
	sb.WriteString("Distinguishes genuinely isolated services from those whose outbound edges could not be resolved.\n\n")

	if gaps := r.Gaps(); gaps > 0 {
		fmt.Fprintf(&sb, "⚠️  %d service(s) classified `coverage_gap`: they look isolated but have unresolved outbound call sites — verify against source.\n\n", gaps)
	}

	sb.WriteString("| Service | Classification | Outbound edges | Detected | Resolved | Unresolved | External |\n")
	sb.WriteString("|---|---|---|---|---|---|---|\n")
	for _, s := range r {
		fmt.Fprintf(&sb, "| %s | %s | %d | %d | %d | %d | %d |\n",
			s.Service, s.Classification, s.OutboundEdges, s.Detected(), s.Resolved(), s.UnresolvedTotal, s.ExternalTotal)
	}
	return sb.String()
}
