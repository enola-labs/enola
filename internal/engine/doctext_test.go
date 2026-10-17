package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/renderers/docsjsonl"
)

// A comment's full text reaches the store's side table and docs.jsonl, and never
// facts.jsonl, on a cold run, on a warm one served from the extractor cache, and for
// the other repositories of an append.
func TestDocText_LeavesTheFactsForTheSideTable(t *testing.T) {
	eng := freezeTestEngine(t)
	eng.RegisterRenderer(docsjsonl.New())
	repo := freezeTestRepo(t)
	writeFile(t, filepath.Join(repo, "pkg", "c", "c.go"),
		"package c\n\n// C places an order.\n//\n// It charges the card first.\nfunc C() {}\n")
	other := freezeTestRepo(t)
	label := filepath.Base(repo)
	key := facts.FactKey{Repo: label, Kind: facts.KindSymbol, Name: "pkg/c.C", File: "pkg/c/c.go"}
	want := "C places an order.\n\nIt charges the card first."

	for run := 0; run < 2; run++ {
		if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
			t.Fatalf("generate: %v", err)
		}
		if err := eng.WriteArtifacts(repo); err != nil {
			t.Fatalf("write artifacts: %v", err)
		}
		if got := eng.Store().DocText(key); got != want {
			t.Errorf("run %d: DocText = %q, want %q", run, got, want)
		}
		for _, f := range eng.Store().ByName("pkg/c.C") {
			if f.PropString(facts.PropDoc) != "C places an order." {
				t.Errorf("run %d: doc = %q", run, f.PropString(facts.PropDoc))
			}
			if _, has := f.Prop(facts.PropDocText); has {
				t.Errorf("run %d: the stored fact still carries doc_text", run)
			}
		}
		b, err := os.ReadFile(filepath.Join(repo, ".enola", "facts.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(facts.PropDocText)) {
			t.Errorf("run %d: facts.jsonl carries doc_text", run)
		}
		docs, err := os.ReadFile(filepath.Join(repo, ".enola", docsjsonl.ArtifactName))
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if !strings.Contains(string(docs), `"doc":"C places an order.\n\nIt charges the card first."`) {
			t.Errorf("run %d: docs.jsonl lacks the whole comment:\n%s", run, docs)
		}
		var receipt struct {
			OutputHashes map[string]string `json:"output_hashes"`
		}
		rb, err := os.ReadFile(filepath.Join(repo, ".enola", "receipt.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(rb, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.OutputHashes[docsjsonl.ArtifactName] == "" {
			t.Errorf("run %d: the receipt does not vouch for docs.jsonl: %v", run, receipt.OutputHashes)
		}
	}

	if _, err := eng.GenerateSnapshot(context.Background(), other, true); err != nil {
		t.Fatalf("append: %v", err)
	}
	appended := key
	appended.File = label + "/" + key.File
	if eng.Store().DocText(key) == "" && eng.Store().DocText(appended) == "" {
		t.Error("appending another repository dropped the first one's doc texts")
	}
}
