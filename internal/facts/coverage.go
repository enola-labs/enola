package facts

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
