package server_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type setPathAnswer struct {
	Note           string   `json:"note"`
	From           string   `json:"from"`
	To             string   `json:"to"`
	Found          bool     `json:"found"`
	Path           []any    `json:"path"`
	UnresolvedFrom []string `json:"unresolved_from"`
	UnresolvedTo   []string `json:"unresolved_to"`
}

func TestFindPath_SetsAnswerWithOneSearch(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	var single, set setPathAnswer
	if err := json.Unmarshal([]byte(text(s.call(t, "find_path", map[string]any{"from": "..main", "to": "pkg/b.Beta"}))), &single); err != nil {
		t.Fatal(err)
	}
	res := s.call(t, "find_path", map[string]any{
		"from_nodes": []string{"pkg/b.Beta", "..main", "no.such.Source"},
		"to_nodes":   []string{"pkg/b.Beta", "no.such.Target"},
	})
	if res.IsError {
		t.Fatalf("set form errored: %s", text(res))
	}
	if err := json.Unmarshal([]byte(text(res)), &set); err != nil {
		t.Fatal(err)
	}
	if !set.Found || set.From != "pkg/b.Beta" || set.To != "pkg/b.Beta" || len(set.Path) != 1 {
		t.Fatalf("got %+v, want the source that is already a target, as FindPath answers a node to itself", set)
	}
	if !reflect.DeepEqual(set.UnresolvedFrom, []string{"no.such.Source"}) || !reflect.DeepEqual(set.UnresolvedTo, []string{"no.such.Target"}) {
		t.Fatalf("unresolved from=%v to=%v, want the unknown names listed", set.UnresolvedFrom, set.UnresolvedTo)
	}

	res = s.call(t, "find_path", map[string]any{"from_nodes": []string{"..main"}, "to_nodes": []string{"pkg/b.Beta"}})
	set = setPathAnswer{}
	if err := json.Unmarshal([]byte(text(res)), &set); err != nil {
		t.Fatal(err)
	}
	if !single.Found || !set.Found || len(set.Path) != len(single.Path) {
		t.Fatalf("a set of one found a %d-node path, the single name %d", len(set.Path), len(single.Path))
	}

	res = s.call(t, "find_path", map[string]any{"from_nodes": []string{"pkg/b.Beta"}, "to_nodes": []string{"..main"}})
	set = setPathAnswer{}
	if err := json.Unmarshal([]byte(text(res)), &set); err != nil {
		t.Fatal(err)
	}
	if set.Found || !strings.Contains(set.Note, "no path within 10 hops") {
		t.Fatalf("got %+v, want not found with a note naming the depth searched", set)
	}
}

func TestFindPath_SetsRefuseToMixWithSingleNames(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)
	for _, args := range []map[string]any{
		{"from": "..main", "from_nodes": []string{"..main"}, "to_nodes": []string{"pkg/b.Beta"}},
		{"from_nodes": []string{"..main"}, "to": "pkg/b.Beta"},
		{"from_nodes": []string{"..main"}},
		{"to_nodes": []string{"pkg/b.Beta"}},
	} {
		if res := s.call(t, "find_path", args); !res.IsError {
			t.Errorf("%v answered %s, want a refusal", args, text(res))
		}
	}
}
