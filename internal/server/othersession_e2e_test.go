package server_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/engine"
)

// Agent sessions working on one repository share its pinned baseline and its previous
// run. When the "before" of a diff came from another session that is still running,
// diff_snapshot says so, and set_baseline says when it replaced that session's pin.
// The server's own session is its parent process; this test process stands in for the
// other session, running and different.
func TestE2E_DiffSaysWhenItsBeforeIsAnotherRunningSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	repo := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), t.TempDir())
	eng, cfg := newTestEngine(t)
	cfg.Repo = repo
	s := connect(t, eng, cfg)
	outDir := filepath.Join(repo, ".enola")
	other := os.Getpid()

	markAs := func(dir, name string, agent int) {
		t.Helper()
		data, _ := json.Marshal(engine.SessionMark{AgentPID: agent, At: "2026-09-27T10:00:00Z"})
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const pinNote, replaceNote, prevNote = "pinned at 2026-09-27T10:00:00Z by another agent session",
		"This replaced a baseline another running agent session pinned", "previous run was taken at 2026-09-27T10:00:00Z by another agent session"

	s.call(t, "generate_snapshot", map[string]any{"repo_path": repo, "fresh": true})
	if out := text(s.call(t, "set_baseline", nil)); strings.Contains(out, replaceNote) {
		t.Fatalf("the first pin claimed to replace another session's:\n%s", out)
	}
	if out := text(s.call(t, "diff_snapshot", nil)); strings.Contains(out, pinNote) {
		t.Fatalf("a diff against this session's own pin was attributed to another session:\n%s", out)
	}

	markAs(filepath.Join(outDir, "baseline"), engine.PinMarkFile, other)
	if out := text(s.call(t, "diff_snapshot", nil)); !strings.Contains(out, pinNote) {
		t.Errorf("a diff against another running session's pin did not say so:\n%s", out)
	}
	if out := text(s.call(t, "diff_snapshot", map[string]any{"output_mode": "full"})); !strings.Contains(out, `"other_session"`) {
		t.Errorf("the full diff does not carry the other_session warning kind:\n%.600s", out)
	}
	if out := text(s.call(t, "set_baseline", nil)); !strings.Contains(out, replaceNote) {
		t.Errorf("set_baseline replaced another running session's pin without saying so:\n%s", out)
	}
	if out := text(s.call(t, "diff_snapshot", nil)); strings.Contains(out, pinNote) {
		t.Errorf("after re-pinning, the baseline is this session's, but the diff still attributes it elsewhere:\n%s", out)
	}

	s.call(t, "generate_snapshot", map[string]any{"repo_path": repo, "fresh": true})
	markAs(filepath.Join(outDir, "previous"), engine.RunMarkFile, other)
	if out := text(s.call(t, "diff_snapshot", map[string]any{"baseline": "previous"})); !strings.Contains(out, prevNote) {
		t.Errorf("a diff against another running session's previous run did not say so:\n%s", out)
	}
}
