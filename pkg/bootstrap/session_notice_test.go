package bootstrap

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A restored graph is flagged only when another agent session that is still running
// wrote it. This session's own graph, a CLI run's, one from before writers were
// recorded, and one whose session has ended are restored without a word.
func TestSiblingNotice(t *testing.T) {
	ended := exec.Command("true")
	if err := ended.Run(); err != nil {
		t.Skipf("cannot start a short-lived process: %v", err)
	}
	endedPID := ended.Process.Pid

	receipt := func(w *facts.GraphWriter) *facts.GraphReceipt {
		return &facts.GraphReceipt{
			GeneratedAt: "2026-09-27T09:02:33Z",
			Repos:       []facts.GraphRepoEntry{{Label: "golf"}, {Label: "golf-ui"}},
			Writer:      w,
		}
	}
	const me = 1
	running := os.Getpid()

	for _, tc := range []struct {
		name   string
		writer *facts.GraphWriter
		want   bool
	}{
		{"another running session", &facts.GraphWriter{PID: 2, ClientPID: running}, true},
		{"this session", &facts.GraphWriter{PID: 2, ClientPID: me}, false},
		{"a CLI run", &facts.GraphWriter{PID: 2}, false},
		{"no writer recorded", nil, false},
		{"a session that has ended", &facts.GraphWriter{PID: 2, ClientPID: endedPID}, false},
	} {
		got := siblingNotice(receipt(tc.writer), me)
		if (got != "") != tc.want {
			t.Errorf("%s: notice = %q, want one: %v", tc.name, got, tc.want)
		}
		if tc.want && !strings.Contains(got, "golf, golf-ui") {
			t.Errorf("%s: notice does not name the restored repos: %q", tc.name, got)
		}
	}
}
