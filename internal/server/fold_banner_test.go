package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func folderOfFixtureRepos(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	for _, fixture := range []string{"go_sample", "ts_sample"} {
		repo := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", fixture), parent)
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return parent
}

// generate_snapshot on a folder of repositories indexes them as a cluster in one call,
// writes the cluster config, and leads with what it did. no_cluster=true indexes the
// folder as one repository and leads with the fold instead.
func TestE2E_FolderOfReposIsIndexedAsACluster(t *testing.T) {
	s := startInMemory(t)
	parent := folderOfFixtureRepos(t)

	res := s.call(t, "generate_snapshot", map[string]any{"repo_path": parent})
	if res.IsError {
		t.Fatalf("generate_snapshot(parent) errored: %s", text(res))
	}
	out := text(res)
	for _, want := range []string{
		"holds 2 git repositories (go_sample, ts_sample), indexed as a cluster",
		"Wrote " + filepath.Join(parent, "cluster.yaml"),
		"Repositories indexed: go_sample, ts_sample",
		"Multi-repo mode active",
		"no_cluster=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cluster answer omits %q; got:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, "cluster.yaml")); err != nil {
		t.Errorf("cluster config not written: %v", err)
	}

	out = text(s.call(t, "generate_snapshot", map[string]any{"repo_path": parent}))
	if !strings.Contains(out, "cluster config already there") {
		t.Errorf("a second call must read the existing cluster config; got:\n%s", out)
	}

	out = text(s.call(t, "generate_snapshot", map[string]any{"repo_path": parent, "no_cluster": true, "fresh": true}))
	if !strings.HasPrefix(out, "⚠️") || !strings.Contains(out, "holds 2 git repositories (go_sample, ts_sample)") || strings.Contains(out, "indexed as a cluster") {
		t.Errorf("no_cluster must index one repository and lead with the fold; got:\n%s", out)
	}
}

// A folder that is itself a git repository keeps its nested checkouts, so it is indexed
// as one repository and warned about, and a normal repository or an append into a store
// says nothing about folding.
func TestE2E_RepositoryWithNestedReposLeadsWithTheFold(t *testing.T) {
	s := startInMemory(t)
	parent := folderOfFixtureRepos(t)
	if err := os.MkdirAll(filepath.Join(parent, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := text(s.call(t, "generate_snapshot", map[string]any{"repo_path": parent}))
	if !strings.HasPrefix(out, "⚠️") || !strings.Contains(out, "holds 2 git repositories (go_sample, ts_sample)") || !strings.Contains(out, "repo_paths=[") || !strings.Contains(out, filepath.Join(parent, "ts_sample")) {
		t.Errorf("a repository with nested repositories must lead with the fold and the repo_paths call; got:\n%s", out)
	}

	for _, call := range []map[string]any{
		{"repo_path": s.repo, "fresh": true},
		{"repo_path": filepath.Join(parent, "ts_sample"), "append": true},
	} {
		if out := text(s.call(t, "generate_snapshot", call)); strings.Contains(out, "git repositories (") {
			t.Errorf("generate_snapshot(%v) must not warn about folding; got:\n%s", call, out)
		}
	}
}
