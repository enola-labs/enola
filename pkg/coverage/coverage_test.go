package coverage

import (
	"reflect"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestBuild_Classifications(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		// connected: has a resolved outbound edge.
		facts.Fact{Kind: facts.KindService, Name: "svc-connected", Repo: "svc-connected",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "svc-beta"}},
			Props: map[string]any{"edge_coverage": []map[string]any{
				{"edge_type": "http_client", "detected": 3, "resolved": 2, "unresolved": 1},
			}}},
		// coverage_gap: no outbound edges but unresolved call sites detected.
		facts.Fact{Kind: facts.KindService, Name: "svc-gap", Repo: "svc-gap",
			Props: map[string]any{"edge_coverage": []map[string]any{
				{"edge_type": "http_client", "detected": 5, "resolved": 0, "unresolved": 5},
			}}},
		// isolated: no outbound edges, no detected call sites.
		facts.Fact{Kind: facts.KindService, Name: "svc-leaf", Repo: "svc-leaf"},
	)

	report := Build(store, "")
	got := map[string]string{}
	for _, sc := range report {
		got[sc.Service] = sc.Classification
	}
	want := map[string]string{
		"svc-connected": "connected",
		"svc-gap":       "coverage_gap",
		"svc-leaf":      "isolated",
	}
	for svc, w := range want {
		if got[svc] != w {
			t.Errorf("%s classified %q, want %q", svc, got[svc], w)
		}
	}

	// repo filter limits the report to one service.
	one := Build(store, "svc-gap")
	if len(one) != 1 || one[0].Service != "svc-gap" || one[0].UnresolvedTotal != 5 {
		t.Errorf("repo-filtered report = %+v, want only svc-gap with 5 unresolved", one)
	}
}

func TestReadEdgeCoverage_JSONRoundTripShape(t *testing.T) {
	// After a facts.jsonl round-trip the list is []any of map[string]any with
	// float64 numbers; the reader must tolerate that.
	svc := facts.Fact{Kind: facts.KindService, Name: "svc", Props: map[string]any{
		"edge_coverage": []any{
			map[string]any{"edge_type": "http_client", "detected": float64(4), "resolved": float64(1), "unresolved": float64(3)},
		},
	}}
	cov := readEdgeCoverage(svc)
	if len(cov) != 1 || cov[0].Detected != 4 || cov[0].Resolved != 1 || cov[0].Unresolved != 3 {
		t.Errorf("readEdgeCoverage = %+v, want detected 4 resolved 1 unresolved 3", cov)
	}
}

// coverage_report carries each service's open call sites by unmatched_reason, in the
// JSON and in the markdown, read through the same count the receipt uses.
func TestBuild_UnresolvedReasons(t *testing.T) {
	call := func(reason string) facts.Fact {
		return facts.Fact{Kind: facts.KindRoute, Name: "/api/" + reason, Repo: "web", Props: map[string]any{
			facts.PropRole: facts.RoleClient, facts.PropUnmatchedByServer: true, facts.PropUnmatchedReason: reason,
		}}
	}
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindService, Name: "web", Repo: "web",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "api"}},
			Props: map[string]any{"edge_coverage": []map[string]any{
				{"edge_type": "http_client", "detected": 6, "resolved": 3, "unresolved": 3},
			}}},
		facts.Fact{Kind: facts.KindService, Name: "api", Repo: "api"},
		call("path_unknown"), call("path_unknown"), call("method_mismatch"),
	)
	report := Build(store, "")
	var web Service
	for _, s := range report {
		if s.Service == "web" {
			web = s
		}
	}
	want := []facts.ReasonCount{{Reason: "path_unknown", Count: 2}, {Reason: "method_mismatch", Count: 1}}
	if !reflect.DeepEqual(web.UnresolvedReasons, want) {
		t.Errorf("web UnresolvedReasons = %+v, want %+v", web.UnresolvedReasons, want)
	}
	md := report.RenderMarkdown()
	if !strings.Contains(md, "web: `path_unknown` 2, `method_mismatch` 1") {
		t.Errorf("markdown lacks the reason breakdown:\n%s", md)
	}
}
