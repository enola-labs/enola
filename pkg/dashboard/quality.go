package dashboard

import (
	"sort"

	"github.com/enola-labs/enola/internal/facts"
)

// coverageRow is one service's cross-repo coverage, backing the Services / Coverage
// gaps cards, with coverage_report's columns. Outbound is the count of resolved
// outbound (depends_on) edges; the other counts are summed across the service's
// edge_coverage entries; Status is facts.CoverageOf's class.
type coverageRow struct {
	Service    string
	Outbound   int
	Detected   int
	Resolved   int
	Unresolved int
	Outside    int
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

// coverageDetails enumerates cross-repo coverage from the live store: one row per
// service node (with its edge_coverage tallies and classification) and the list of
// unmatched client routes (the constituents of the unresolved-edges count). A nil
// store yields empty lists, so the cards fall back to plain numbers.
func coverageDetails(store *facts.Store) (services []coverageRow, unresolved []routeRow) {
	if store == nil {
		return nil, nil
	}

	for _, svc := range store.ByKind(facts.KindService) {
		c := facts.CoverageOf(svc)
		services = append(services, coverageRow{
			Service:    svc.Name,
			Outbound:   c.Outbound,
			Detected:   c.Detected,
			Resolved:   c.Resolved,
			Unresolved: c.Unresolved,
			Outside:    c.Outside,
			External:   c.External,
			Status:     c.Class(),
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
