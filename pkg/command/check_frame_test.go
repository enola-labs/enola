package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	frameRule = `components:
  - name: personal-data
    match: ["customers/**"]
rules:
  - id: personal-data-never-reaches-the-log
    forbid: personal-data
    to_name: ["log.*"]
    via: calls
    mode: strict
    because: A subject's name in a log is a copy nobody can delete.
`
	frameBefore = `package customers

// ProfileStore holds names and addresses.
type ProfileStore struct {
	rows map[string]string
}

// Erase removes every trace of one subject.
func (s *ProfileStore) Erase(subject string) {
	delete(s.rows, subject)
}
`
	frameAfter = `package customers

import "log"

// ProfileStore holds names and addresses.
type ProfileStore struct {
	rows map[string]string
}

// Erase removes every trace of one subject.
func (s *ProfileStore) Erase(subject string) {
	log.Printf("erasing %s", subject)
	delete(s.rows, subject)
}
`
	// frameDecoy is another checkout's copy of the same file: every line of it is
	// recognisable, so a frame quoted from it cannot pass for the graded one.
	frameDecoy = "DECOY 1\nDECOY 2\nDECOY 3\nDECOY 4\nDECOY 5\nDECOY 6\nDECOY 7\nDECOY 8\nDECOY 9\nDECOY 10\nDECOY 11\nDECOY 12\nDECOY 13\nDECOY 14\n"
)

// `enola check <repo>` quotes the graded repository's source under a finding,
// whichever directory it runs from. It used to read the working directory, and
// from another checkout with the same file names it printed that checkout's
// line under the graded tree's finding.
func TestCheck_FrameComesFromTheGradedRepository(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "enola")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/enola").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	parent := t.TempDir()
	repo := filepath.Join(parent, "shop")
	write := func(root, rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(repo, "go.mod", "module shop\n\ngo 1.21\n")
	write(repo, "customers/store.go", frameBefore)
	write(repo, "enola/constraints/privacy.yaml", frameRule)
	decoy := t.TempDir()
	write(decoy, "customers/store.go", frameDecoy)

	home := t.TempDir()
	run := func(dir string, args ...string) (int, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HOME="+home, "ENOLA_NO_UPDATE_CHECK=1", "ENOLA_NO_PROMPTS=1")
		out, err := cmd.Output()
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), string(out)
		} else if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return 0, string(out)
	}

	if code, out := run(repo, "baseline", "pin", "."); code != 0 {
		t.Fatalf("baseline pin exited %d:\n%s", code, out)
	}
	write(repo, "customers/store.go", frameAfter)

	const signature = "func (s *ProfileStore) Erase(subject string) {"
	for _, tc := range []struct{ name, dir string }{
		{"from another checkout", decoy},
		{"from an unrelated directory", t.TempDir()},
		{"from inside the repository", repo},
	} {
		code, out := run(tc.dir, "check", "--fail-on=constraints", repo)
		if code != 1 {
			t.Fatalf("%s: exit %d, want 1 (the rule is breached):\n%s", tc.name, code, out)
		}
		if strings.Contains(out, "DECOY") {
			t.Errorf("%s: the frame was quoted from the working directory:\n%s", tc.name, out)
		}
		if !strings.Contains(out, signature) {
			t.Errorf("%s: the frame must quote the graded file:\n%s", tc.name, out)
		}
	}

	// Annotations name the file relative to where the host runs.
	_, out := run(parent, "check", "--fail-on=constraints", "--format=annotations", "--host=github", repo)
	if !strings.Contains(out, "file=shop/customers/store.go,") {
		t.Errorf("from the parent directory the annotation must name shop/customers/store.go:\n%s", out)
	}
	_, out = run(repo, "check", "--fail-on=constraints", "--format=annotations", "--host=github", ".")
	if !strings.Contains(out, "file=customers/store.go,") {
		t.Errorf("from inside the repository the annotation must name customers/store.go:\n%s", out)
	}
}
