package command

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/cli"
)

func writePinMark(t *testing.T, baselineDir string, agent int) {
	t.Helper()
	data, err := json.Marshal(engine.SessionMark{AgentPID: agent, At: "2026-09-27T10:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baselineDir, engine.PinMarkFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOtherSessionPin(t *testing.T) {
	ended := exec.Command("true")
	if err := ended.Run(); err != nil {
		t.Skipf("cannot start a short-lived process: %v", err)
	}
	me, other := os.Getppid(), os.Getpid() // both running, and different
	for _, tc := range []struct {
		name   string
		pinner int
		want   bool
	}{
		{"another running session", other, true},
		{"this session", me, false},
		{"an unknown pinner", 0, false},
		{"a session that has ended", ended.Process.Pid, false},
	} {
		dir := t.TempDir()
		writePinMark(t, dir, tc.pinner)
		if got := otherSessionPin(dir, me) != nil; got != tc.want {
			t.Errorf("%s: otherSessionPin = %v, want %v", tc.name, got, tc.want)
		}
	}
	if otherSessionPin(t.TempDir(), me) != nil {
		t.Error("a baseline with no pin mark was attributed to another session")
	}
}

// Session A auto-pins and starts editing. Session B opens the same repository: the tree
// is dirty with A's edits, which shouldAutoPin alone reads as "refresh". Re-pinning would
// fold A's edits into A's "before" and hide them from A's grade, so B must leave it.
// A pin of B's own session is still refreshed.
func TestSessionStartHook_LeavesAnotherRunningSessionsPinAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module m\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baselineDir := filepath.Join(repo, ".enola", "baseline")
	me, other := os.Getppid(), os.Getpid()
	r := New(cli.Binary{Name: "enola"})

	// A non-git repo: shouldAutoPin cannot prove the auto-pin current and says refresh.
	writeBaseline(t, baselineDir, &facts.GitInfo{Commit: "deadbeef", Dirty: true}, true)
	writePinMark(t, baselineDir, other)
	r.pinBaselineSingleFlight(context.Background(), repo, me)
	if m := engine.ReadSessionMark(baselineDir, engine.PinMarkFile); m == nil || m.AgentPID != other {
		t.Fatalf("session start replaced a baseline another running session pinned: pin mark now %+v", m)
	}

	writePinMark(t, baselineDir, me)
	r.pinBaselineSingleFlight(context.Background(), repo, me)
	m := engine.ReadSessionMark(baselineDir, engine.PinMarkFile)
	if m == nil || m.AgentPID != me || m.At == "2026-09-27T10:00:00Z" {
		t.Fatalf("this session's own auto-pin was not refreshed: pin mark %+v", m)
	}
}

// A grade against another running session's pin comes out clean when this session
// changed nothing structural, and a silent Stop would read as "this session's change is
// clean". The user is told, once, what the grade was measured against.
func TestStopHook_CaveatsAGradeAgainstAnotherRunningSessionsPin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module m\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New(cli.Binary{Name: "enola"})
	r.pinBaselineSingleFlight(context.Background(), repo, 0)
	baselineDir := filepath.Join(repo, ".enola", "baseline")

	stop := func(session string) string {
		t.Helper()
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		prev := os.Stdout
		os.Stdout = pw
		withHookPayload(t, `{"cwd":"`+repo+`","session_id":"`+session+`"}`, func() { r.runStopHook(context.Background()) })
		os.Stdout = prev
		_ = pw.Close()
		out, _ := io.ReadAll(pr)
		return string(out)
	}

	if out := stop("s1"); out != "" {
		t.Fatalf("a clean grade against a pin of no known session printed:\n%s", out)
	}
	writePinMark(t, baselineDir, os.Getpid())
	out := stop("s2")
	if !strings.Contains(out, "another agent session") || !strings.Contains(out, "systemMessage") {
		t.Fatalf("a clean grade against another running session's pin was not caveated to the user:\n%s", out)
	}
	if again := stop("s2"); again != "" {
		t.Errorf("the caveat repeated within the session:\n%s", again)
	}
}

// enola check and the Stop hook name the "before" by selector: a pin for pinned, the
// rotated run for previous, and nothing for an explicit path the caller chose.
func TestOtherSessionBefore_FollowsTheSelector(t *testing.T) {
	me, other := os.Getppid(), os.Getpid()
	dir := t.TempDir()
	writePinMark(t, dir, other)
	data, _ := json.Marshal(engine.SessionMark{AgentPID: other, At: "2026-09-27T11:00:00Z"})
	if err := os.WriteFile(filepath.Join(dir, engine.RunMarkFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := otherSessionBefore(dir, "pinned", me); !strings.Contains(got, "baseline was pinned at 2026-09-27T10:00:00Z") {
		t.Errorf("pinned: %q", got)
	}
	if got := otherSessionBefore(dir, "previous", me); !strings.Contains(got, "previous run was taken at 2026-09-27T11:00:00Z") {
		t.Errorf("previous: %q", got)
	}
	if got := otherSessionBefore(dir, dir, me); got != "" {
		t.Errorf("an explicit baseline path was attributed to a session: %q", got)
	}
	if got := otherSessionBefore(dir, "pinned", other); got != "" {
		t.Errorf("this session's own pin was attributed to another: %q", got)
	}
}
