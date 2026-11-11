package crossrepo

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestExplain_SummarizesCrossRepoEdges(t *testing.T) {
	s := facts.NewStore()
	s.Add(
		facts.Fact{
			Kind: facts.KindDependency, Name: "svc-alpha -> svc-beta", Repo: "svc-alpha",
			Props: map[string]any{
				"type": "cross_repo", "synthetic": "crossrepo",
				"via": []string{"http"}, "endpoint_count": 3,
			},
		},
		facts.Fact{
			Kind: facts.KindDependency, Name: "app-web-app -> app-web", Repo: "app-web-app",
			Props: map[string]any{
				"type": "cross_repo", "synthetic": "crossrepo",
				"via": []string{"import"}, "import_count": 7,
			},
		},
		// A non-cross-repo dependency must be ignored.
		facts.Fact{Kind: facts.KindDependency, Name: "a -> b", Repo: "svc-alpha"},
	)

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want 1", len(insights))
	}
	ins := insights[0]
	if !strings.Contains(ins.Title, "2 edges") {
		t.Errorf("title = %q, want it to mention 2 edges", ins.Title)
	}
	if len(ins.Evidence) != 2 {
		t.Errorf("evidence count = %d, want 2", len(ins.Evidence))
	}
	if !strings.Contains(ins.Description, "svc-alpha -> svc-beta") ||
		!strings.Contains(ins.Description, "3 endpoint") {
		t.Errorf("description missing expected detail: %q", ins.Description)
	}
}

func TestExplain_NoCrossRepoFactsReturnsNil(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "m"})
	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}
	if insights != nil {
		t.Errorf("insights = %+v, want nil", insights)
	}
}

// propInt tolerates the float64 form produced by a JSON round-trip.
func TestPropInt_JSONRoundTripForm(t *testing.T) {
	d := facts.Fact{Props: map[string]any{"endpoint_count": float64(5)}}
	if got := propInt(d, "endpoint_count"); got != 5 {
		t.Errorf("propInt(float64 5) = %d, want 5", got)
	}
}

// TestExplain_ViaJSONLRoundTripForm guards BUG-4: a store reloaded from
// facts.jsonl decodes the "via" array as []any, not []string. edgeDetail used to
// type-assert []string only, so the "via ..." clause silently vanished on a
// reloaded snapshot. Both int (float64) and slice ([]any) props must survive.
func TestExplain_ViaJSONLRoundTripForm(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{
		Kind: facts.KindDependency, Name: "svc-a -> svc-b", Repo: "svc-a",
		Props: map[string]any{
			"type": "cross_repo",
			"via":  []any{"http", "import"}, // JSONL round-trip shape
			// endpoint_count as float64, the JSON number shape.
			"endpoint_count": float64(4),
		},
	})

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want 1", len(insights))
	}
	desc := insights[0].Description
	if !strings.Contains(desc, "via http+import") {
		t.Errorf("description missing 'via http+import' (dropped on JSONL form): %q", desc)
	}
	if !strings.Contains(desc, "4 endpoint") {
		t.Errorf("description missing '4 endpoint(s)' from float64 count: %q", desc)
	}
}

// TestExplain_DeterministicSortedByName: edges added out of name order must be
// reported sorted, so insights.json is stable.
func TestExplain_DeterministicSortedByName(t *testing.T) {
	s := facts.NewStore()
	s.Add(
		facts.Fact{Kind: facts.KindDependency, Name: "z-svc -> y-svc", Props: map[string]any{"type": "cross_repo", "via": []string{"http"}}},
		facts.Fact{Kind: facts.KindDependency, Name: "a-svc -> b-svc", Props: map[string]any{"type": "cross_repo", "via": []string{"http"}}},
		facts.Fact{Kind: facts.KindDependency, Name: "m-svc -> n-svc", Props: map[string]any{"type": "cross_repo", "via": []string{"http"}}},
	)
	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	var order []string
	for _, ev := range insights[0].Evidence {
		order = append(order, ev.Fact)
	}
	want := []string{"a-svc -> b-svc", "m-svc -> n-svc", "z-svc -> y-svc"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("evidence order = %v, want sorted %v", order, want)
	}
}

