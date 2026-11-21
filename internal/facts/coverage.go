package facts

import "sort"

// Service coverage classifications. A service node's class answers "did enola
// resolve this repo's outbound calls, and if not, is that a blind spot or is the
// repo genuinely a leaf?"
const (
	// ServiceConnected: at least one outbound call site resolved to a loaded repo.
	// Some call sites may still be unresolved (an unloaded repo, a third-party API);
	// that is partial coverage, not a blind spot.
	ServiceConnected = "connected"
	// ServiceCoverageGap: nothing resolved, yet outbound call sites were detected.
	// The repo looks isolated but almost certainly is not — verify against source.
	ServiceCoverageGap = "coverage_gap"
	// ServiceIsolated: no resolved outbound call, and every detected call site (if
	// any) is an expected one: a third-party host (external) or a declared target.
	// Nothing is left that a missing repository or a resolution gap could explain.
	ServiceIsolated = "isolated"
)

// ClassifyService is the single definition of a service's cross-repo coverage
// class. Callers read a service node through CoverageOf and call Class, which
// feeds this; the snapshot receipt (internal/engine), coverage_report
// (pkg/coverage), the coverage explainer and the dashboard all do. They once each
// carried their own copy, and the copies drifted: the receipt counted every service
// with any unresolved call site as a gap, which saturated the metric on a healthy
// multi-repo snapshot, and later the surfaces fed different buckets in.
//
// outbound is the count of resolved outbound cross-repo dependencies (see
// DependsOnCount); detected and resolved are summed across the service's
// edge_coverage entries. open is every detected call site that is neither resolved
// nor expected: the unresolved bucket plus the outside bucket (a named target the
// snapshot does not hold). The linker derives unresolved as
// detected - resolved - external - declared - outside, so open is
// detected - resolved - external - declared.
func ClassifyService(outbound, detected, resolved, open int) string {
	if outbound > 0 || resolved > 0 {
		return ServiceConnected
	}
	// external-only and declared-only call sites are expected, not a blind spot, so
	// a service with no resolved edges and nothing open stays isolated, not a gap.
	if detected > 0 && open > 0 {
		return ServiceCoverageGap
	}
	return ServiceIsolated
}

// EdgeCoverage is one edge type's tally on a service node, as the cross-repo
// linker writes it into the edge_coverage prop.
type EdgeCoverage struct {
	EdgeType   string
	Detected   int
	Resolved   int
	Unresolved int
	External   int
	Declared   int
	Outside    int
}

// ServiceCoverage is a service node's coverage, read once: its resolved outbound
// dependencies, its per-edge-type tallies, and their sums.
type ServiceCoverage struct {
	Outbound int
	Entries  []EdgeCoverage
	EdgeCoverage
}

// Open counts the detected call sites that are neither resolved nor expected.
func (c ServiceCoverage) Open() int { return c.Unresolved + c.Outside }

// Class is the service's coverage class under ClassifyService.
func (c ServiceCoverage) Class() string {
	return ClassifyService(c.Outbound, c.Detected, c.Resolved, c.Open())
}

// CoverageOf reads a service node's edge_coverage prop and depends_on relations,
// tolerating both the in-memory shape ([]map[string]any with int values) and the
// shape a facts.jsonl round-trip leaves ([]any of map[string]any, float64 values).
func CoverageOf(svc Fact) ServiceCoverage {
	c := ServiceCoverage{Outbound: DependsOnCount(svc)}
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
	}
	for _, m := range raw {
		edgeType, _ := m["edge_type"].(string)
		e := EdgeCoverage{
			EdgeType:   edgeType,
			Detected:   coverageInt(m["detected"]),
			Resolved:   coverageInt(m["resolved"]),
			Unresolved: coverageInt(m["unresolved"]),
			External:   coverageInt(m["external"]),
			Declared:   coverageInt(m["declared"]),
			Outside:    coverageInt(m["outside"]),
		}
		c.Entries = append(c.Entries, e)
		c.Detected += e.Detected
		c.Resolved += e.Resolved
		c.Unresolved += e.Unresolved
		c.External += e.External
		c.Declared += e.Declared
		c.Outside += e.Outside
	}
	return c
}

func coverageInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// DependsOnCount returns how many resolved outbound (cross-repo) dependencies a
// service node carries.
func DependsOnCount(svc Fact) int {
	n := 0
	for _, rel := range svc.Relations {
		if rel.Kind == RelDependsOn {
			n++
		}
	}
	return n
}

// ReasonCount is how many open client call sites carry one unmatched_reason.
type ReasonCount struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// IsOpenClientCall reports whether f is a client call site the cross-repo linker
// could not resolve to a loaded server (unmatched_by_server), test doubles excluded.
// It is the one rule the per-reason counts and the per-site listings select by.
func IsOpenClientCall(f Fact) bool {
	if f.Kind != KindRoute || f.PropString(PropRole) != RoleClient {
		return false
	}
	if b, _ := f.PropAny(PropUnmatchedByServer).(bool); !b {
		return false
	}
	double, _ := f.PropAny("test_double").(bool)
	return !double
}

// UnresolvedReasons counts the open client call sites of one repository, or of every
// repository when repo is empty, by the unmatched_reason the linker recorded. The
// snapshot receipt, coverage_report and the coverage insight all read this count, so
// the breakdown one surface prints is the breakdown every surface prints.
func UnresolvedReasons(store *Store, repo string) map[string]int {
	out := map[string]int{}
	for _, f := range store.ByKind(KindRoute) {
		if repo != "" && f.Repo != repo {
			continue
		}
		if IsOpenClientCall(f) {
			out[f.PropString(PropUnmatchedReason)]++
		}
	}
	return out
}

// SortedReasons orders reason counts most first, then by name, so two renders of
// one graph agree. Empty for no counts.
func SortedReasons(counts map[string]int) []ReasonCount {
	if len(counts) == 0 {
		return nil
	}
	out := make([]ReasonCount, 0, len(counts))
	for reason, n := range counts {
		out = append(out, ReasonCount{Reason: reason, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}
