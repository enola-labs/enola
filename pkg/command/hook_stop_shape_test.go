package command

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStopOutput_ShapedForTheHarness: Codex's Stop hook ignores additionalContext, so
// every report enola sent it reached no model. Codex continues the turn on decision
// "block" and takes reason as the next prompt; Claude Code, and Pi through its
// extension, read additionalContext. A notice for the user is systemMessage for all.
func TestStopOutput_ShapedForTheHarness(t *testing.T) {
	codex := readHookInputFrom(t, `{"cwd":"/r","session_id":"s","turn_id":"t-1","stop_hook_active":false}`)
	if codex.TurnID != "t-1" {
		t.Fatalf("turn_id not read from a Codex payload: %+v", codex)
	}
	claude := readHookInputFrom(t, `{"cwd":"/r","session_id":"s"}`)

	for name, tc := range map[string]struct {
		in              hookInput
		context, notice string
		want, avoid     []string
	}{
		"codex report": {codex, "a cycle", "",
			[]string{`"decision":"block"`, `"reason":"a cycle"`}, []string{"additionalContext"}},
		"claude report": {claude, "a cycle", "",
			[]string{`"additionalContext":"a cycle"`, `"hookEventName":"Stop"`}, []string{"decision"}},
		"codex notice": {codex, "", "could not grade",
			[]string{`"systemMessage":"could not grade"`}, []string{"decision", "additionalContext"}},
		"claude notice": {claude, "", "could not grade",
			[]string{`"systemMessage":"could not grade"`}, []string{"decision", "additionalContext"}},
	} {
		b, err := json.Marshal(stopOutput(tc.in, tc.context, tc.notice))
		if err != nil {
			t.Fatal(err)
		}
		got := string(b)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %s lacks %s", name, got, w)
			}
		}
		for _, a := range tc.avoid {
			if strings.Contains(got, a) {
				t.Errorf("%s: %s must not carry %s", name, got, a)
			}
		}
	}
}
