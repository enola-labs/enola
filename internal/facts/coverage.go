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
	// ServiceIsolated: no resolved outbound edges and nothing unresolved to explain
	// them away — genuinely a leaf.
	ServiceIsolated = "isolated"
)

// ClassifyService is the single definition of a service's cross-repo coverage
// class. The snapshot receipt (internal/engine), coverage_report (internal/server)
// and the coverage explainer all classify through here; they previously each
// carried their own copy and the receipt's had drifted, counting every service
// with any unresolved call site as a gap. Because unresolved is derived as
// detected-resolved-external, that made the metric saturate — on a healthy
// multi-repo snapshot every client has some unresolved call site, so the count
// equalled the number of services and could not signal ill health.
//
// outbound is the count of resolved outbound cross-repo dependencies (see
// DependsOnCount); detected, resolved and unresolved are summed across the
// service's edge_coverage entries.
func ClassifyService(outbound, detected, resolved, unresolved int) string {
	if outbound > 0 || resolved > 0 {
		return ServiceConnected
	}
	// external-only call sites are expected, not a blind spot, so a service with no
	// resolved edges but only external calls stays isolated, not a gap.
	if detected > 0 && unresolved > 0 {
		return ServiceCoverageGap
	}
	return ServiceIsolated
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
