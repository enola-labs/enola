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
	if res.code != 1 || !strings.Contains(res.stdout, "names no declared component") || strings.Contains(res.stdout, "enola-intent.yaml with") {
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

// A draft is laid over the repository's declaration: a draft of rules alone keeps the
// layer order the repository already declares, and says which entries it adds.
func TestConstraintsLintDeclarationDraftOverlays(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildEnola(t)
	repo, home := draftRepo(t, bin)
	declared := "layers:\n  - {name: web, paths: [\"web/**\"]}\n  - {name: storage, paths: [\"storage/**\"]}\n" +
		"components:\n  - {name: web, match: [\"web/**\"]}\n"
	if err := os.WriteFile(filepath.Join(repo, "enola-intent.yaml"), []byte(declared), 0o644); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(t.TempDir(), "draft.yaml")
	if err := os.WriteFile(draft, []byte("components:\n  - {name: storage, match: [\"storage/**\"]}\n"+
		"rules:\n  - {id: storage-up, forbid: storage, to: web, via: imports, because: \"storage serves web\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runEnola(t, bin, repo, home, "constraints", "lint", "--declaration", draft, repo)
	if res.code != 0 {
		t.Fatalf("a draft naming a component the repository declares: exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	for _, want := range []string{"Declared layer resolution", "draft adds components: storage", "draft adds rules: storage-up", "laid over it"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, res.stdout)
		}
	}
	data, err := os.ReadFile(filepath.Join(repo, "enola-intent.yaml"))
	if err != nil || string(data) != declared {
		t.Error("linting a draft changed the repository's declaration")
	}
}

// Lint says what each rule would report today, so a proposal shows what adopting it
// accepts: the breaches it would carry, and which rules hold.
func TestConstraintsLintReportsRuleVerdicts(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildEnola(t)
	repo, home := draftRepo(t, bin)
	draft := filepath.Join(t.TempDir(), "draft.yaml")
	if err := os.WriteFile(draft, []byte("layers:\n  - {name: web, paths: [\"web/**\"]}\n  - {name: storage, paths: [\"storage/**\"]}\n"+
		"components:\n  - {name: storage, match: [\"storage/**\"]}\n  - {name: web, match: [\"web/**\"]}\n  - {name: ghost, match: [\"nowhere/**\"]}\n"+
		"rules:\n"+
		"  - {id: storage-up, forbid: storage, to: web, via: imports, because: \"storage serves web\"}\n"+
		"  - {id: web-no-storage, forbid: web, to: storage, via: imports, because: \"test\"}\n"+
		"  - {id: ghost-rule, forbid: ghost, to: web, via: imports, because: \"test\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runEnola(t, bin, repo, home, "constraints", "lint", "--declaration", draft, repo)
	if res.code != 0 {
		t.Fatalf("exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	for _, want := range []string{
		"Rule verdicts against the snapshot",
		"web-no-storage           1 breach [ratchet]",
		"storage-up               holds [ratchet]",
		"ghost-rule               holds vacuously: ghost matches nothing",
		"Declared layer order against the snapshot: 1 import(s) inward, 0 against.",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, res.stdout)
		}
	}
	if !strings.Contains(res.stdout, "example.com/d/web") || !strings.Contains(res.stdout, "example.com/d/storage") {
		t.Errorf("the breach does not name its edge:\n%s", res.stdout)
	}
}

// --fail-empty turns a selector that selects nothing into a problem, so a caller can
// gate on it; without the flag it stays a note and lint exits 0.
func TestConstraintsLintFailEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildEnola(t)
	repo, home := draftRepo(t, bin)
	drafts := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(drafts, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ghost := write("ghost.yaml", "components:\n  - {name: web, match: [\"web/**\"]}\n  - {name: ghost, match: [\"nowhere/**\"]}\n"+
		"rules:\n  - {id: g, forbid: ghost, to: web, via: imports, because: \"test\"}\n")
	if res := runEnola(t, bin, repo, home, "constraints", "lint", "--declaration", ghost, repo); res.code != 0 {
		t.Errorf("without --fail-empty an empty component is a note: exit %d\n%s", res.code, res.stdout)
	}
	res := runEnola(t, bin, repo, home, "constraints", "lint", "--fail-empty", "--declaration", ghost, repo)
	if res.code != 1 || !strings.Contains(res.stdout, "--fail-empty: 1 component or layer selects nothing") {
		t.Errorf("an empty component under --fail-empty: exit %d, want 1\n%s", res.code, res.stdout)
	}
	layer := write("layer.yaml", "layers:\n  - {name: web, paths: [\"web/**\"]}\n  - {name: gone, paths: [\"gone/**\"]}\n")
	if res := runEnola(t, bin, repo, home, "constraints", "lint", "--fail-empty", "--declaration", layer, repo); res.code != 1 {
		t.Errorf("an empty layer under --fail-empty: exit %d, want 1\n%s", res.code, res.stdout)
	}
	full := write("full.yaml", "components:\n  - {name: web, match: [\"web/**\"]}\n  - {name: storage, match: [\"storage/**\"]}\n")
	if res := runEnola(t, bin, repo, home, "constraints", "lint", "--fail-empty", "--declaration", full, repo); res.code != 0 {
		t.Errorf("every selector resolves: exit %d, want 0\n%s", res.code, res.stdout)
	}
}
