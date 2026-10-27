package history

import (
	"encoding/json"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func symbolLine(t *testing.T, name, file string, line int, body string) string {
	t.Helper()
	f := facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Line: line, Repo: "r"}
	if body != "" {
		f.Props = map[string]any{facts.PropBodyHash: body}
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A symbol that changed between two revisions is one symbol that changed, not a removal
// and an addition, and the body version says whether its code did.
func TestBlamePairs(t *testing.T) {
	keep := symbolLine(t, "app.other", "app.ts", 1, "k1")
	for name, tc := range map[string]struct {
		prev, cur    []string
		needle       string
		want         string
		added, gone  int
		presentDelta int
	}{
		"shifted": {
			prev:   []string{keep, symbolLine(t, "app.f", "app.ts", 10, "b1")},
			cur:    []string{keep, symbolLine(t, "app.f", "app.ts", 11, "b1")},
			needle: "app.f", want: PairShifted,
		},
		"rewritten": {
			prev:   []string{symbolLine(t, "app.f", "app.ts", 10, "b1")},
			cur:    []string{symbolLine(t, "app.f", "app.ts", 10, "b2")},
			needle: "app.f", want: PairRewritten,
		},
		"no version": {
			prev:   []string{symbolLine(t, "app.f", "app.ts", 10, "")},
			cur:    []string{symbolLine(t, "app.f", "app.ts", 12, "b1")},
			needle: "app.f", want: PairChanged,
		},
		"moved, both ends match": {
			prev:   []string{symbolLine(t, "old.f", "old/a.ts", 10, "b1")},
			cur:    []string{symbolLine(t, "new.f", "new/a.ts", 3, "b1")},
			needle: "a.ts", want: PairMoved,
		},
		"moved into the pattern": {
			prev:   []string{symbolLine(t, "old.f", "old/a.ts", 10, "b1")},
			cur:    []string{symbolLine(t, "new.f", "new/a.ts", 3, "b1")},
			needle: "new/", want: PairMoved, presentDelta: 1,
		},
		"moved out of the pattern": {
			prev:   []string{symbolLine(t, "old.f", "old/a.ts", 10, "b1")},
			cur:    []string{symbolLine(t, "new.f", "new/a.ts", 3, "b1")},
			needle: "old/", want: PairMoved, presentDelta: -1,
		},
		"same member body, other parent": {
			prev:   []string{symbolLine(t, "ui.ButtonProps.title", "ui/Button.tsx", 3, "b1")},
			cur:    []string{symbolLine(t, "ui.RadioProps.title", "ui/Radio.tsx", 3, "b1")},
			needle: "title", added: 1, gone: 1,
		},
		"moved with its parent": {
			prev:   []string{symbolLine(t, "app/data.Store.add", "app/data/store.ts", 3, "b1")},
			cur:    []string{symbolLine(t, "pkg/element/src.Store.add", "pkg/element/src/store.ts", 9, "b1")},
			needle: "store.add", want: PairMoved,
		},
		"same body, other name": {
			prev:   []string{symbolLine(t, "old.f", "old/a.ts", 10, "b1")},
			cur:    []string{symbolLine(t, "new.g", "new/a.ts", 3, "b1")},
			needle: "a.ts", added: 1, gone: 1,
		},
		"new symbol": {
			prev:   []string{keep},
			cur:    []string{keep, symbolLine(t, "app.f", "app.ts", 10, "b1")},
			needle: "app.f", added: 1,
		},
	} {
		added, removed := matchDelta(tc.prev, tc.cur, canonicalNeedle(tc.needle))
		added, removed, pairs := pairDelta(added, removed, tc.prev, tc.cur, canonicalNeedle(tc.needle))
		if len(added) != tc.added || len(removed) != tc.gone {
			t.Errorf("%s: %d added, %d removed unpaired, want %d and %d", name, len(added), len(removed), tc.added, tc.gone)
		}
		if tc.want == "" {
			if len(pairs) != 0 {
				t.Errorf("%s: paired %v, want none", name, pairs)
			}
			continue
		}
		if len(pairs) != 1 || pairs[0].Kind != tc.want || pairs[0].net != tc.presentDelta {
			t.Errorf("%s: pairs %+v, want one %s with net %d", name, pairs, tc.want, tc.presentDelta)
		}
	}
}
