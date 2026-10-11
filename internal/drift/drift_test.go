package drift

import (
	"errors"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/engine"
)

type fakeSource struct {
	drift engine.Drift
	err   error
	asked []string
}

func (f *fakeSource) Drift(repoPath string) (engine.Drift, error) {
	f.asked = append(f.asked, repoPath)
	return f.drift, f.err
}

// A diff computed over a tree that has moved since its snapshot describes
// neither, and one whose tree cannot be checked is unverified. Both must stop
// the diff from reading as comparable; a clean tree must leave it alone.
func TestAddWarning(t *testing.T) {
	cases := []struct {
		name      string
		source    *fakeSource
		wantParts []string
	}{
		{name: "an unchanged tree adds nothing", source: &fakeSource{}},
		{
			name:      "a moved tree says what moved and which tool to run again",
			source:    &fakeSource{drift: engine.Drift{Modified: []string{"a.go"}, Added: []string{"b.go"}}},
			wantParts: []string{"the working tree has changed", "1 modified, 1 added", "a.go", "re-run generate_snapshot and diff_snapshot again"},
		},
		{
			name:      "a tree that cannot be checked is unverified, not clean",
			source:    &fakeSource{drift: engine.Drift{Unknown: true}},
			wantParts: []string{"could not verify", "recorded no file hashes"},
		},
		{
			name:      "an unknown with a reason gives the reason",
			source:    &fakeSource{drift: engine.Drift{Unknown: true, Reason: "indexed under other ignore globs"}},
			wantParts: []string{"could not verify", "indexed under other ignore globs"},
		},
		// Drift is a caveat on the answer; failing to compute it must not replace the answer.
		{name: "a source that fails adds nothing", source: &fakeSource{err: errors.New("walk failed")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &diff.SnapshotDiff{}
			d.Comparability.Comparable = true
			AddWarning(d, tc.source, "/repo", "diff_snapshot")

			if len(tc.source.asked) != 1 || tc.source.asked[0] != "/repo" {
				t.Errorf("the source was asked about %v, want /repo once", tc.source.asked)
			}
			if len(tc.wantParts) == 0 {
				if len(d.Comparability.Warnings) != 0 || !d.Comparability.Comparable {
					t.Errorf("got warnings %v, comparable %v; want none and comparable",
						d.Comparability.Warnings, d.Comparability.Comparable)
				}
				return
			}
			if len(d.Comparability.Warnings) != 1 {
				t.Fatalf("got %d warnings, want 1: %v", len(d.Comparability.Warnings), d.Comparability.Warnings)
			}
			if d.Comparability.Comparable {
				t.Error("a diff carrying a drift warning must not be comparable")
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(d.Comparability.Warnings[0], part) {
					t.Errorf("warning must say %q; got: %s", part, d.Comparability.Warnings[0])
				}
			}
		})
	}
}

// Only a bounded sample of paths is named, so a large refactor cannot make the
// warning unbounded.
func TestAddWarning_SamplesPaths(t *testing.T) {
	var modified []string
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		modified = append(modified, name+".go")
	}
	d := &diff.SnapshotDiff{}
	AddWarning(d, &fakeSource{drift: engine.Drift{Modified: modified}}, "/repo", "diff_snapshot")

	if len(d.Comparability.Warnings) != 1 {
		t.Fatalf("got %d warnings, want 1", len(d.Comparability.Warnings))
	}
	w := d.Comparability.Warnings[0]
	if !strings.Contains(w, "7 modified") || !strings.Contains(w, "e.go") || strings.Contains(w, "f.go") || !strings.Contains(w, "…") {
		t.Errorf("warning must count all 7, name the first %d and mark the rest elided; got: %s", samplePaths, w)
	}
}

// Nothing to annotate, nobody to ask, or no repository: each is a no-op rather
// than a panic, because callers pass whatever they happen to hold.
func TestAddWarning_MissingInputsAreNoOps(t *testing.T) {
	src := &fakeSource{drift: engine.Drift{Modified: []string{"a.go"}}}

	AddWarning(nil, src, "/repo", "diff_snapshot")
	d := &diff.SnapshotDiff{}
	AddWarning(d, nil, "/repo", "diff_snapshot")
	AddWarning(d, src, "", "diff_snapshot")

	if len(src.asked) != 0 {
		t.Errorf("the source was asked %v; none of these calls should reach it", src.asked)
	}
	if len(d.Comparability.Warnings) != 0 {
		t.Errorf("got warnings %v, want none", d.Comparability.Warnings)
	}
}
