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
	// Outside counts call sites whose target hint names a service the snapshot does
	// not hold: a dependency on something not loaded, not a resolution gap.
	Outside int `json:"outside,omitempty"`
}

// Service is the coverage picture for one service node.
type Service struct {
	Service        string `json:"service"`
	Classification string `json:"classification"` // connected | coverage_gap | isolated

	OutboundEdges   int            `json:"outbound_edges"`
	EdgeCoverage    []EdgeCoverage `json:"edge_coverage,omitempty"`
	UnresolvedTotal int            `json:"unresolved_total"`
	ExternalTotal   int            `json:"external_total,omitempty"`
	OutsideTotal    int            `json:"outside_total,omitempty"`
	UnresolvedSites []CallSite     `json:"unresolved_sites,omitempty"`
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
		c := facts.CoverageOf(svc)
		out = append(out, Service{
			Service:         svc.Name,
			Classification:  c.Class(),
			OutboundEdges:   c.Outbound,
			EdgeCoverage:    edgeCoverage(c.Entries),
			UnresolvedTotal: c.Unresolved,
			ExternalTotal:   c.External,
			OutsideTotal:    c.Outside,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// readEdgeCoverage reads the edge_coverage entries of a service or extraction node.
func readEdgeCoverage(f facts.Fact) []EdgeCoverage {
	return edgeCoverage(facts.CoverageOf(f).Entries)
}

// edgeCoverage converts facts' per-edge-type tallies into this package's JSON shape.
func edgeCoverage(entries []facts.EdgeCoverage) []EdgeCoverage {
	if len(entries) == 0 {
		return nil
	}
	out := make([]EdgeCoverage, 0, len(entries))
	for _, e := range entries {
		out = append(out, EdgeCoverage{
			EdgeType:   e.EdgeType,
			Detected:   e.Detected,
			Resolved:   e.Resolved,
			Unresolved: e.Unresolved,
			External:   e.External,
			Outside:    e.Outside,
		})
	}
	return out
}

// gapCalls names what the coverage_gap rows hold, by bucket, so the warning is true of
// every row it counts: unresolved call sites are to be verified against source, and
// calls naming a service the snapshot does not hold are resolved by loading it.
func (r Report) gapCalls() string {
	unresolved, outside := false, false
	for _, s := range r {
		if s.Classification == facts.ServiceCoverageGap {
			unresolved = unresolved || s.UnresolvedTotal > 0
			outside = outside || s.OutsideTotal > 0
		}
	}
	const (
		u = "unresolved outbound call sites — verify against source"
		o = "outbound calls to a service that is not loaded — load it to resolve them"
	)
	switch {
	case unresolved && outside:
		return u + ", or " + o
	case outside:
		return o
	}
	return u
}

// RenderMarkdown is the agent-facing view: a table, with coverage gaps called out
// above it.
func (r Report) RenderMarkdown() string {
	var sb strings.Builder
	sb.WriteString("# Coverage Report\n\n")
	sb.WriteString("Distinguishes genuinely isolated services from those whose outbound edges could not be resolved.\n\n")

	if gaps := r.Gaps(); gaps > 0 {
		fmt.Fprintf(&sb, "⚠️  %d service(s) classified `coverage_gap`: they look isolated but have %s.\n\n", gaps, r.gapCalls())
	}

	sb.WriteString("| Service | Classification | Outbound edges | Detected | Resolved | Unresolved | Outside snapshot | External |\n")
	sb.WriteString("|---|---|---|---|---|---|---|---|\n")
	outside := false
	for _, s := range r {
		fmt.Fprintf(&sb, "| %s | %s | %d | %d | %d | %d | %d | %d |\n",
			s.Service, s.Classification, s.OutboundEdges, s.Detected(), s.Resolved(), s.UnresolvedTotal, s.OutsideTotal, s.ExternalTotal)
		outside = outside || s.OutsideTotal > 0
	}
	if outside {
		sb.WriteString("\n`Outside snapshot` counts calls whose target the source names (an env var, a client name) as a service that is not loaded: load it to resolve them.\n")
	}
	for _, s := range r {
		if s.Classification == facts.ServiceIsolated {
			sb.WriteString("\n`isolated` means no outbound edges and no detected call sites either: a genuine leaf, not a gap in enola's resolution.\n")
			break
		}
	}
	return sb.String()
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
