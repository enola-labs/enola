package flowsjson

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

func render(t *testing.T, ff ...facts.Fact) (facts.Artifact, map[string]any) {
	t.Helper()
	store := facts.NewStore()
	store.Add(ff...)
	var r plugin.Renderer = New()
	sr, ok := r.(plugin.StoreRenderer)
	if !ok {
		t.Fatal("the flows renderer must read the store")
	}
	artifacts, err := sr.RenderStore(context.Background(), &facts.Snapshot{}, store)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("artifacts = %v, err = %v", artifacts, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(artifacts[0].Content, &doc); err != nil {
		t.Fatalf("flows.json must be one JSON document: %v\n%s", err, artifacts[0].Content)
	}
	return artifacts[0], doc
}

func TestRenderStore_WritesTheRankedFlows(t *testing.T) {
	fn := func(name string, calls ...string) facts.Fact {
		f := facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "shop", File: "shop/app.py", Line: 1,
			Props: map[string]any{"symbol_kind": facts.SymbolFunc}}
		for _, c := range calls {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: c})
		}
		return f
	}
	artifact, doc := render(t, fn("shop/app.main", "shop/app.serve"), fn("shop/app.serve"))
	if artifact.Name != ArtifactName {
		t.Errorf("artifact name = %q", artifact.Name)
	}
	flows, _ := doc["flows"].([]any)
	if len(flows) != 1 || doc["total"] != float64(1) {
		t.Fatalf("flows = %v, total = %v", flows, doc["total"])
	}
	flow := flows[0].(map[string]any)
	if flow["name"] != "shop/app.main" || flow["mechanism"] != "main" || flow["summary"] == "" {
		t.Errorf("flow = %v", flow)
	}
	if stages, _ := flow["stages"].([]any); len(stages) != 1 {
		t.Errorf("the listed flow must carry its stages, got %v", flow["stages"])
	}
}

// A consumer iterates flows and reads roots; neither may be null on the
// repositories that have nothing to list.
func TestRenderStore_NoFlowsIsAnEmptyList(t *testing.T) {
	_, doc := render(t, facts.Fact{Kind: facts.KindModule, Name: "shop", Repo: "shop"})
	if flows, ok := doc["flows"].([]any); !ok || len(flows) != 0 {
		t.Errorf("flows = %#v, want an empty array", doc["flows"])
	}
	if roots, ok := doc["roots"].(map[string]any); !ok || len(roots) != 0 {
		t.Errorf("roots = %#v, want an empty object", doc["roots"])
	}
}

// Without the store there is no index to walk by, so the plain path writes
// nothing rather than a file that looks like a repository with no flows.
func TestRender_WithoutTheStoreWritesNothing(t *testing.T) {
	artifacts, err := New().Render(context.Background(), &facts.Snapshot{})
	if err != nil || artifacts != nil {
		t.Errorf("artifacts = %v, err = %v", artifacts, err)
	}
}
