package coverage

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestExplain_DistinguishesGapFromPartial(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		// No outbound edges but unresolved call sites -> coverage gap.
		facts.Fact{Kind: facts.KindService, Name: "svc-gap", Repo: "svc-gap",
			Props: map[string]any{"edge_coverage": []map[string]any{
				{"edge_type": "http_client", "detected": 5, "resolved": 0, "unresolved": 5},
			}}},
		// Has an outbound edge plus some unresolved -> partial coverage.
		facts.Fact{Kind: facts.KindService, Name: "svc-partial", Repo: "svc-partial",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "svc-other"}},
			Props: map[string]any{"edge_coverage": []map[string]any{
				{"edge_type": "http_client", "detected": 4, "resolved": 3, "unresolved": 1},
			}}},
		// Fully resolved -> no insight.
		facts.Fact{Kind: facts.KindService, Name: "svc-clean", Repo: "svc-clean",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "svc-other"}},
			Props: map[string]any{"edge_coverage": []map[string]any{
				{"edge_type": "http_client", "detected": 2, "resolved": 2, "unresolved": 0},
			}}},
		// Genuine leaf with no detected call sites -> no insight.
		facts.Fact{Kind: facts.KindService, Name: "svc-leaf", Repo: "svc-leaf"},
	)

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	byService := map[string]string{}
	for _, in := range insights {
		switch {
		case strings.Contains(in.Title, "svc-gap"):
			byService["svc-gap"] = in.Title
		case strings.Contains(in.Title, "svc-partial"):
			byService["svc-partial"] = in.Title
		case strings.Contains(in.Title, "svc-clean"):
			byService["svc-clean"] = in.Title
		case strings.Contains(in.Title, "svc-leaf"):
			byService["svc-leaf"] = in.Title
		}
	}

	if !strings.HasPrefix(byService["svc-gap"], "Coverage gap") {
		t.Errorf("svc-gap insight = %q, want a 'Coverage gap' title", byService["svc-gap"])
	}
	if !strings.HasPrefix(byService["svc-partial"], "Partial coverage") {
		t.Errorf("svc-partial insight = %q, want a 'Partial coverage' title", byService["svc-partial"])
	}
	if _, ok := byService["svc-clean"]; ok {
		t.Errorf("svc-clean should produce no insight, got %q", byService["svc-clean"])
	}
	if _, ok := byService["svc-leaf"]; ok {
		t.Errorf("svc-leaf should produce no insight, got %q", byService["svc-leaf"])
	}
}

// TestExplain_JSONLRoundTripShape: a service reloaded from facts.jsonl carries
// edge_coverage as []any of map[string]any with float64 numbers. readCoverage
// must classify it identically to the in-memory []map[string]any/int form.
func TestExplain_JSONLRoundTripShape(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindService, Name: "svc-gap", Repo: "svc-gap",
		Props: map[string]any{"edge_coverage": []any{
			map[string]any{"edge_type": "http_client", "detected": float64(5), "resolved": float64(0), "unresolved": float64(5)},
		}}})

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight from JSONL-shape coverage, got %d", len(insights))
	}
	if !strings.HasPrefix(insights[0].Title, "Coverage gap") {
		t.Errorf("JSONL-shape gap misclassified: %q", insights[0].Title)
	}
	if insights[0].Confidence != 0.9 {
		t.Errorf("gap confidence = %v, want 0.9", insights[0].Confidence)
	}
}

// TestExplain_MultipleEdgeTypes: counts across edge types are summed, and the
// service is a partial (not a gap) because it has a resolved outbound edge.
func TestExplain_MultipleEdgeTypes(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindService, Name: "svc", Repo: "svc",
		Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "other"}},
		Props: map[string]any{"edge_coverage": []map[string]any{
			{"edge_type": "http_client", "detected": 4, "resolved": 3, "unresolved": 1},
			{"edge_type": "grpc_client", "detected": 3, "resolved": 1, "unresolved": 2},
		}}})

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d", len(insights))
	}
	in := insights[0]
	if !strings.HasPrefix(in.Title, "Partial coverage") {
		t.Errorf("want partial coverage (has a resolved edge), got %q", in.Title)
	}
	// 1 + 2 = 3 unresolved across the two edge types.
	if !strings.Contains(in.Title, "3 unresolved") {
		t.Errorf("summed unresolved should be 3, got %q", in.Title)
	}
	if in.Confidence != 0.75 {
		t.Errorf("partial confidence = %v, want 0.75", in.Confidence)
	}
	// Detail renders both edge types in stable insertion order.
	if !strings.Contains(in.Description, "http_client") || !strings.Contains(in.Description, "grpc_client") {
		t.Errorf("detail should mention both edge types: %q", in.Description)
	}
}

func TestExplain_NoServicesNoInsights(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindModule, Name: "internal/foo"})
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("expected no insights for single-repo snapshot, got %d", len(insights))
	}
}

// TestExplain_UnresolvedCallersReadTheLinkerVerdict: the per-component breakdown is
// evidence for the count beside it, so it must count the same call sites. It used to
// re-match client calls against server routes with its own key, which collapsed only
// a bare "{}" or a ":"-prefixed segment, so a call the linker had matched through
// "{id}" against ":id", or through a ".json" suffix, was listed as unresolved. On a
// real cluster the breakdown said 799 where the count said 126.
func TestExplain_UnresolvedCallersReadTheLinkerVerdict(t *testing.T) {
	matched := []map[string]any{{"repo": "api", "name": "/orders/:id", "method": "GET"}}
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindService, Name: "web", Repo: "web",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "api"}},
			Props: map[string]any{"edge_coverage": []map[string]any{
				{"edge_type": "http_client", "detected": 3, "resolved": 2, "unresolved": 1},
			}}},
		facts.Fact{Kind: facts.KindRoute, Name: "/orders/:id", File: "api/server.go", Repo: "api",
			Props: map[string]any{"method": "GET", "role": "server"}},
		// Resolved by the linker through a param dialect and a format suffix.
		facts.Fact{Kind: facts.KindRoute, Name: "/orders/{id}", File: "web/client/orders.go", Repo: "web",
			Props: map[string]any{"method": "GET", "role": "client", facts.PropMatchedRoutes: matched}},
		facts.Fact{Kind: facts.KindRoute, Name: "/orders/{id}.json", File: "web/client/orders.go", Repo: "web",
			Props: map[string]any{"method": "GET", "role": "client", facts.PropMatchedRoutes: matched}},
		// The one the linker could not place.
		facts.Fact{Kind: facts.KindRoute, Name: "/{}/orders", File: "web/client/tenant.go", Repo: "web",
			Props: map[string]any{"method": "GET", "role": "client",
				facts.PropUnmatchedByServer: true, "unmatched_reason": "path_unknown"}},
	)

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("want one insight for web, got %d: %+v", len(insights), insights)
	}
	var breakdown []facts.Evidence
	for _, e := range insights[0].Evidence {
		if strings.Contains(e.Detail, "from here") {
			breakdown = append(breakdown, e)
		}
	}
	if len(breakdown) != 1 || breakdown[0].Fact != "web/client" ||
		breakdown[0].Detail != "1 unresolved outbound call site(s) from here" {
		t.Errorf("breakdown = %+v, want only web/client with the one call the linker left unresolved", breakdown)
	}
}
