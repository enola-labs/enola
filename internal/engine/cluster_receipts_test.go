package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/explainers/cycles"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
	"github.com/enola-labs/enola/internal/facts"
)

func gitRepoWithOneCommit(t *testing.T, repo, marker string) {
	t.Helper()
	writeFile(t, filepath.Join(repo, "MARKER"), marker+"\n")
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", marker},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v (%s)", args, err, out)
		}
	}
}

func readReceipt(t *testing.T, repo, outputDir string) facts.Receipt {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, outputDir, "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r facts.Receipt
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// A cluster's fan-out writes the same union to every member dir, and each member's
// receipt still describes that member: its own path and commit, the extractors and
// counts of the turn that read it. Sharing the union must not share the provenance.
func TestWriteArtifacts_ClusterMembersKeepTheirOwnReceipts(t *testing.T) {
	repoA, repoB := freezeTestRepo(t), freezeTestRepo(t)
	gitRepoWithOneCommit(t, repoA, "a")
	gitRepoWithOneCommit(t, repoB, "b")
	writeFile(t, filepath.Join(repoB, "pkg", "c", "c.go"), "package c\n\nfunc C() {}\n")

	cfg := config.Default()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(goextractor.New())
	eng.RegisterExplainer(cycles.New())

	if _, err := eng.GenerateCluster(context.Background(), []string{repoA, repoB}, false); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{repoA, repoB} {
		if err := eng.WriteArtifacts(repo); err != nil {
			t.Fatalf("write %s: %v", repo, err)
		}
	}

	a, b := readReceipt(t, repoA, cfg.Output.Dir), readReceipt(t, repoB, cfg.Output.Dir)
	if a.RepoPath != repoA || b.RepoPath != repoB {
		t.Fatalf("receipts name the wrong repo: %q (want %q), %q (want %q)", a.RepoPath, repoA, b.RepoPath, repoB)
	}
	if a.Git == nil || b.Git == nil || a.Git.Commit == b.Git.Commit {
		t.Fatalf("receipts share a commit: %+v vs %+v", a.Git, b.Git)
	}
	if a.Quality.FilesSeen == 0 || a.Quality.FilesSeen == b.Quality.FilesSeen {
		t.Fatalf("receipts share walk counts: %d vs %d files seen", a.Quality.FilesSeen, b.Quality.FilesSeen)
	}
	if a.Quality.FilesParsed == 0 || a.Quality.FilesParsed == b.Quality.FilesParsed {
		t.Fatalf("receipts share parse counts: %d vs %d files parsed", a.Quality.FilesParsed, b.Quality.FilesParsed)
	}
	if a.SnapshotID == "" || a.SnapshotID != b.SnapshotID {
		t.Fatalf("members disagree on the union they hold: %q vs %q", a.SnapshotID, b.SnapshotID)
	}
	if a.OutputHashes["facts.jsonl"] == "" || a.OutputHashes["facts.jsonl"] != b.OutputHashes["facts.jsonl"] {
		t.Fatalf("members disagree on the facts bytes: %v vs %v", a.OutputHashes, b.OutputHashes)
	}
	if a.FactCount != b.FactCount {
		t.Fatalf("fact_count describes the dir's facts.jsonl, which is shared: %d vs %d", a.FactCount, b.FactCount)
	}
}
