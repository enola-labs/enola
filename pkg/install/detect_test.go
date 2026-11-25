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
