package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A folder of repositories gets Claude Code's hooks, the grading ones included: its
// pre-edit hook finds the repository an edit lands in and pins it, and its stop hook
// grades those. Codex and Pi, whose hooks cannot tell which repository an edit is in,
// get none, and only Claude Code's rule describes grading. Uninstall still removes
// everything enola owns.
func TestInstall_FolderOfReposGradesThroughClaudeOnly(t *testing.T) {
	o := opts(t, true)
	o.FolderOfRepos = true
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}

	settings := read(t, filepath.Join(o.RepoDir, ".claude", "settings.json"))
	for _, want := range []string{"enola hook session-start", "enola hook pre-tool", "enola hook post-tool",
		"enola hook pre-edit", "enola hook stop"} {
		if !strings.Contains(settings, want) {
			t.Errorf("settings.json lacks %q:\n%s", want, settings)
		}
	}
	if rule := read(t, filepath.Join(o.RepoDir, ".claude", "rules", "enola.md")); !strings.Contains(rule, "enola's hook is installed") {
		t.Errorf("Claude Code's rule does not describe the grading it gets:\n%s", rule)
	}
	if rule := read(t, filepath.Join(o.RepoDir, ".cursor", "rules", "enola.mdc")); strings.Contains(rule, "enola's hook is installed") {
		t.Errorf("Cursor's rule describes grading it does not get:\n%s", rule)
	}
	if hooks := codexHooksAfter(t, o, false); hooks != nil {
		t.Errorf("Codex got hooks in a folder of repositories: %#v", hooks)
	}
	if src := piExtensionSource(o); !strings.Contains(src, "const HOOKS = false") {
		t.Errorf("Pi's extension should have its hooks off in a folder of repositories")
	}

	summary := strings.Join(HookSummary(o), "\n")
	if !strings.Contains(summary, "first edit inside one of the folder's repositories") ||
		!strings.Contains(summary, "each repository the session edited is graded") {
		t.Errorf("the summary leaves out the folder's grading:\n%s", summary)
	}
	if !strings.Contains(summary, "first two file searches") {
		t.Errorf("the summary leaves out the search gate:\n%s", summary)
	}

	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(o.RepoDir, ".claude", "settings.json")); err == nil && strings.Contains(string(b), "enola hook") {
		t.Errorf("uninstall left enola's hooks behind:\n%s", b)
	}
}

// Outside a folder of repositories the pre-edit hook has no job and is not written, and
// a repository re-installed after a folder install loses it.
func TestInstall_PreEditOnlyInAFolder(t *testing.T) {
	o := opts(t, true)
	o.FolderOfRepos = true
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	o.FolderOfRepos = false
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	settings := read(t, filepath.Join(o.RepoDir, ".claude", "settings.json"))
	if strings.Contains(settings, "hook pre-edit") {
		t.Errorf("a repository kept the folder's pre-edit hook:\n%s", settings)
	}
	for _, want := range []string{"hook pre-tool", "hook stop"} {
		if !strings.Contains(settings, want) {
			t.Errorf("settings.json lacks %q:\n%s", want, settings)
		}
	}
}
