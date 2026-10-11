package engine_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/explainers/cycles"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
	"github.com/enola-labs/enola/pkg/bootstrap"
	pkghistory "github.com/enola-labs/enola/pkg/history"
)

// A cluster walked with linking deferred on every turn but the last ends in
// the same union as one linked on every turn: the same facts, the same
// insights, the same repo paths. What the deferral removes is work nobody
// reads, never a result.
func TestDeferLinking_ClusterEndsInTheSameUnion(t *testing.T) {
	repoA, repoB, repoC := freezeTestRepo(t), freezeTestRepo(t), freezeTestRepo(t)
	repos := []string{repoA, repoB, repoC}

	walk := func(deferred bool) ([]string, int, int) {
		eng := freezeTestEngine(t)
		for i, repo := range repos {
			eng.SetDeferLinking(deferred && i < len(repos)-1)
			snap, err := eng.GenerateSnapshot(context.Background(), repo, i > 0)
			if err != nil {
				t.Fatalf("generate %d: %v", i, err)
			}
			if deferred && i < len(repos)-1 && len(snap.Insights) != 0 {
				t.Fatalf("turn %d published insights while deferred", i)
			}
		}
		snap := eng.Snapshot()
		names := make([]string, 0, eng.Store().Count())
		for _, f := range eng.Store().All() {
			names = append(names, f.Repo+"/"+f.Kind+"/"+f.Name)
		}
		return names, len(snap.Insights), len(eng.RepoPaths())
	}

	eagerNames, eagerInsights, eagerRepos := walk(false)
	lazyNames, lazyInsights, lazyRepos := walk(true)
	if len(eagerNames) != len(lazyNames) || eagerInsights != lazyInsights || eagerRepos != lazyRepos {
		t.Fatalf("deferred walk differs: facts %d vs %d, insights %d vs %d, repos %d vs %d",
			len(eagerNames), len(lazyNames), eagerInsights, lazyInsights, eagerRepos, lazyRepos)
	}
	for i := range eagerNames {
		if eagerNames[i] != lazyNames[i] {
			t.Fatalf("fact %d differs: %q vs %q", i, eagerNames[i], lazyNames[i])
		}
	}
}

// A cluster writes its one union to every repository's output dir. The second
// dir gets byte-identical facts.jsonl under the same digest, and a history store
// those dirs share records the revision once, not once per dir.
func TestWriteArtifacts_SameBundleToManyDirsWritesOnceAndRecordsOnce(t *testing.T) {
	repoA, repoB := freezeTestRepo(t), freezeTestRepo(t)
	historyRoot := t.TempDir()

	cfg := config.Default()
	cfg.History.Dir = historyRoot
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap.RecordHistory(eng, cfg)
	eng.RegisterExtractor(goextractor.New())
	eng.RegisterExplainer(cycles.New())

	eng.SetDeferLinking(true)
	if _, err := eng.GenerateSnapshot(context.Background(), repoA, false); err != nil {
		t.Fatal(err)
	}
	eng.SetDeferLinking(false)
	if _, err := eng.GenerateSnapshot(context.Background(), repoB, true); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{repoA, repoB} {
		if err := eng.WriteArtifacts(repo); err != nil {
			t.Fatalf("write %s: %v", repo, err)
		}
	}

	factsA, err := os.ReadFile(filepath.Join(repoA, cfg.Output.Dir, "facts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	factsB, err := os.ReadFile(filepath.Join(repoB, cfg.Output.Dir, "facts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(factsA) == 0 || !bytes.Equal(factsA, factsB) {
		t.Fatalf("facts.jsonl differs between dirs (%d vs %d bytes)", len(factsA), len(factsB))
	}

	entries, err := pkghistory.Read(historyRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("shared history has %d revisions after one cluster write, want 1", len(entries))
	}
}
