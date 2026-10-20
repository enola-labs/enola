package server_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findPathCalls are the shapes find_path was asked before it took sets: a
// resolved pair, a pair with no route, an ambiguous name, a name that resolves
// to nothing, a relation filter and a depth.
var findPathCalls = []map[string]any{
	{"from": "..main", "to": "pkg/a.Alpha"},
	{"from": "pkg/b.Beta", "to": "..main"},
	{"from": "main", "to": "Beta"},
	{"from": "a", "to": "Beta"},
	{"from": "Alpha", "to": "nosuchthing"},
	{"from": "..main", "to": "pkg/b.Beta", "relation_kinds": []string{"calls"}},
	{"from": "..main", "to": "pkg/b.Beta", "max_depth": 1},
}

// Every find_path call that could be made before from_nodes and to_nodes
// existed answers byte for byte as it did then. The golden was written from
// the server before the two arguments were added.
func TestFindPath_EveryEarlierCallAnswersAsBefore(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)
	var got bytes.Buffer
	for _, args := range findPathCalls {
		call, _ := json.Marshal(args)
		res := s.call(t, "find_path", args)
		got.WriteString("find_path " + string(call) + "\n")
		if res.IsError {
			got.WriteString("error: ")
		}
		got.WriteString(strings.ReplaceAll(text(res), s.repo, "<repo>"))
		got.WriteString("\n\n")
	}
	golden := filepath.Join("testdata", "find_path_before_sets.golden")
	if *update {
		if err := os.WriteFile(golden, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("find_path moved for a call that names no set:\n--- got\n%s\n--- want\n%s", got.Bytes(), want)
	}
}
