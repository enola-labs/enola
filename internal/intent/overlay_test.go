package intent

import (
	"reflect"
	"testing"
)

func TestOverlayKeepsWhatTheDraftDoesNotDeclare(t *testing.T) {
	base := &Declaration{
		Layers:     []Layer{{Name: "web", Paths: []string{"web/**"}}, {Name: "storage", Paths: []string{"storage/**"}}},
		Components: []ConstraintComponent{{Name: "web", Match: []string{"web/**"}}},
		Rules:      []ConstraintRule{{ID: "old", Forbid: "web", To: "web", Via: "imports", Because: "before"}},
		Source:     RepoFileName,
	}
	draft := &Declaration{
		Components: []ConstraintComponent{{Name: "storage", Match: []string{"storage/**"}}, {Name: "web", Match: []string{"app/web/**"}}},
		Rules:      []ConstraintRule{{ID: "old", Forbid: "storage", To: "web", Via: "imports", Because: "after"}, {ID: "new", Forbid: "web", To: "storage", Via: "calls", Because: "x"}},
		Source:     "draft.yaml",
	}
	got, rep := Overlay(base, draft)

	if !reflect.DeepEqual(got.Layers, base.Layers) {
		t.Errorf("a draft without layers dropped the declared order: %+v", got.Layers)
	}
	if len(got.Components) != 2 || got.Components[0].Match[0] != "app/web/**" {
		t.Errorf("components = %+v, want web replaced in place and storage added", got.Components)
	}
	if len(got.Rules) != 2 || got.Rules[0].Because != "after" || got.Rules[1].ID != "new" {
		t.Errorf("rules = %+v, want old replaced in place and new added", got.Rules)
	}
	want := OverlayReport{AddedComponents: []string{"storage"}, ReplacedComponents: []string{"web"}, AddedRules: []string{"new"}, ReplacedRules: []string{"old"}}
	if !reflect.DeepEqual(rep, want) {
		t.Errorf("report = %+v, want %+v", rep, want)
	}
	if base.Components[0].Match[0] != "web/**" || base.Rules[0].Because != "before" {
		t.Error("Overlay modified the base declaration")
	}
}

func TestOverlayDraftLayersReplaceTheOrder(t *testing.T) {
	base := &Declaration{Layers: []Layer{{Name: "a", Paths: []string{"a/**"}}}}
	draft := &Declaration{Layers: []Layer{{Name: "b", Paths: []string{"b/**"}}}}
	got, rep := Overlay(base, draft)
	if len(got.Layers) != 1 || got.Layers[0].Name != "b" {
		t.Errorf("layers = %+v, want the draft's order", got.Layers)
	}
	if !reflect.DeepEqual(rep.Replaced, []string{"layers"}) {
		t.Errorf("replaced = %v, want [layers]", rep.Replaced)
	}
}

func TestOverlayWithoutBase(t *testing.T) {
	draft := &Declaration{Rules: []ConstraintRule{{ID: "r"}}, Source: "d.yaml"}
	got, rep := Overlay(nil, draft)
	if len(got.Rules) != 1 || got.Source != "d.yaml" || len(rep.AddedRules) != 1 {
		t.Errorf("got %+v, report %+v", got, rep)
	}
}
