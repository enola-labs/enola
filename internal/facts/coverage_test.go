package facts

import "testing"

func TestClassifyService(t *testing.T) {
	tests := []struct {
		name                               string
		outbound, detected, resolved, open int
		want                               string
	}{
		{"resolved edge, everything covered", 1, 4, 4, 0, ServiceConnected},
		{"resolved edge plus unresolved is partial coverage, not a gap", 1, 4, 3, 1, ServiceConnected},
		{"many unresolved still connected while one edge resolved", 1, 1219, 877, 342, ServiceConnected},
		{"nothing resolved but call sites detected", 0, 3, 0, 3, ServiceCoverageGap},
		{"resolved only to its own routes is not a gap", 0, 157, 133, 24, ServiceConnected},
		{"genuine leaf", 0, 0, 0, 0, ServiceIsolated},
		{"external-only call sites are expected, not a blind spot", 0, 3, 0, 0, ServiceIsolated},
		// The linker derives unresolved as detected-resolved-external-declared-outside,
		// so open>0 with detected==0 cannot arise from it. A hand-authored or truncated
		// fact can still produce it; classify it isolated rather than inventing a gap.
		{"unresolved without detected", 0, 0, 0, 2, ServiceIsolated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyService(tt.outbound, tt.detected, tt.resolved, tt.open); got != tt.want {
				t.Errorf("ClassifyService(%d, %d, %d, %d) = %q, want %q",
					tt.outbound, tt.detected, tt.resolved, tt.open, got, tt.want)
			}
		})
	}
}

func TestDependsOnCount(t *testing.T) {
	svc := Fact{
		Kind: KindService,
		Name: "svc",
		Relations: []Relation{
			{Kind: RelDependsOn, Target: "a"},
			{Kind: RelCalls, Target: "b"},
			{Kind: RelDependsOn, Target: "c"},
			{Kind: RelImports, Target: "d"},
		},
	}
	if got := DependsOnCount(svc); got != 2 {
		t.Errorf("DependsOnCount = %d, want 2 (only depends_on relations)", got)
	}
	if got := DependsOnCount(Fact{Kind: KindService, Name: "leaf"}); got != 0 {
		t.Errorf("DependsOnCount(no relations) = %d, want 0", got)
	}
}

// coverageNode builds a service node whose edge_coverage has the shape a
// facts.jsonl round-trip leaves behind: []any of map[string]any with float64 values.
func coverageNode(name string, dependsOn int, entries ...map[string]any) Fact {
	svc := Fact{Kind: KindService, Name: name, Repo: name}
	raw := make([]any, 0, len(entries))
	for _, e := range entries {
		raw = append(raw, e)
	}
	svc.Props = map[string]any{"edge_coverage": raw}
	for i := 0; i < dependsOn; i++ {
		svc.Relations = append(svc.Relations, Relation{Kind: RelDependsOn, Target: "dep"})
	}
	return svc
}

func TestCoverageOf_AllOutsideServiceIsAGap(t *testing.T) {
	// Every call names a service the snapshot does not hold, so unresolved is 0 at
	// emit time. The calls are still outbound calls nothing resolved.
	svc := coverageNode("widget-client", 0, map[string]any{
		"edge_type": "http_client", "detected": 3.0, "resolved": 0.0, "unresolved": 0.0,
		"external": 0.0, "declared": 0.0, "outside": 3.0,
	})
	c := CoverageOf(svc)
	if c.Detected != 3 || c.Outside != 3 || c.Unresolved != 0 {
		t.Fatalf("CoverageOf sums = %+v, want detected 3, outside 3, unresolved 0", c)
	}
	if got := c.Class(); got != ServiceCoverageGap {
		t.Errorf("Class() = %q, want %q", got, ServiceCoverageGap)
	}
}

func TestCoverageOf_SelfResolvedServiceIsConnected(t *testing.T) {
	// A call matched to the caller's own repo counts resolved but emits no edge.
	svc := coverageNode("self-svc", 0, map[string]any{
		"edge_type": "http_client", "detected": 1.0, "resolved": 1.0, "unresolved": 0.0,
	})
	c := CoverageOf(svc)
	if c.Outbound != 0 || c.Resolved != 1 {
		t.Fatalf("CoverageOf = %+v, want outbound 0, resolved 1", c)
	}
	if got := c.Class(); got != ServiceConnected {
		t.Errorf("Class() = %q, want %q", got, ServiceConnected)
	}
}

func TestCoverageOf_ExternalAndDeclaredOnlyStayIsolated(t *testing.T) {
	svc := coverageNode("api", 0,
		map[string]any{"edge_type": "http_client", "detected": 6, "external": 4, "declared": 2},
	)
	c := CoverageOf(svc)
	if c.External != 4 || c.Declared != 2 {
		t.Fatalf("CoverageOf = %+v, want external 4, declared 2", c)
	}
	if got := c.Class(); got != ServiceIsolated {
		t.Errorf("Class() = %q, want %q", got, ServiceIsolated)
	}
}

func TestCoverageOf_SumsEveryEdgeType(t *testing.T) {
	svc := coverageNode("svc", 1,
		map[string]any{"edge_type": "http_client", "detected": 4.0, "resolved": 2.0, "unresolved": 2.0},
		map[string]any{"edge_type": "kafka", "detected": 3.0, "resolved": 2.0, "outside": 1.0},
	)
	c := CoverageOf(svc)
	if len(c.Entries) != 2 || c.Entries[1].EdgeType != "kafka" || c.Entries[1].Outside != 1 {
		t.Fatalf("Entries = %+v, want http_client then kafka with outside 1", c.Entries)
	}
	if c.Outbound != 1 || c.Detected != 7 || c.Resolved != 4 || c.Unresolved != 2 || c.Outside != 1 {
		t.Errorf("CoverageOf = %+v, want outbound 1, detected 7, resolved 4, unresolved 2, outside 1", c)
	}
	if got := c.Class(); got != ServiceConnected {
		t.Errorf("Class() = %q, want %q", got, ServiceConnected)
	}
}
