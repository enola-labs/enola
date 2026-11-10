package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
	"github.com/enola-labs/enola/internal/facts"
)

// insights.json is written twice: WriteArtifacts puts it on disk, and
// GetArtifact serves the same document to the MCP server and the dashboard.
// They marshal through the same store call so a consumer cannot be handed two
// different documents — and, before fact ids existed, they did not: GetArtifact
// skipped WriteArtifacts' nil guard. These tests hold the two together and
// check that a citation reaching the file resolves to a fact that is in it.
func TestInsightsArtifact_BothPathsAgree(t *testing.T) {
	repo := t.TempDir()
	writeRepoFile(t, repo, "go.mod", "module example.com/ins\n\ngo 1.25\n")
	// A cycle, so at least one explainer has something to cite.
	writeRepoFile(t, repo, "a/a.go", "package a\n\nimport \"example.com/ins/b\"\n\nfunc A() { b.B() }\n")
	writeRepoFile(t, repo, "b/b.go", "package b\n\nimport \"example.com/ins/a\"\n\nfunc B() { a.A() }\n")

	cfg := config.Default()
	cfg.Repo = repo

	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The engine registers no extractors of its own; the CLI bootstrap does.
	e.RegisterExtractor(goextractor.New())
	if _, err := e.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	if err := e.WriteArtifacts(repo); err != nil {
		t.Fatalf("WriteArtifacts: %v", err)
	}

	onDisk, err := os.ReadFile(filepath.Join(repo, cfg.Output.Dir, "insights.json"))
	if err != nil {
		t.Fatal(err)
	}
	served, err := e.GetArtifact("insights.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(served) {
		t.Errorf("insights.json on disk and from GetArtifact differ:\n disk %d bytes\nserved %d bytes",
			len(onDisk), len(served))
	}
}

// TestInsightsArtifact_FactIDsNameFactsInTheSnapshot — an id that names nothing
// is worse than no id: it invites a consumer to link an edge to a node that does
// not exist. Every fact_id written must appear as an id in facts.jsonl.
func TestInsightsArtifact_FactIDsNameFactsInTheSnapshot(t *testing.T) {
	repo := t.TempDir()
	writeRepoFile(t, repo, "go.mod", "module example.com/ins2\n\ngo 1.25\n")
	writeRepoFile(t, repo, "a/a.go", "package a\n\nimport \"example.com/ins2/b\"\n\nfunc A() { b.B() }\n")
	writeRepoFile(t, repo, "b/b.go", "package b\n\nimport \"example.com/ins2/a\"\n\nfunc B() { a.A() }\n")

	cfg := config.Default()
	cfg.Repo = repo
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The engine registers no extractors of its own; the CLI bootstrap does.
	e.RegisterExtractor(goextractor.New())
	snap, err := e.GenerateSnapshot(context.Background(), repo, false)
	if err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	// Findings are injected rather than waited for: which explainer fires on a
	// three-file fixture is not this test's subject, and a fixture that stops
	// producing one would turn this into a test that passes by measuring nothing.
	// The citations name facts the snapshot definitely has.
	withEvidence := *snap
	withEvidence.Insights = []facts.Insight{{
		Title: "injected", Source: "test", Description: "d", Confidence: 1,
		Evidence: []facts.Evidence{
			{Symbol: "example.com/ins2/a.A", Detail: "a symbol the fixture declares"},
			{Fact: "example.com/ins2/a", Detail: "a module the fixture declares"},
			{Symbol: "NoSuchThing", Detail: "names nothing: must get no id"},
		},
	}}
	e.SetSnapshot(&withEvidence)
	if err := e.WriteArtifacts(repo); err != nil {
		t.Fatalf("WriteArtifacts: %v", err)
	}

	factsRaw, err := os.ReadFile(filepath.Join(repo, cfg.Output.Dir, "facts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(factsRaw)), "\n") {
		var f struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatal(err)
		}
		ids[f.ID] = true
	}

	insightsRaw, err := os.ReadFile(filepath.Join(repo, cfg.Output.Dir, "insights.json"))
	if err != nil {
		t.Fatal(err)
	}
	var insights []struct {
		Source   string `json:"source"`
		Evidence []struct {
			Symbol string `json:"symbol"`
			Fact   string `json:"fact"`
			FactID string `json:"fact_id"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(insightsRaw, &insights); err != nil {
		t.Fatal(err)
	}

	cited, resolved := 0, 0
	for _, in := range insights {
		for _, e := range in.Evidence {
			if e.Symbol == "" && e.Fact == "" {
				continue
			}
			cited++
			if e.FactID == "" {
				continue
			}
			resolved++
			if !ids[e.FactID] {
				t.Errorf("%s cites fact_id %q, which is not an id in facts.jsonl", in.Source, e.FactID)
			}
		}
	}
	if cited == 0 {
		t.Fatal("no evidence cited a symbol or fact: the fixture stopped exercising this")
	}
	if resolved == 0 {
		t.Errorf("%d citations and not one resolved; ids are not reaching insights.json", cited)
	}
	t.Logf("%d citations, %d resolved to a fact in the snapshot", cited, resolved)
}
