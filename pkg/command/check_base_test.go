package command

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// `check --base main` grades a branch against where it left main, with no baseline
// pinned, no remote, and nothing written: the comparison every reviewing agent had to
// assemble by hand. main moving on after the branch point is not the branch's doing.
func TestCheckBaseGradesTheBranchAgainstItsMergeBase(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildEnola(t)
	repo, home := filepath.Join(t.TempDir(), "shop"), t.TempDir()
	writeFiles(t, repo, map[string]string{
		"go.mod":            "module example.com/shop\n\ngo 1.22\n",
		"web/w.go":          "package web\n\nimport \"example.com/shop/storage\"\n\nfunc Serve() int { return storage.Read() }\n",
		"storage/s.go":      "package storage\n\nfunc Read() int { return 1 }\n",
		"enola-intent.yaml": "layers:\n  - {name: web, paths: [\"web/**\"]}\n  - {name: storage, paths: [\"storage/**\"]}\n",
		".gitignore":        ".enola/\n",
	})
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")

	gitIn(t, repo, "checkout", "-q", "-b", "pr")
	writeFiles(t, repo, map[string]string{
		"storage/leak.go": "package storage\n\nimport \"example.com/shop/web\"\n\nfunc Leak() int { return web.Serve() }\n",
	})
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "storage reaches up")

	// main moves on: a package the branch never saw.
	gitIn(t, repo, "checkout", "-q", "main")
	writeFiles(t, repo, map[string]string{"billing/b.go": "package billing\n\nfunc Bill() int { return 2 }\n"})
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "billing")
	gitIn(t, repo, "checkout", "-q", "pr")

	res := runEnola(t, bin, repo, home, "check", "--base", "main", "--fail-on=layers", "--detail", repo)
	out := res.stdout + res.stderr
	if res.code != 1 {
		t.Fatalf("exit %d, want 1 for the layer the branch crossed\n%s", res.code, out)
	}
	if !strings.Contains(res.stdout, "storage") || !strings.Contains(res.stdout, "Layer violation") {
		t.Errorf("the verdict does not name the crossing:\n%s", res.stdout)
	}
	if strings.Contains(res.stdout, "billing") {
		t.Errorf("main's later commit was graded as the branch's change:\n%s", res.stdout)
	}
	if !strings.Contains(res.stderr, "where this branch left main") {
		t.Errorf("the merge-base it compared against was not named:\n%s", res.stderr)
	}

	status, err := exec.Command("git", "-C", repo, "status", "--short").Output()
	if err != nil || len(bytes.TrimSpace(status)) != 0 {
		t.Errorf("the repository was changed: %q %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".enola", "baseline")); err == nil {
		t.Error("a baseline was pinned")
	}
	if entries, _ := os.ReadDir(home); len(entries) > 0 {
		t.Errorf("--base wrote into the home directory: %s", entries[0].Name())
	}
}

func TestUntarRefusesEscapes(t *testing.T) {
	for _, name := range []string{"../out.go", "a/../../out.go"} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(t.TempDir(), "export")
		if err := untar(&buf, dest); err == nil {
			t.Errorf("%q was written outside the export", name)
		}
	}
}
