package dashboard

import (
	"sort"

	"github.com/enola-labs/enola/pkg/facts"
)

// coverageRow is one service's cross-repo coverage, backing the Services / Coverage
// gaps cards. Resolved is the count of resolved outbound (depends_on) edges;
// Detected/Unresolved/External are summed across the service's edge_coverage
// entries; Status classifies the node (connected | coverage_gap | isolated).
type coverageRow struct {
	Service    string
	Resolved   int
	Detected   int
	Unresolved int
	External   int
	Status     string
}

// routeRow is one unmatched client route, backing the Unresolved edges card — a
// client call site for which no loaded server route matched.
type routeRow struct {
	Method string
	Path   string
	Repo   string
	Reason string
	File   string
	Line   int
}

// Service coverage classifications — mirrors facts.ClassifyService (the OSS logic
// is internal-only, so the tiny decision is reproduced here).
const (
	svcConnected   = "connected"
	svcCoverageGap = "coverage_gap"
	svcIsolated    = "isolated"
)

// classifyService reproduces facts.ClassifyService: any resolved outbound edge →
// connected; else detected-but-unresolved call sites → coverage_gap (a blind spot);
// else a genuine leaf → isolated.
func classifyService(resolved, detected, unresolved int) string {
	if resolved > 0 {
		return svcConnected
	}
	if detected > 0 && unresolved > 0 {
		return svcCoverageGap
	}
	return svcIsolated
}

// coverageDetails enumerates cross-repo coverage from the live store: one row per
// service node (with its edge_coverage tallies and classification) and the list of
// unmatched client routes (the constituents of the unresolved-edges count). A nil
// store yields empty lists, so the cards fall back to plain numbers.
func coverageDetails(store *facts.Store) (services []coverageRow, unresolved []routeRow) {
	if store == nil {
		return nil, nil
	}

	for _, svc := range store.ByKind(facts.KindService) {
		resolved := 0
		for _, rel := range svc.Relations {
			if rel.Kind == facts.RelDependsOn {
				resolved++
			}
		}
		detected := edgeCoverageSum(svc, "detected")
		unres := edgeCoverageSum(svc, "unresolved")
		services = append(services, coverageRow{
			Service:    svc.Name,
			Resolved:   resolved,
			Detected:   detected,
			Unresolved: unres,
			External:   edgeCoverageSum(svc, "external"),
			Status:     classifyService(resolved, detected, unres),
		})
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Service < services[j].Service })

	for _, r := range store.ByKind(facts.KindRoute) {
		if !propBool(r, "unmatched_by_server") {
			continue
		}
		unresolved = append(unresolved, routeRow{
			Method: propStr(r, "method"),
			Path:   r.Name,
			Repo:   r.Repo,
			Reason: propStr(r, "unmatched_reason"),
			File:   r.File,
			Line:   r.Line,
		})
	}
	sort.Slice(unresolved, func(i, j int) bool {
		if unresolved[i].Repo != unresolved[j].Repo {
			return unresolved[i].Repo < unresolved[j].Repo
		}
		if unresolved[i].Path != unresolved[j].Path {
			return unresolved[i].Path < unresolved[j].Path
		}
		return unresolved[i].Line < unresolved[j].Line
	})

	return services, unresolved
}

// edgeCoverageSum totals a numeric field across a service node's edge_coverage prop
// (one entry per edge_type). Mirrors the engine's readCoverageField: the value may
// be []map[string]any or []any, with int (in-process) or float64 (JSON) numbers.
func edgeCoverageSum(svc facts.Fact, field string) int {
	if svc.Props == nil {
		return 0
	}
	var entries []map[string]any
	switch v := svc.PropAny("edge_coverage").(type) {
	case []map[string]any:
		entries = v
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				entries = append(entries, m)
			}
		}
	default:
		return 0
	}
	total := 0
	for _, m := range entries {
		switch n := m[field].(type) {
		case int:
			total += n
		case float64:
			total += int(n)
		}
	}
	return total
}

// propStr returns a string-valued prop, or "" when absent/non-string.
func propStr(f facts.Fact, key string) string {
	if f.Props == nil {
		return ""
	}
	if s, ok := f.PropAny(key).(string); ok {
		return s
	}
	return ""
}

// propBool returns a bool-valued prop, or false when absent/non-bool.
func propBool(f facts.Fact, key string) bool {
	if f.Props == nil {
		return false
	}
	b, ok := f.PropAny(key).(bool)
	return ok && b
}
