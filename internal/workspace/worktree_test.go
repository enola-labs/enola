package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A worktree an agent harness creates inside the repository is a linked worktree; the
// repository itself, a plain directory and a nested clone are not.
func TestIsLinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	git(t, root, "init", "-q")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-qm", "init")
	nested := filepath.Join(root, ".claude", "worktrees", "feat")
	git(t, root, "worktree", "add", "-q", nested, "-b", "feat")
	sibling := filepath.Join(t.TempDir(), "sibling")
	git(t, root, "worktree", "add", "-q", sibling, "-b", "sib")
	clone := filepath.Join(root, "vendor-clone")
	git(t, root, "clone", "-q", root, clone)

	for _, c := range []struct {
		dir  string
		want bool
	}{
		{nested, true},
		{sibling, true},
		{root, false},
		{clone, false},
		{filepath.Join(root, ".claude"), false},
	} {
		if got := IsLinkedWorktree(c.dir); got != c.want {
			t.Errorf("IsLinkedWorktree(%s) = %v, want %v", c.dir, got, c.want)
		}
	}
}

// A submodule's .git is also a file, but it names a directory under .git/modules with
// no commondir, so a submodule stays part of the repository that contains it.
func TestIsLinkedWorktree_SubmoduleIsNot(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, ".git/modules/lib", "lib")
	writeFile(t, filepath.Join(root, "lib", ".git"), "gitdir: ../.git/modules/lib\n")
	if IsLinkedWorktree(filepath.Join(root, "lib")) {
		t.Error("a submodule was reported as a linked worktree")
	}

	// The same layout with git's worktree marker is one, relative gitdir and all.
	mkdirs(t, root, ".git/worktrees/wt", "wt")
	writeFile(t, filepath.Join(root, ".git", "worktrees", "wt", "commondir"), "../..\n")
	writeFile(t, filepath.Join(root, "wt", ".git"), "gitdir: ../.git/worktrees/wt\n")
	if !IsLinkedWorktree(filepath.Join(root, "wt")) {
		t.Error("a worktree with a relative gitdir was not recognised")
	}

	// A .git file that does not point anywhere is not a worktree.
	mkdirs(t, root, "broken")
	if err := os.WriteFile(filepath.Join(root, "broken", ".git"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if IsLinkedWorktree(filepath.Join(root, "broken")) {
		t.Error("a malformed .git file was reported as a linked worktree")
	}
}
