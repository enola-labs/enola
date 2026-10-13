package facts

import "testing"

func TestClassifyService(t *testing.T) {
	tests := []struct {
		name                                     string
		outbound, detected, resolved, unresolved int
		want                                     string
	}{
		{"resolved edge, everything covered", 1, 4, 4, 0, ServiceConnected},
		{"resolved edge plus unresolved is partial coverage, not a gap", 1, 4, 3, 1, ServiceConnected},
		{"many unresolved still connected while one edge resolved", 1, 1219, 877, 342, ServiceConnected},
		{"nothing resolved but call sites detected", 0, 3, 0, 3, ServiceCoverageGap},
		{"resolved only to its own routes is not a gap", 0, 157, 133, 24, ServiceConnected},
		{"genuine leaf", 0, 0, 0, 0, ServiceIsolated},
		{"external-only call sites are expected, not a blind spot", 0, 3, 0, 0, ServiceIsolated},
		// unresolved is derived as detected-resolved-external, so unresolved>0 with
		// detected==0 cannot arise from the linker. A hand-authored or truncated fact
		// can still produce it; classify it isolated rather than inventing a gap.
		{"unresolved without detected", 0, 0, 0, 2, ServiceIsolated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyService(tt.outbound, tt.detected, tt.resolved, tt.unresolved); got != tt.want {
				t.Errorf("ClassifyService(%d, %d, %d, %d) = %q, want %q",
					tt.outbound, tt.detected, tt.resolved, tt.unresolved, got, tt.want)
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
