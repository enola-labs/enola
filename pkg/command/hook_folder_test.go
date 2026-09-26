package command

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/hookstate"
	"github.com/enola-labs/enola/pkg/cli"
)

// withHookPayload feeds payload to the hook on stdin for the duration of fn.
func withHookPayload(t *testing.T, payload string, fn func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	saved := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = saved }()
	fn()
}

// TestHooks_DoNothingInAFolderOfRepositories: both hooks snapshot the directory they
// run in as ONE repository. Installed in a folder holding every repository a user has,
// that was 878,773 files and 9.1M facts per session start and per graded turn, a 16 GB
// .enola, and tens of GB of memory. In a folder of repositories they must do no work,
// write no snapshot, and record why, so `doctor` can say so.
func TestHooks_DoNothingInAFolderOfRepositories(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	folder := t.TempDir()
	for _, repo := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(folder, repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(folder, repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := New(cli.Binary{Name: "enola"})
	payload := `{"cwd":"` + folder + `","session_id":"s1"}`

	withHookPayload(t, payload, func() { r.runSessionStartHook(context.Background(), nil) })
	withHookPayload(t, payload, func() { r.runStopHook(context.Background()) })
	// The detached child, reached directly, must refuse as well.
	r.pinBaselineSingleFlight(context.Background(), folder)

	outDir := filepath.Join(folder, ".enola")
	state := hookstate.Load(outDir)
	for _, e := range []hookstate.Event{hookstate.EventSessionStart, hookstate.EventStop} {
		if rec := state.Get(e); rec == nil || rec.LastOutcome != hookstate.OutcomeNotARepo {
			t.Errorf("%s: outcome %+v, want %s", e, rec, hookstate.OutcomeNotARepo)
		}
	}
	for _, name := range []string{"facts.jsonl", filepath.Join("baseline", "facts.jsonl")} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err == nil {
			t.Errorf("a hook snapshotted the folder of repositories: %s exists", name)
		}
	}
}