// TestExplain_AllZeroCountsFallback: a cross_repo edge with no via/counts falls
// back to the generic "cross-repo dependency" detail rather than an empty string.
func TestExplain_AllZeroCountsFallback(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindDependency, Name: "svc-a -> svc-b", Props: map[string]any{"type": "cross_repo"}})
	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(insights[0].Description, "cross-repo dependency") {
		t.Errorf("expected generic fallback detail, got %q", insights[0].Description)
	}
}

func TestExplain_SharedCodeIsSeparateHedgedInsight(t *testing.T) {
	s := facts.NewStore()
	s.Add(
		facts.Fact{
			Kind: facts.KindDependency, Name: "svc-alpha -> svc-beta", Repo: "svc-alpha",
			Props: map[string]any{
				"type": facts.TypeCrossRepo, "synthetic": "crossrepo",
				"via": []string{"http"}, "endpoint_count": 3,
			},
		},
		facts.Fact{
			Kind: facts.KindDependency, Name: "fork-a <-> fork-b", Repo: "fork-a",
			Props: map[string]any{
				"type": facts.TypeCrossRepoSharedCode, "synthetic": "crossrepo",
				"via": []string{"shared_symbols"}, "repos": []string{"fork-a", "fork-b"},
				"symbol_count": 39,
			},
		},
	)

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}
	if len(insights) != 2 {
		t.Fatalf("insights = %d, want 2 (dependencies + shared code)", len(insights))
	}

	// The dependency insight must count ONLY real edges: a shared-code pair is not one.
	dep := insights[0]
	if !strings.Contains(dep.Title, "1 edges") {
		t.Errorf("dependency title = %q, want it to count only the 1 real edge", dep.Title)
	}
	if strings.Contains(dep.Description, "fork-a") {
		t.Errorf("shared-code pair leaked into the dependency insight: %q", dep.Description)
	}

	shared := insights[1]
	if !strings.Contains(shared.Title, "1 pair(s)") {
		t.Errorf("shared-code title = %q, want it to mention 1 pair", shared.Title)
	}
	if !strings.Contains(shared.Description, "39 shared type(s)") {
		t.Errorf("shared-code description should carry the symbol count: %q", shared.Description)
	}
	// Without name_match_count nothing was verified, so the wording must stay cautious
	// and the confidence low.
	if !strings.Contains(shared.Description, "do not prove") {
		t.Errorf("unverified shared code must keep the cautious caveat: %q", shared.Description)
	}
	if shared.Confidence != 0.5 {
		t.Errorf("unverified confidence = %v, want 0.5", shared.Confidence)
	}
	// The wording must state plainly that this is not a dependency and not traversable,
	// since that is the whole reason it is reported separately.
	for _, want := range []string{"NOT a dependency", "no graph edge", "find_path"} {
		if !strings.Contains(shared.Description, want) {
			t.Errorf("shared-code description missing %q: %q", want, shared.Description)
		}
	}
	if shared.Confidence >= dep.Confidence {
		t.Errorf("shared-code confidence (%v) must be lower than dependency confidence (%v): names alone cannot confirm shared code",
			shared.Confidence, dep.Confidence)
	}
}

func TestExplain_NoSharedCodeInsightWhenNonePresent(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{
		Kind: facts.KindDependency, Name: "svc-alpha -> svc-beta", Repo: "svc-alpha",
		Props: map[string]any{
			"type": facts.TypeCrossRepo, "synthetic": "crossrepo", "via": []string{"http"},
		},
	})
	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want 1 (no shared-code pairs, so no second insight)", len(insights))
	}
}

// TestExplain_SharedCodeVerifiedRaisesConfidence pins the difference between a count
// inferred from names and one measured against the files: when the linker recorded a
// name_match_count larger than the verified symbol_count, the insight must say so and
// carry higher confidence.
func TestExplain_SharedCodeVerifiedRaisesConfidence(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{
		Kind: facts.KindDependency, Name: "fork-a <-> fork-b", Repo: "fork-a",
		Props: map[string]any{
			"type": facts.TypeCrossRepoSharedCode, "synthetic": "crossrepo",
			"via": []string{"shared_symbols"}, "repos": []string{"fork-a", "fork-b"},
			"symbol_count": 11, "name_match_count": 39,
		},
	})

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want 1", len(insights))
	}
	got := insights[0]
	if !strings.Contains(got.Description, "11 of 39 shared type name(s) backed by near-identical files") {
		t.Errorf("verified description should report both counts: %q", got.Description)
	}
	if got.Confidence != 0.8 {
		t.Errorf("verified confidence = %v, want 0.8 (measured, not inferred)", got.Confidence)
	}
	if strings.Contains(got.Description, "do not prove") {
		t.Errorf("verified insight must drop the name-only caveat: %q", got.Description)
	}
}

