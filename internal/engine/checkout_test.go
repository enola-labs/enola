package engine

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/config"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// goCheckout writes a tiny Go module into dir and commits it with the given remote.
func goCheckout(t *testing.T, dir, remote string) {
	t.Helper()
	writeTree(t, dir, map[string]string{
		"go.mod":     "module example.com/demo\n\ngo 1.22\n",
		"pkg/a/a.go": "package a\n\nimport \"example.com/demo/pkg/b\"\n\nfunc Hello() string { return b.World() }\n",
		"pkg/b/b.go": "package b\n\nfunc World() string { return \"w\" }\n",
	})
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "remote", "add", "origin", remote)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "init")
}

func TestSameRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	main := filepath.Join(base, "demo")
	goCheckout(t, main, "git@github.com:acme/demo.git")
	wt := filepath.Join(base, "demo-wt")
	gitIn(t, main, "worktree", "add", "-q", wt, "-b", "wt")
	clone := filepath.Join(base, "demo-clone")
	goCheckout(t, clone, "https://github.com/acme/demo")
	fork := filepath.Join(base, "fork", "demo")
	goCheckout(t, fork, "git@github.com:other-org/demo.git")

	for _, c := range []struct {
		a, b string
		want bool
	}{
		{main, wt, true},
		{main, clone, true},
		{main, fork, false},
		{main, t.TempDir(), false},
	} {
		if got := sameRepository(c.a, c.b); got != c.want {
			t.Errorf("sameRepository(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// An append of another checkout of the only repository loaded replaces it: the two
// carry one label, and joining them doubled every fact under it.
func TestGenerateSnapshot_AppendOfAnotherCheckoutReplaces(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	main := filepath.Join(base, "demo")
	goCheckout(t, main, "git@github.com:acme/demo.git")
	wt := filepath.Join(base, "demo-wt")
	gitIn(t, main, "worktree", "add", "-q", wt, "-b", "wt")

	eng, err := New(config.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, err := eng.GenerateSnapshot(context.Background(), main, false)
	if err != nil {
		t.Fatalf("snapshot of main: %v", err)
	}
	second, err := eng.GenerateSnapshot(context.Background(), wt, true)
	if err != nil {
		t.Fatalf("append of the worktree: %v", err)
	}
	if second.Meta.FactCount != first.Meta.FactCount {
		t.Errorf("the graph holds %d facts after appending the worktree, want the single checkout's %d", second.Meta.FactCount, first.Meta.FactCount)
	}
	if other, members := eng.LoadedCheckoutOf(main); other == "" || members != 1 {
		t.Errorf("LoadedCheckoutOf(main) = %q, %d; want the worktree, alone in the graph", other, members)
	}
}

// Two directories of one repository indexed as a cluster share its git directory and
// remote, but are different services: only checkout roots are checkouts.
func TestSameRepository_SubdirectoriesAreNot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := filepath.Join(t.TempDir(), "mono")
	writeTree(t, root, map[string]string{"api/main.go": "package main\n", "web/main.go": "package main\n"})
	goCheckout(t, root, "git@github.com:acme/mono.git")
	if sameRepository(filepath.Join(root, "api"), filepath.Join(root, "web")) {
		t.Error("two directories of one repository were treated as checkouts of it")
	}
}
