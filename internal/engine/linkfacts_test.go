package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A cluster's repositories share one facts.jsonl through links. A later write to one
// of them replaces its file and leaves the others as they were.
func TestLinkedFactsSurviveAWriteToOneRepository(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "a", "facts.jsonl"), filepath.Join(dir, "b", "facts.jsonl")
	for _, p := range []string{first, second} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	union := facts.NewStore()
	union.Add(facts.Fact{Kind: facts.KindModule, Name: "shared"})
	if _, err := writeFactsJSONL(union, first); err != nil {
		t.Fatal(err)
	}
	if err := linkOrCopyFile(first, second); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(first)
	b, _ := os.Stat(second)
	if !os.SameFile(a, b) {
		t.Log("filesystem gave a copy, not a link")
	}
	before, _ := os.ReadFile(first)

	alone := facts.NewStore()
	alone.Add(facts.Fact{Kind: facts.KindModule, Name: "only-b"})
	if _, err := writeFactsJSONL(alone, second); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(first); string(after) != string(before) {
		t.Errorf("writing b's facts changed a's:\n%s", after)
	}
	if got, _ := os.ReadFile(second); string(got) == string(before) {
		t.Error("b's write did not land")
	}
}
