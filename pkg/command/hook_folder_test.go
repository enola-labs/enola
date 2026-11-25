package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	r.pinBaselineSingleFlight(context.Background(), folder, 0)

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

// A session in a folder of repositories is graded per repository: the pre-edit hook pins
// the baseline of the repository an edit lands in, once per session, and the stop hook
// grades that repository and names it. A repository the session never edited is not
// snapshotted at all, and neither is the folder.
func TestFolderSession_GradesTheRepositoryItEdited(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	folder := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(folder, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, repo := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(folder, repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(repo, "go.mod"), "module "+repo+"\n\ngo 1.22\n")
	}
	write("alpha/a/a.go", "package a\n\nimport \"alpha/b\"\n\nfunc A() { b.B() }\n")
	write("alpha/b/b.go", "package b\n\nfunc B() {}\n")
	write("beta/main.go", "package main\n\nfunc main() {}\n")

	r := New(cli.Binary{Name: "enola"})
	session := "folder-grade-" + filepath.Base(folder)
	t.Cleanup(func() {
		if p, ok := gateStatePath(session); ok {
			_ = os.Remove(p)
		}
	})
	edit := `{"cwd":"` + folder + `","session_id":"` + session + `","tool_name":"Edit","tool_input":{"file_path":"` +
		filepath.Join(folder, "alpha", "b", "b.go") + `"}}`

	withHookPayload(t, edit, func() { r.runPreEditHook(context.Background()) })
	baseline := filepath.Join(folder, "alpha", ".enola", "baseline", "facts.jsonl")
	first, err := os.Stat(baseline)
	if err != nil {
		t.Fatalf("the pre-edit hook pinned no baseline for the repository edited: %v", err)
	}
	if got := loadGate(session).Repos; len(got) != 1 || got[0] != filepath.Join(folder, "alpha") {
		t.Fatalf("session repos = %v", got)
	}

	// The edit: b now imports a, which imports b.
	write("alpha/b/b.go", "package b\n\nimport \"alpha/a\"\n\nfunc B() { a.A() }\n")

	withHookPayload(t, edit, func() { r.runPreEditHook(context.Background()) })
	if again, err := os.Stat(baseline); err != nil || !again.ModTime().Equal(first.ModTime()) {
		t.Errorf("a second edit in the same session re-pinned the baseline (err=%v)", err)
	}

	stop := `{"cwd":"` + folder + `","session_id":"` + session + `"}`
	out := captureStdout(t, func() {
		withHookPayload(t, stop, func() { r.runStopHook(context.Background()) })
	})
	if !strings.Contains(out, "In the repository alpha") || !strings.Contains(strings.ToLower(out), "cycle") {
		t.Errorf("the stop hook did not report the cycle in alpha:\n%s", out)
	}
	for _, p := range []string{filepath.Join(folder, "beta", ".enola", "baseline"), filepath.Join(folder, ".enola", "facts.jsonl")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("snapshotted what the session never edited: %s", p)
		}
	}
	if rec := hookstate.Load(filepath.Join(folder, ".enola")).Get(hookstate.EventStop); rec == nil {
		t.Error("the folder's stop hook left no record for doctor")
	}
}

func TestRepoOfEdit(t *testing.T) {
	folder := t.TempDir()
	for _, d := range []string{"alpha/.git", "alpha/vendor/inner/.git", "loose/dir"} {
		if err := os.MkdirAll(filepath.Join(folder, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{
		filepath.Join(folder, "alpha", "x", "new.go"): filepath.Join(folder, "alpha"),
		"alpha/main.go": filepath.Join(folder, "alpha"),
		filepath.Join(folder, "alpha", "vendor", "inner", "y.go"): filepath.Join(folder, "alpha", "vendor", "inner"),
		filepath.Join(folder, "loose", "dir", "z.go"):             "",
		filepath.Join(folder, "top.md"):                           "",
		filepath.Join(filepath.Dir(folder), "elsewhere", "w.go"):  "",
		"": "",
	} {
		if got := repoOfEdit(folder, path); got != want {
			t.Errorf("repoOfEdit(%q) = %q, want %q", path, got, want)
		}
	}
}
