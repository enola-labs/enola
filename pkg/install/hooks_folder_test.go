package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A folder of repositories gets the hooks that steer the agent and not the ones that
// grade: Claude Code's session-start note, search gate and use recorder, no stop hook,
// no grading note in its rule, and nothing for Codex or Pi, whose hooks only grade.
// Uninstall still removes everything enola owns.
func TestInstall_FolderOfReposGetsTheSteeringHooksOnly(t *testing.T) {
	o := opts(t, true)
	o.FolderOfRepos = true
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}

	settings := read(t, filepath.Join(o.RepoDir, ".claude", "settings.json"))
	for _, want := range []string{"enola hook session-start", "enola hook pre-tool", "enola hook post-tool"} {
		if !strings.Contains(settings, want) {
			t.Errorf("settings.json lacks %q:\n%s", want, settings)
		}
	}
	if strings.Contains(settings, "enola hook stop") {
		t.Errorf("a folder of repositories got the grading hook:\n%s", settings)
	}
	if rule := read(t, filepath.Join(o.RepoDir, ".claude", "rules", "enola.md")); strings.Contains(rule, "enola's hook is installed") {
		t.Errorf("the rule describes a grading hook that is not installed:\n%s", rule)
	}
	if hooks := codexHooksAfter(t, o, false); hooks != nil {
		t.Errorf("Codex got hooks in a folder of repositories: %#v", hooks)
	}
	if src := piExtensionSource(o); !strings.Contains(src, "const HOOKS = false") {
		t.Errorf("Pi's extension should have its hooks off in a folder of repositories")
	}

	summary := strings.Join(HookSummary(o), "\n")
	if strings.Contains(summary, "at the end of a turn") || strings.Contains(summary, "frozen as a baseline") {
		t.Errorf("the summary announces grading that is not installed:\n%s", summary)
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
