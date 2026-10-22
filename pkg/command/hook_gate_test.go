package command

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func TestIsSearch(t *testing.T) {
	for _, c := range []struct {
		tool, cmd string
		want      bool
	}{
		{"Grep", "", true},
		{"Glob", "", true},
		{"Read", "", false},
		{"Bash", `grep -rn "Foo" internal`, true},
		{"Bash", `cd repo && rg -l Foo | head`, true},
		{"Bash", `git grep -n Foo`, true},
		{"Bash", `find . -name '*.go'`, true},
		{"Bash", `go test ./...`, false},
		{"Bash", `git status`, false},
		{"Bash", `cat main.go`, false},
	} {
		in := map[string]any{"command": c.cmd}
		if got := isSearch(c.tool, in); got != c.want {
			t.Errorf("isSearch(%s, %q) = %v, want %v", c.tool, c.cmd, got, c.want)
		}
	}
}

// runHookWith feeds payload to a hook function on stdin and returns its stdout.
func runHookWith(t *testing.T, fn func(), payload string) string {
	t.Helper()
	in, err := os.CreateTemp(t.TempDir(), "payload")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	savedIn, savedOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = in, w
	fn()
	os.Stdin, os.Stdout = savedIn, savedOut
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// The gate refuses a session's first two searches with a pointer to enola, lets the
// third through, and stops refusing altogether once an enola tool has been used.
func TestPreToolGate(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	r := &Runner{}
	search := `{"session_id":"s1","tool_name":"Grep","tool_input":{"pattern":"Foo"}}`

	for i, wantDeny := range []bool{true, true, false} {
		out := runHookWith(t, r.runPreToolHook, search)
		denied := strings.Contains(out, `"permissionDecision":"deny"`)
		if denied != wantDeny {
			t.Fatalf("search %d: denied=%v, want %v (output %q)", i+1, denied, wantDeny, out)
		}
		if denied {
			var got struct {
				HookSpecificOutput struct {
					HookEventName            string `json:"hookEventName"`
					PermissionDecisionReason string `json:"permissionDecisionReason"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil || got.HookSpecificOutput.HookEventName != "PreToolUse" ||
				!strings.Contains(got.HookSpecificOutput.PermissionDecisionReason, "select:mcp__enola__") {
				t.Errorf("search %d: refusal should name the tools to load: %q (%v)", i+1, out, err)
			}
		}
	}

	// A fresh session that uses enola before searching is never refused.
	runHookWith(t, r.runPostToolHook, `{"session_id":"s2","tool_name":"mcp__enola__impact_analysis"}`)
	if out := runHookWith(t, r.runPreToolHook, `{"session_id":"s2","tool_name":"Glob","tool_input":{}}`); out != "" {
		t.Errorf("a session that used enola should not be gated: %q", out)
	}
	// Something that is not a search is never refused.
	if out := runHookWith(t, r.runPreToolHook, `{"session_id":"s3","tool_name":"Bash","tool_input":{"command":"go test ./..."}}`); out != "" {
		t.Errorf("a build command is not a search: %q", out)
	}
}

func TestSessionStartNote(t *testing.T) {
	out := runHookWith(t, writeSessionStartNote, "")
	var got struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.HookSpecificOutput.HookEventName != "SessionStart" ||
		!strings.Contains(got.HookSpecificOutput.AdditionalContext, "ToolSearch") {
		t.Errorf("session start note = %q (%v)", out, err)
	}
}
