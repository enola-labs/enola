package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runEnola(t *testing.T, bin, home, workDir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = workDir
	cmd.Env = sandboxEnv(home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("enola %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func folderOfGovernedRepos(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	for _, name := range []string{"api", "web"} {
		repo := writeGovernedRepo(t, filepath.Join(parent, name), name+"-rule")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return parent
}

// A folder of repositories given to --generate, without a terminal, is indexed as a
// cluster: it says so, writes the cluster config, and a second run reads that config.
// --no-cluster indexes the folder as one repository and says nothing about folding.
func TestGenerate_FolderOfRepos_IsIndexedAsACluster(t *testing.T) {
	bin := enolaBinary(t)
	home := t.TempDir()
	parent := folderOfGovernedRepos(t)
	cluster := filepath.Join(parent, "cluster.yaml")

	out := runEnola(t, bin, home, t.TempDir(), "--generate", parent)
	for _, want := range []string{"Detected 2 git repositories in " + parent + ": api, web", "Wrote " + cluster, "Repositories: 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("--generate on a folder of repositories must say %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Warning:") {
		t.Errorf("a cluster run must not warn about folding; got:\n%s", out)
	}
	if _, err := os.Stat(cluster); err != nil {
		t.Fatalf("cluster config not written: %v", err)
	}

	out = runEnola(t, bin, home, t.TempDir(), "--generate", parent)
	if !strings.Contains(out, "already there: "+cluster) || !strings.Contains(out, "Repositories: 2") {
		t.Errorf("a second run must index the cluster from the config already there; got:\n%s", out)
	}

	out = runEnola(t, bin, home, t.TempDir(), "--generate", "--no-cluster", parent)
	if strings.Contains(out, "Repositories: 2") || strings.Contains(out, "Detected") || strings.Contains(out, "Warning:") {
		t.Errorf("--no-cluster must index one repository quietly; got:\n%s", out)
	}
}

// A folder that is itself a git repository keeps its nested checkouts: it is indexed as
// one repository, with the warning naming the command that makes a cluster.
func TestGenerate_RepositoryWithNestedRepos_Warns(t *testing.T) {
	bin := enolaBinary(t)
	home := t.TempDir()
	parent := folderOfGovernedRepos(t)
	if err := os.MkdirAll(filepath.Join(parent, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := runEnola(t, bin, home, t.TempDir(), "--generate", parent)
	for _, want := range []string{"holds 2 git repositories: api, web", "enola cluster init " + parent, "Warning:"} {
		if !strings.Contains(out, want) {
			t.Errorf("--generate on a repository with nested repositories must say %q; got:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, "cluster.yaml")); err == nil {
		t.Error("a repository with nested repositories must not get a cluster config written")
	}
}
