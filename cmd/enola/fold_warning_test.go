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

// A folder of repositories given to --generate without a terminal still snapshots, as it
// always did, but says it folded them and names the command that does not. Following
// that command gives a cluster, and the cluster run says nothing about folding.
func TestGenerate_FolderOfRepos_WarnsThenClusterInitLinksThem(t *testing.T) {
	bin := enolaBinary(t)
	home := t.TempDir()
	parent := t.TempDir()
	for _, name := range []string{"api", "web"} {
		repo := writeGovernedRepo(t, filepath.Join(parent, name), name+"-rule")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	out := runEnola(t, bin, home, t.TempDir(), "--generate", parent)
	for _, want := range []string{"holds 2 git repositories: api, web", "enola cluster init " + parent, "Warning:"} {
		if !strings.Contains(out, want) {
			t.Errorf("--generate on a folder of repositories must say %q; got:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, "cluster.yaml")); err == nil {
		t.Fatal("without a terminal nothing may be written on the user's behalf")
	}

	runEnola(t, bin, home, t.TempDir(), "cluster", "init", parent)
	out = runEnola(t, bin, home, t.TempDir(), "--generate", filepath.Join(parent, "cluster.yaml"))
	if !strings.Contains(out, "Repositories: 2") {
		t.Errorf("the written config must index both repositories; got:\n%s", out)
	}
	if strings.Contains(out, "git repositories:") {
		t.Errorf("a cluster run must not warn about folding; got:\n%s", out)
	}
}
