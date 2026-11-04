package command

import (
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/history"
)

// A broad pattern lists up to -n facts and counts the rest; -n 0 lists all.
func TestRenderBlame_Capped(t *testing.T) {
	ev := func(id string, n int) history.BlameEvent {
		e := history.BlameEvent{Entry: history.Entry{ID: "sha256:" + id, At: "2026-11-01T10:00:00Z"}}
		for i := 1; i <= n; i++ {
			e.Added = append(e.Added, fmt.Sprintf(`{"kind":"symbol","name":"pkg.F%d","file":"pkg/f.go","line":%d}`, i, i))
		}
		return e
	}
	b := &history.Blame{Pattern: "pkg", Scanned: 3, Events: []history.BlameEvent{
		ev("aaaaaaa1", 3), ev("bbbbbbb2", 4), ev("ccccccc3", 5),
	}}

	out := renderBlame(b, false, false, 5)
	if got := strings.Count(out, "    + "); got != 5 {
		t.Errorf("listed %d facts, want 5:\n%s", got, out)
	}
	for _, want := range []string{"… 2 more in this revision", "… and 1 later event.", "Listed 5 of 12 matching facts"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if all := renderBlame(b, false, false, 0); strings.Count(all, "    + ") != 12 || strings.Contains(all, "Listed ") {
		t.Errorf("-n 0 must list every fact:\n%s", all)
	}
}
