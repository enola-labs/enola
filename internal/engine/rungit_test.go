package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Reading a repository's git state takes no lock in it: the dirty check must not
// make a concurrent `git checkout` by whoever keeps the checkout current fail on
// .git/index.lock. Shown by the .git directory staying untouched, since creating and
// removing the lock changes its modification time.
func TestGitInfo_TakesNoLock(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")
	// Touch the file so the index is stale and a locking `git status` would refresh it.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(repo, "a.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(repo, ".git"), old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(filepath.Join(repo, ".git"))
	if info := gitInfo(repo, ".enola"); info == nil {
		t.Fatal("no git info")
	}
	after, _ := os.Stat(filepath.Join(repo, ".git"))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf(".git changed while its state was read: a lock was taken")
	}
}
