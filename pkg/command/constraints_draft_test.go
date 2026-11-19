package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// draftRepo is a two-package Go module with a snapshot, so lint can resolve selectors.
func draftRepo(t *testing.T, bin string) (repo, home string) {
	t.Helper()
	repo, home = t.TempDir(), t.TempDir()
	files := map[string]string{
		"go.mod":        "module example.com/d\n\ngo 1.22\n",
		"web/w.go":      "package web\n\nimport \"example.com/d/storage\"\n\nfunc Serve() int { return storage.Read() }\n",
		"storage/s.go":  "package storage\n\nfunc Read() int { return 1 }\n",
		"mcp-arch.yaml": "history:\n  enabled: false\n",
	}
	for p, c := range files {
		if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if res := runEnola(t, bin, repo, home, "--generate", repo); res.code != 0 {
		t.Fatalf("generate: %d\n%s", res.code, res.stderr)
	}
	return repo, home
}

// A draft is linted, and its selectors resolved, without being written: the point is
// to check a proposal before anyone adopts it.
func TestConstraintsLintDeclarationDraft(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildEnola(t)
	repo, home := draftRepo(t, bin)
	drafts := t.TempDir()

	good := filepath.Join(drafts, "good.yaml")
	if err := os.WriteFile(good, []byte("layers:\n  - {name: web, paths: [\"web/**\"]}\n  - {name: storage, paths: [\"storage/**\"]}\n"+
		"components:\n  - {name: storage, match: [\"storage/**\"]}\n  - {name: web, match: [\"web/**\"]}\n"+
		"rules:\n  - {id: storage-up, forbid: storage, to: web, via: imports, because: \"storage serves web\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runEnola(t, bin, repo, home, "constraints", "lint", "--declaration", good, repo)
	if res.code != 0 {
		t.Fatalf("a valid draft: exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "member(s)") || strings.Contains(res.stdout, " 0 member(s)") {
		t.Errorf("the draft's selectors were not resolved against the snapshot:\n%s", res.stdout)
	}

	bad := filepath.Join(drafts, "bad.yaml")
	if err := os.WriteFile(bad, []byte("rules:\n  - {id: x, forbid: nowhere, to: web, via: imports, because: \"y\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = runEnola(t, bin, repo, home, "constraints", "lint", "--declaration", bad, repo)
	if res.code != 1 || !strings.Contains(res.stdout, "names no declared component") {
		t.Errorf("an invalid draft: exit %d, want 1 naming the undeclared component\n%s%s", res.code, res.stdout, res.stderr)
	}

	if _, err := os.Stat(filepath.Join(repo, "enola-intent.yaml")); err == nil {
		t.Error("linting a draft wrote it into the repository")
	}

	// The old trap: the draft passed where a config path goes used to lint "OK".
	res = runEnola(t, bin, repo, home, "constraints", "lint", bad)
	if res.code == 0 || !strings.Contains(res.stdout+res.stderr, "--declaration") {
		t.Errorf("a declaration passed as a config: exit %d, want a refusal pointing at --declaration\n%s%s", res.code, res.stdout, res.stderr)
	}
}
