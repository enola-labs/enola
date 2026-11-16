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

// A cluster writes one union to its own directory, and each member's provenance still
// describes that member: its own path and commit, the extractors and counts of the turn
// that read it, in the cluster receipt's member entries and in ReadMemberMeta. Sharing
// the union must not share the provenance, and no member gets a receipt of the union.
func TestWriteGraph_ClusterMembersKeepTheirOwnProvenance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
	if err := eng.WriteGraph(); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{repoA, repoB} {
		if _, err := os.Stat(filepath.Join(repo, cfg.Output.Dir, "receipt.json")); err == nil {
			t.Errorf("%s got a receipt of the union", repo)
		}
	}

	dir := eng.GraphDir([]string{repoA, repoB})
	var r facts.Receipt
	data, err := os.ReadFile(filepath.Join(dir, "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	byPath := map[string]facts.MemberReceipt{}
	for _, m := range r.Members {
		byPath[m.RepoPath] = m
	}
	ra, rb := byPath[repoA], byPath[repoB]
	if ra.Git == nil || rb.Git == nil || ra.Git.Commit == rb.Git.Commit {
		t.Fatalf("member entries share a commit: %+v vs %+v", ra.Git, rb.Git)
	}
	if ra.FilesParsed == 0 || ra.FilesParsed == rb.FilesParsed {
		t.Fatalf("member entries share parse counts: %d vs %d", ra.FilesParsed, rb.FilesParsed)
	}

	a, err := engine.ReadMemberMeta(dir, repoA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := engine.ReadMemberMeta(dir, repoB)
	if err != nil {
		t.Fatal(err)
	}
	if a.RepoPath != repoA || b.RepoPath != repoB {
		t.Fatalf("member metas name the wrong repo: %q, %q", a.RepoPath, b.RepoPath)
	}
	if a.Git == nil || b.Git == nil || a.Git.Commit == b.Git.Commit {
		t.Fatalf("member metas share a commit: %+v vs %+v", a.Git, b.Git)
	}
	if len(a.FileHashes) == 0 || len(a.FileHashes) == len(b.FileHashes) {
		t.Fatalf("member metas share file hashes: %d vs %d", len(a.FileHashes), len(b.FileHashes))
	}
	if a.SnapshotID == "" || a.SnapshotID != b.SnapshotID || a.SnapshotID != r.SnapshotID {
		t.Fatalf("member metas disagree on the union: %q, %q, receipt %q", a.SnapshotID, b.SnapshotID, r.SnapshotID)
	}
	if a.FactCount != r.FactCount || a.OutputHashes["facts.jsonl"] != r.OutputHashes["facts.jsonl"] {
		t.Fatalf("member meta does not carry the union's counts and hashes")
	}
}
