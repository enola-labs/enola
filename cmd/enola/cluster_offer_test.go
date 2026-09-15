package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
)

func folderOfRepos(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(dir, n, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestOfferCluster_EnterWritesTheConfigAndRunsWithIt(t *testing.T) {
	dir := folderOfRepos(t, "api", "web")
	var out bytes.Buffer
	path, err := offerCluster(strings.NewReader("\n"), &out, dir, []string{"api", "web"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "cluster.yaml"); path != want {
		t.Fatalf("path = %q, want %q\n%s", path, want, out.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("written config must load: %v", err)
	}
	if paths, _ := cfg.RepoPaths(); len(paths) != 2 {
		t.Errorf("RepoPaths = %v, want the two repositories", paths)
	}
}

// Declining, or a closed stdin, goes on as one repository and writes nothing.
func TestOfferCluster_DeclineWritesNothing(t *testing.T) {
	for _, answer := range []string{"n\n", "no\n", "later\n", ""} {
		dir := folderOfRepos(t, "api", "web")
		var out bytes.Buffer
		path, err := offerCluster(strings.NewReader(answer), &out, dir, []string{"api", "web"})
		if err != nil || path != "" {
			t.Errorf("answer %q: path = %q, err = %v; want a single-repository run", answer, path, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "cluster.yaml")); err == nil {
			t.Errorf("answer %q wrote a cluster config", answer)
		}
	}
}

func TestOfferCluster_ExistingConfigIsUsedNotRewritten(t *testing.T) {
	dir := folderOfRepos(t, "api", "web")
	existing := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(existing, []byte("repos:\n  - api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	path, err := offerCluster(strings.NewReader("y\n"), &out, dir, []string{"api", "web"})
	if err != nil || path != existing {
		t.Fatalf("path = %q, err = %v; want the existing config", path, err)
	}
	if body, _ := os.ReadFile(existing); string(body) != "repos:\n  - api\n" {
		t.Errorf("existing config was rewritten:\n%s", body)
	}
}

func TestFoldedRepos_ClusterConfigSaysNothing(t *testing.T) {
	dir := folderOfRepos(t, "api", "web")
	if _, repos := foldedRepos(&config.Config{Repo: dir}); len(repos) != 2 {
		t.Errorf("a folder of two repositories as repo: must be reported, got %v", repos)
	}
	if _, repos := foldedRepos(&config.Config{Repo: dir, Repos: []string{"api", "web"}}); repos != nil {
		t.Errorf("a config with repos: already names a cluster, got %v", repos)
	}
}
