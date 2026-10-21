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

type setPathFiles struct {
	setPathAnswer
	FromFiles      map[string]int    `json:"from_files"`
	ToFiles        map[string]int    `json:"to_files"`
	FromCandidates map[string]string `json:"from_candidates"`
	ToCandidates   map[string]string `json:"to_candidates"`
}

func TestFindPath_SetsTakeASourceFileAsTheSymbolsItDeclares(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	var got setPathFiles
	res := s.call(t, "find_path", map[string]any{
		"from_nodes": []string{"main.go"},
		"to_nodes":   []string{"pkg/b/b.go", "pkg/b/missing.go"},
	})
	if err := json.Unmarshal([]byte(text(res)), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.To != "pkg/b.Beta" {
		t.Fatalf("got %+v, want a path from the symbols main.go declares to pkg/b.Beta, which b.go declares", got)
	}
	if got.FromFiles["main.go"] == 0 || got.ToFiles["pkg/b/b.go"] == 0 {
		t.Errorf("from_files %v, to_files %v: want each file expanded named with its symbol count", got.FromFiles, got.ToFiles)
	}
	if !reflect.DeepEqual(got.UnresolvedTo, []string{"pkg/b/missing.go"}) {
		t.Errorf("unresolved_to %v, want only the file that declares nothing", got.UnresolvedTo)
	}
}

func TestFindPath_SetsOfferTheNearestNameForAnUnresolvedOne(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	var got setPathFiles
	res := s.call(t, "find_path", map[string]any{
		"from_nodes": []string{"..main", "b.Beta"},
		"to_nodes":   []string{"pkg/b.Beta", "no.such.Target"},
	})
	if err := json.Unmarshal([]byte(text(res)), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Found {
		t.Fatalf("got %+v, want the exact names searched while the unresolved ones are listed", got)
	}
	if got.FromCandidates["b.Beta"] != "pkg/b.Beta" {
		t.Errorf("from_candidates %v, want the nearest name for the misspelling", got.FromCandidates)
	}
	if _, guessed := got.ToCandidates["no.such.Target"]; guessed && got.ToCandidates["no.such.Target"] == "" {
		t.Errorf("to_candidates %v holds an empty suggestion", got.ToCandidates)
	}
}
