package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/history"
)

// A blame pattern as broad as a repository name matches every fact in it; listed as raw
// JSON that was megabytes, which the client dropped. The answer lists short lines up to a
// cap and counts the rest.
func TestRenderBlameResult_ShortAndCapped(t *testing.T) {
	fact := func(i int) string {
		return fmt.Sprintf(`{"kind":"symbol","name":"pkg.F%d","file":"pkg/f.go","line":%d,"props":{"body_hash":"abc"}}`, i, i)
	}
	ev := func(id string, n int) history.BlameEvent {
		e := history.BlameEvent{Entry: history.Entry{ID: "sha256:" + id, At: "2026-11-01T10:00:00Z"}}
		for i := 1; i <= n; i++ {
			e.Added = append(e.Added, fact(i))
		}
		return e
	}
	b := &history.Blame{Pattern: "pkg", Scanned: 3, Events: []history.BlameEvent{
		ev("aaaaaaa1", 3), ev("bbbbbbb2", 4), ev("ccccccc3", 5),
	}}

	out := renderBlameResult(b, 5)
	if strings.Contains(out, `"props"`) {
		t.Errorf("facts must be listed by kind, name and file, not as JSON:\n%s", out)
	}
	if !strings.Contains(out, "+ symbol      pkg.F1  (pkg/f.go:1)") {
		t.Errorf("want the short line form:\n%s", out)
	}
	if got := strings.Count(out, "\n  + "); got != 5 {
		t.Errorf("listed %d facts, want the cap of 5:\n%s", got, out)
	}
	for _, want := range []string{
		"… 2 more in this revision",
		"… and 1 later revision(s) with matching changes.",
		"Listed 5 of 12 matching facts",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ccccccc") {
		t.Errorf("a revision past the cap must be counted, not headed:\n%s", out)
	}

	if full := renderBlameResult(b, 100); strings.Contains(full, "more in this revision") || strings.Contains(full, "Listed ") {
		t.Errorf("under the cap nothing is elided:\n%s", full)
	}
}
