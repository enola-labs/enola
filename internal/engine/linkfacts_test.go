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

// previous/ holds the snapshot it was rotated from, linked rather than copied, and
// keeps it when the next snapshot is written.
func TestRotatedFactsAreLinkedAndKept(t *testing.T) {
	out := t.TempDir()
	old := facts.NewStore()
	old.Add(facts.Fact{Kind: facts.KindModule, Name: "old"})
	if _, err := writeFactsJSONL(old, filepath.Join(out, "facts.jsonl")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(out, "facts.jsonl"))
	if err := rotatePrevious(out); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(filepath.Join(out, "facts.jsonl"))
	b, err := os.Stat(filepath.Join(out, PreviousSubdir, "facts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Log("filesystem gave a copy, not a link")
	}
	fresh := facts.NewStore()
	fresh.Add(facts.Fact{Kind: facts.KindModule, Name: "new"})
	if _, err := writeFactsJSONL(fresh, filepath.Join(out, "facts.jsonl")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(out, PreviousSubdir, "facts.jsonl")); string(got) != string(before) {
		t.Errorf("previous/facts.jsonl changed with the new snapshot:\n%s", got)
	}
}
