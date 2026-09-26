package install

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestStopHook_IncomparableBaselineTellsTheUserNotTheModel — a baseline the gate cannot
// compare against is enola's setup, not the session's change. Handed to the model as
// additionalContext it bought a turn the model spent re-pinning over a deliberate
// baseline and chasing `enola doctor`, in a session that had edited nothing. So it must
// come out as systemMessage, which the harness shows the user and does not feed back.
func TestStopHook_IncomparableBaselineTellsTheUserNotTheModel(t *testing.T) {
	if testing.Short() {
		t.Skip("builds enola")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the fixture's paths assume POSIX")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	work := t.TempDir()
	enola := buildEnola(ctx, t, work)
	t.Setenv("HOME", filepath.Join(work, "home"))
	repo := writeCyclePendingRepo(t, work)
	runCLI(ctx, t, repo, enola, "baseline", "pin", repo)

	// Changing the ignore globs after the pin is one of the blocking comparability
	// causes: the set of files parsed differs, so the delta is not the code's.
	writeFile(t, filepath.Join(repo, "mcp-arch.yaml"), "ignore:\n  - \"docs/**\"\n")

	cmd := exec.CommandContext(ctx, enola, "hook", "stop")
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(`{"cwd":"` + repo + `","session_id":"s1","hook_event_name":"Stop"}`)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("hook stop: %v\n%s", err, raw)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &out); err != nil {
		t.Fatalf("hook stop printed no JSON: %v\n%s", err, raw)
	}
	msg, _ := out["systemMessage"].(string)
	if !strings.Contains(msg, "could not grade") || !strings.Contains(msg, "ignore_globs") {
		t.Errorf("systemMessage does not say why grading was declined: %q", msg)
	}
	if _, ok := out["hookSpecificOutput"]; ok {
		t.Errorf("the decline was also handed to the model: %s", raw)
	}
}
