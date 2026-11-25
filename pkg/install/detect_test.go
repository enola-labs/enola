package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHooksInstalled_SeesEachTargetsHooks: an install with hooks for any single target is
// recognised, and one without hooks is not.
func TestHooksInstalled_SeesEachTargetsHooks(t *testing.T) {
	for _, target := range []string{"claude", "codex", "pi", "opencode"} {
		t.Run(target, func(t *testing.T) {
			o := opts(t, true)
			o.Targets = []string{target}
			if HooksInstalled(o) {
				t.Fatal("hooks reported before anything was installed")
			}
			if _, err := Install(o); err != nil {
				t.Fatal(err)
			}
			if !HooksInstalled(o) {
				t.Errorf("hooks written for %s are not recognised", target)
			}
		})
	}
	o := opts(t, false)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if HooksInstalled(o) {
		t.Error("an install without --hooks is reported as having hooks")
	}
}

// TestHooksInstalled_IgnoresHooksOfTheUsers: a settings.json holding only the user's own
// hooks is not an enola install.
func TestHooksInstalled_IgnoresHooksOfTheUsers(t *testing.T) {
	o := opts(t, false)
	path := filepath.Join(o.RepoDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"make lint"}]}]}}`
	if err := os.WriteFile(path, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	if HooksInstalled(o) {
		t.Error("the user's own hook was taken for enola's")
	}
}

// TestDropHooks_RemovesHooksAndKeepsTheRest: --no-hooks takes out every hook an earlier
// --hooks install wrote and leaves the instructions and skills installed.
func TestDropHooks_RemovesHooksAndKeepsTheRest(t *testing.T) {
	o := opts(t, true)
	if err := os.MkdirAll(filepath.Join(o.RepoDir, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	o.Hooks, o.DropHooks = false, true
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if HooksInstalled(o) {
		t.Error("hooks are still installed after DropHooks")
	}
	rule := read(t, filepath.Join(o.RepoDir, ".claude", "rules", "enola.md"))
	if strings.Contains(rule, HooksNote) {
		t.Error("the rule still describes hooks that were removed")
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, ".pi", filepath.FromSlash(PiExtensionFile))); err != nil {
		t.Errorf("Pi's extension went with the hooks, but it serves the tools: %v", err)
	}
}

// TestDrift_ReportsOnlyWhatAPlainReinstallChanges: nothing installed is no install; a
// fresh install is current; an edited rule and a moved binary are each drift; and a
// narrowed install is not behind for the targets it left out.
func TestDrift_ReportsOnlyWhatAPlainReinstallChanges(t *testing.T) {
	o := opts(t, true)
	o.HookCommand = "/opt/enola/bin/enola"
	if rs, installed, err := Drift(o); err != nil || installed || rs != nil {
		t.Fatalf("empty repository: drift=%v installed=%v err=%v", rs, installed, err)
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	check := func(o Options, want ...string) {
		t.Helper()
		o.Hooks = false // as doctor and upgrade call it
		rs, installed, err := Drift(o)
		if err != nil || !installed {
			t.Fatalf("installed=%v err=%v", installed, err)
		}
		var got []string
		for _, r := range rs {
			got = append(got, string(r.Action)+" "+filepath.ToSlash(strings.TrimPrefix(r.Path, o.RepoDir+string(filepath.Separator))))
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("drift:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	check(o)

	rule := filepath.Join(o.RepoDir, ".claude", "rules", "enola.md")
	if err := os.WriteFile(rule, []byte(read(t, rule)+"\nedited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	check(o, "updated .claude/rules/enola.md")

	moved := o
	moved.HookCommand = "/usr/local/bin/enola"
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	rs, _, err := Drift(moved)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range rs {
		paths = append(paths, filepath.Base(r.Path))
	}
	if !strings.Contains(strings.Join(paths, " "), "settings.json") || !strings.Contains(strings.Join(paths, " "), "enola.js") {
		t.Errorf("a moved binary is not drift in the hooks and Pi's extension: %v", paths)
	}

	narrow := opts(t, false)
	narrow.Targets = []string{"cursor"}
	if _, err := Install(narrow); err != nil {
		t.Fatal(err)
	}
	narrow.Targets = nil
	check(narrow)
}
