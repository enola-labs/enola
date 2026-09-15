package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generate_snapshot on a folder of repositories indexes it as one repository, as it
// always did, but leads with that and with the per-repository append remedy. A normal
// repository and an append into a store say nothing about folding.
func TestE2E_FolderOfReposLeadsWithTheFold(t *testing.T) {
	s := startInMemory(t)

	parent := t.TempDir()
	for _, fixture := range []string{"go_sample", "ts_sample"} {
		repo := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", fixture), parent)
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	res := s.call(t, "generate_snapshot", map[string]any{"repo_path": parent})
	if res.IsError {
		t.Fatalf("generate_snapshot(parent) errored: %s", text(res))
	}
	out := text(res)
	if !strings.HasPrefix(out, "⚠️") || !strings.Contains(out, "holds 2 git repositories (go_sample, ts_sample)") || !strings.Contains(out, "append=true") {
		t.Errorf("a folder of repositories must lead with the fold and the append remedy; got:\n%s", out)
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
