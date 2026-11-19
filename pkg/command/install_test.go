package command

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/hookstate"
	"github.com/enola-labs/enola/pkg/cli"
	"github.com/enola-labs/enola/pkg/install"
)

// The heartbeat is the one thing `install --hooks` writes outside the agents' own config
// files, and in a repository that has never been snapshotted it is also what creates the
// output directory. Uninstall has to take both back, or the reversal stops one empty
// directory short of complete.
func TestClearHookHeartbeat_RemovesTheDirectoryItCreated(t *testing.T) {
	repo := t.TempDir()
	outDir := filepath.Join(repo, ".enola")

	hookstate.RecordInstalled(outDir, "enola")
	if _, err := os.Stat(hookstate.Path(outDir)); err != nil {
		t.Fatalf("the heartbeat was not written: %v", err)
	}

	clearHookHeartbeat(repo, outDir)

	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(outDir)
		t.Errorf("uninstall left the output directory behind (err=%v, %d entries)", err, len(entries))
	}
}

// It is the emptiness that licenses the removal, never the uninstall. A baseline or a
// snapshot in there is the user's work, and losing it to an uninstall would cost far more
// than the empty directory this is here to clean up.
func TestClearHookHeartbeat_KeepsAnOutputDirectoryHoldingWork(t *testing.T) {
	repo := t.TempDir()
	outDir := filepath.Join(repo, ".enola")

	hookstate.RecordInstalled(outDir, "enola")
	baseline := filepath.Join(outDir, "baseline.json")
	if err := os.WriteFile(baseline, []byte(`{"pinned":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	clearHookHeartbeat(repo, outDir)

	if _, err := os.Stat(baseline); err != nil {
		t.Errorf("uninstall removed a pinned baseline: %v", err)
	}
	if _, err := os.Stat(hookstate.Path(outDir)); !os.IsNotExist(err) {
		t.Errorf("the heartbeat survived an uninstall (err=%v)", err)
	}
}

// An output directory configured as the repository itself is not ours to remove, however
// empty it happens to be.
func TestClearHookHeartbeat_NeverRemovesTheRepositoryItself(t *testing.T) {
	repo := t.TempDir()

	hookstate.RecordInstalled(repo, "enola")
	clearHookHeartbeat(repo, repo)

	if _, err := os.Stat(repo); err != nil {
		t.Errorf("uninstall removed the repository directory: %v", err)
	}
}

// runInstallCapturing runs `install`/`uninstall` with the given args in an isolated
// HOME and returns what it wrote to stdout. Both return normally on the paths tested
// here (--yes skips the confirmation, and a temp repository is a valid target).
func runInstallCapturing(t *testing.T, remove bool, args ...string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = prev }()

	New(cli.Binary{Name: "enola"}, "upgrade").Install(args, remove)

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The preview and the applied result are the same list whenever nothing moved under
// the run, and printing both in full made every `--yes` install report each file
// twice with nothing in between to explain the repetition — it reads as a rendering
// bug. The plan is shown once; the second pass collapses to a line.
func TestInstall_PrintsThePlanOnceWhenApplyingMatchesThePreview(t *testing.T) {
	repo := t.TempDir()

	out := runInstallCapturing(t, false, "--yes", repo)

	rule := filepath.ToSlash(filepath.Join(repo, ".claude", "rules", "enola.md"))
	if got := strings.Count(filepath.ToSlash(out), rule); got != 1 {
		t.Errorf("the plan named %s %d time(s), want exactly 1:\n%s", rule, got, out)
	}
	if !strings.Contains(out, "as previewed.") {
		t.Errorf("no one-line confirmation that the apply matched the preview:\n%s", out)
	}
	if strings.Contains(out, "differs from the preview") {
		t.Errorf("an unchanged apply was reported as divergent:\n%s", out)
	}
}

// A repository with an AGENTS.md is the case where one target's reason depended on
// another's write: the agents target fills AGENTS.md, and opencode, deciding after it,
// worded its skip differently in the apply than in the preview. A real install then
// printed every file twice under "differs from the preview".
func TestInstall_MatchesThePreviewWithAnAgentsMd(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("# Agents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runInstallCapturing(t, false, "--yes", repo)
	if strings.Contains(out, "differs from the preview") || !strings.Contains(out, "as previewed.") {
		t.Errorf("an install that did what it previewed was reported as divergent:\n%s", out)
	}
}

// Uninstall runs the same two passes, so it gets the same treatment — and its own
// verb, since "Written" is not what a removal did.
func TestUninstall_PrintsThePlanOnceAndSaysRemoved(t *testing.T) {
	repo := t.TempDir()
	runInstallCapturing(t, false, "--yes", repo)

	out := runInstallCapturing(t, true, "--yes", repo)

	rule := filepath.ToSlash(filepath.Join(repo, ".claude", "rules", "enola.md"))
	if got := strings.Count(filepath.ToSlash(out), rule); got != 1 {
		t.Errorf("the plan named %s %d time(s), want exactly 1:\n%s", rule, got, out)
	}
	if !strings.Contains(out, "Removed — ") {
		t.Errorf("uninstall did not report what it removed:\n%s", out)
	}
}

// The collapse is only safe while it is conditional. A result that diverges from the
// preview is the one case worth re-reading in full, so samePlan compares every field
// rather than counting entries.
func TestSamePlan_ComparesActionsAndReasonsNotJustPaths(t *testing.T) {
	planned := []install.Result{
		{Path: "a", Action: install.ActionCreated},
		{Path: "b", Action: install.ActionSkipped, Reason: "does not exist"},
	}

	for name, applied := range map[string][]install.Result{
		"same":            {{Path: "a", Action: install.ActionCreated}, {Path: "b", Action: install.ActionSkipped, Reason: "does not exist"}},
		"action changed":  {{Path: "a", Action: install.ActionUnchanged}, {Path: "b", Action: install.ActionSkipped, Reason: "does not exist"}},
		"reason changed":  {{Path: "a", Action: install.ActionCreated}, {Path: "b", Action: install.ActionSkipped, Reason: "unreadable"}},
		"entry dropped":   {{Path: "a", Action: install.ActionCreated}},
		"order different": {{Path: "b", Action: install.ActionSkipped, Reason: "does not exist"}, {Path: "a", Action: install.ActionCreated}},
	} {
		want := name == "same"
		if got := samePlan(planned, applied); got != want {
			t.Errorf("%s: samePlan = %v, want %v", name, got, want)
		}
	}
}

// Only files that were actually touched are counted: `skipped` and `unchanged`
// entries earn their place in the plan through the reasons they carry, and counting
// them would overstate what the run did.
func TestCountChanged_IgnoresSkippedAndUnchanged(t *testing.T) {
	rs := []install.Result{
		{Path: "a", Action: install.ActionCreated},
		{Path: "b", Action: install.ActionUpdated},
		{Path: "c", Action: install.ActionRemoved},
		{Path: "d", Action: install.ActionSkipped},
		{Path: "e", Action: install.ActionUnchanged},
	}
	if got := countChanged(rs); got != 3 {
		t.Errorf("countChanged = %d, want 3", got)
	}
}