// The shared-code insight names the shared types and cites where each repo declares
// them, so the copies can be compared without searching for them. Where a repo
// declares a name twice, the copy at the same path in the other repo is the one cited.
func TestExplain_SharedCodeNamesTheTypesAndTheirFiles(t *testing.T) {
	s := facts.NewStore()
	s.Add(
		facts.Fact{
			Kind: facts.KindDependency, Name: "fork-a <-> fork-b", Repo: "fork-a",
			Props: map[string]any{
				"type": facts.TypeCrossRepoSharedCode, "synthetic": "crossrepo",
				"via": []string{"shared_symbols"}, "repos": []string{"fork-a", "fork-b"},
				"symbol_count": 3, "name_match_count": 5,
				"symbol_samples": []any{"Ledger", "Shelf"},
			},
		},
		facts.Fact{Kind: facts.KindSymbol, Name: "example.com/fork-a/internal/shelf.Shelf", Repo: "fork-a",
			File: "fork-a/internal/shelf/shelf.go", Line: 7, Props: map[string]any{"symbol_kind": facts.SymbolStruct}},
		facts.Fact{Kind: facts.KindSymbol, Name: "example.com/fork-b/internal/admin.Shelf", Repo: "fork-b",
			File: "fork-b/internal/admin/admin.go", Line: 3, Props: map[string]any{"symbol_kind": facts.SymbolStruct}},
		facts.Fact{Kind: facts.KindSymbol, Name: "example.com/fork-b/internal/shelf.Shelf", Repo: "fork-b",
			File: "fork-b/internal/shelf/shelf.go", Line: 9, Props: map[string]any{"symbol_kind": facts.SymbolStruct}},
		facts.Fact{Kind: facts.KindSymbol, Name: "example.com/fork-a/internal/ledger.Ledger", Repo: "fork-a",
			File: "fork-a/internal/ledger/ledger.go", Line: 4, Props: map[string]any{"symbol_kind": facts.SymbolInterface}},
		facts.Fact{Kind: facts.KindSymbol, Name: "example.com/fork-b/internal/books.Ledger", Repo: "fork-b",
			File: "fork-b/internal/books/ledger.go", Line: 12, Props: map[string]any{"symbol_kind": facts.SymbolInterface}},
		// A function of a shared name is not a type and is not cited.
		facts.Fact{Kind: facts.KindSymbol, Name: "example.com/fork-a/internal/shelf.Ledger", Repo: "fork-a",
			File: "fork-a/internal/shelf/ledger.go", Line: 1, Props: map[string]any{"symbol_kind": facts.SymbolFunc}},
	)

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want 1", len(insights))
	}
	got := insights[0]
	if !strings.Contains(got.Description, "3 of 5 shared type name(s) backed by near-identical files: Ledger, Shelf and 1 more") {
		t.Errorf("description should name the shared types: %q", got.Description)
	}
	want := map[string]facts.Evidence{
		"example.com/fork-a/internal/ledger.Ledger": {File: "fork-a/internal/ledger/ledger.go", Line: 4,
			Detail: "fork-b also declares Ledger at fork-b/internal/books/ledger.go:12"},
		"example.com/fork-a/internal/shelf.Shelf": {File: "fork-a/internal/shelf/shelf.go", Line: 7,
			Detail: "fork-b also declares Shelf at fork-b/internal/shelf/shelf.go:9"},
	}
	cited := 0
	for _, ev := range got.Evidence {
		if ev.Symbol == "" {
			continue
		}
		cited++
		w, ok := want[ev.Symbol]
		if !ok {
			t.Errorf("unexpected evidence %+v", ev)
			continue
		}
		if ev.File != w.File || ev.Line != w.Line || ev.Detail != w.Detail {
			t.Errorf("evidence for %s = %s:%d %q, want %s:%d %q", ev.Symbol, ev.File, ev.Line, ev.Detail, w.File, w.Line, w.Detail)
		}
	}
	if cited != len(want) {
		t.Errorf("cited %d types, want %d: %+v", cited, len(want), got.Evidence)
	}
}
