package engine

// coverageSummary rolls up per-service edge_coverage into the snapshot-level
// CoverageSummary. These tests pin that external call sites are surfaced separately
// and excluded from the internal blind-spot count (unresolved) and gap tally.

import (
	"fmt"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func svcCoverage(name string, resolved, unresolved, external int) facts.Fact {
	return facts.Fact{
		Kind: facts.KindService,
		Name: name,
		Repo: name,
		Props: map[string]any{
			"synthetic": "crossrepo",
			"edge_coverage": []map[string]any{{
				"edge_type":  "http_client",
				"detected":   resolved + unresolved + external,
				"resolved":   resolved,
				"unresolved": unresolved,
				"external":   external,
			}},
		},
	}
}

// withDeps attaches n resolved outbound cross-repo dependencies to a service node.
// Without this, every fixture has outbound == 0, which is the one input that tells
// a coverage gap apart from a merely partially-covered service.
func withDeps(f facts.Fact, n int) facts.Fact {
	for i := 0; i < n; i++ {
		f.Relations = append(f.Relations, facts.Relation{
			Kind:   facts.RelDependsOn,
			Target: fmt.Sprintf("dep-%d", i),
		})
	}
	return f
}

// A service with a resolved outbound edge is "partial coverage", which both
// coverage_report and the coverage explainer classify as connected — not a gap.
// The receipt must agree with them.
func TestCoverageSummary_PartiallyCoveredServiceIsNotAGap(t *testing.T) {
	st := facts.NewStore()
	st.Add(
		withDeps(svcCoverage("partial", 5, 2, 0), 1), // resolved edge + unresolved -> connected
		svcCoverage("gap", 0, 3, 0),                  // no resolved edge + unresolved -> gap
		withDeps(svcCoverage("clean", 4, 0, 0), 1),   // nothing unresolved -> connected
	)

	sum := coverageSummary(st)
	if sum == nil {
		t.Fatal("expected a CoverageSummary")
	}
	if sum.CoverageGaps != 1 {
		t.Errorf("CoverageGaps = %d, want 1 (only the service with no resolved outbound edge)", sum.CoverageGaps)
	}
	// UnresolvedEdges must keep accumulating across every service, gapped or not.
	if sum.UnresolvedEdges != 5 {
		t.Errorf("UnresolvedEdges = %d, want 5 (2+3, counted regardless of classification)", sum.UnresolvedEdges)
	}
}

func TestCoverageSummary_ExternalBucket(t *testing.T) {
	st := facts.NewStore()
	st.Add(
		svcCoverage("a", 0, 2, 0), // 2 internal unresolved -> a coverage gap
		svcCoverage("b", 4, 0, 3), // only external -> NOT a gap
		svcCoverage("c", 0, 1, 4), // both -> gap, external counted separately
	)

	sum := coverageSummary(st)
	if sum == nil {
		t.Fatal("expected a CoverageSummary")
	}
	if sum.ServicesTotal != 3 {
		t.Errorf("ServicesTotal = %d, want 3", sum.ServicesTotal)
	}
	if sum.CoverageGaps != 2 {
		t.Errorf("CoverageGaps = %d, want 2 (external-only service is not a gap)", sum.CoverageGaps)
	}
	if sum.UnresolvedEdges != 3 {
		t.Errorf("UnresolvedEdges = %d, want 3 (internal only: 2+1)", sum.UnresolvedEdges)
	}
	if sum.ExternalEdges != 7 {
		t.Errorf("ExternalEdges = %d, want 7 (3+4)", sum.ExternalEdges)
	}
}
