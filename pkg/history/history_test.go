package history

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// The epoch's whole job is to notice when the graph changed because ENOLA changed rather
// than because the code did. Each of these inputs has produced exactly that in this
// repository: a version bump after an extractor fix, a language enabled, an ignore glob
// edited, an explainer toggled.
func TestEpoch_ChangesWithEveryInputThatChangesTheGraph(t *testing.T) {
	base := facts.SnapshotMeta{
		EnolaVersion:     "0.9.1",
		ExtractorVersion: "v146",
		ConfigHash:       "sha256:cfg",
		IgnoreGlobHash:   "sha256:glob",
		Extractors:       []string{"go", "ts"},
		Explainers:       []string{"cycles"},
	}
	want := Epoch(base)

	for name, mutate := range map[string]func(m *facts.SnapshotMeta){
		"version": func(m *facts.SnapshotMeta) { m.EnolaVersion = "0.9.2" },
		// The one that was missing, and the only one that moves for a locally built
		// binary: EnolaVersion is the constant "dev" until a release sets it, so without
		// this an extractor change is invisible to every other field here.
		"extractorVersion": func(m *facts.SnapshotMeta) { m.ExtractorVersion = "v147" },
		"config":           func(m *facts.SnapshotMeta) { m.ConfigHash = "sha256:other" },
		"ignoreGlobs":      func(m *facts.SnapshotMeta) { m.IgnoreGlobHash = "sha256:other" },
		"extractors":       func(m *facts.SnapshotMeta) { m.Extractors = []string{"go", "ts", "swift"} },
		// Explainers change no fact, but findings are keyed by their source, so an
		// explainer present on one side contributes its entire output as a delta — and
		// Summary counts findings.
		"explainers": func(m *facts.SnapshotMeta) { m.Explainers = []string{"cycles", "layers"} },
	} {
		m := base
		mutate(&m)
		if got := Epoch(m); got == want {
			t.Errorf("%s: epoch did not change (%s) — a rebuild would be reported as a code change", name, got)
		}
	}
}

// Extractor order is an artifact of detection order, not a property of the snapshot. Two
// runs that differ only in it are the same epoch, and treating them as different would
// mark a seam on every other line.
func TestEpoch_IgnoresPluginOrder(t *testing.T) {
	a := facts.SnapshotMeta{Extractors: []string{"go", "ts"}, Explainers: []string{"cycles", "layers"}}
	b := facts.SnapshotMeta{Extractors: []string{"ts", "go"}, Explainers: []string{"layers", "cycles"}}
	if Epoch(a) != Epoch(b) {
		t.Fatal("plugin order must not open a new epoch")
	}
}

// The scenario that produced the fix, end to end: two builds reporting the same version,
// the same config and the same plugins, differing only in how their extractors read code.
// Before ExtractorVersion entered the fingerprint these were one epoch, so a graph rewritten
// by an enola change was recorded as a change to the codebase.
func TestEpoch_SeparatesTwoLocalBuildsThatExtractDifferently(t *testing.T) {
	before := facts.SnapshotMeta{
		EnolaVersion: "dev", ExtractorVersion: "v146",
		ConfigHash: "sha256:cfg", Extractors: []string{"go"},
	}
	after := before
	after.ExtractorVersion = "v147"

	if Epoch(before) == Epoch(after) {
		t.Fatal("two builds that extract differently must not share an epoch — the delta between " +
			"them is enola's work, not the author's")
	}
}

func TestHeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Summary
		want string
	}{
		{
			// The first snapshot did not ADD the codebase — it found it. A delta here
			// would credit whoever ran enola first with writing everything.
			"initial",
			Summary{Initial: true, FactCount: 3787, InsightCount: 114},
			"initial: 3787 facts, 114 findings",
		},
		{
			"nothing structural",
			Summary{FactCount: 10},
			"no architectural change",
		},
		{
			// Findings lead: a new cycle is the thing worth noticing on a line that also
			// says "+12 facts".
			"findings lead",
			Summary{FactsAdded: 12, EdgesAdded: 2, FindingsNew: 1},
			"1 new finding · +12 facts · +2 edges",
		},
		{
			"both directions",
			Summary{FactsAdded: 3, FactsRemoved: 2, FactsChanged: 1},
			"+3/-2 facts · ~1 changed",
		},
	} {
		if got := tc.s.Headline(); got != tc.want {
			t.Errorf("%s: Headline() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Retention treats "dirty tree" and "not a git repository" alike, because they are the
// same thing for this purpose: no commit that will still mean anything tomorrow. A
// non-git tree that counted as committed would accumulate one permanent revision per
// snapshot, which on an agent loop is thousands.
func TestWorking(t *testing.T) {
	for _, tc := range []struct {
		name string
		git  *facts.GitInfo
		want bool
	}{
		{"clean commit", &facts.GitInfo{Commit: "abc"}, false},
		{"dirty tree", &facts.GitInfo{Commit: "abc", Dirty: true}, true},
		{"not a git repo", nil, true},
	} {
		if got := (Entry{Git: tc.git}).Working(); got != tc.want {
			t.Errorf("%s: Working() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestShortID(t *testing.T) {
	if got := ShortID("sha256:9a0386c614dcd5c6"); got != "9a0386c" {
		t.Errorf("ShortID = %q, want the first 7 hex digits", got)
	}
	if got := ShortID("abc"); got != "abc" {
		t.Errorf("a short id must survive intact, got %q", got)
	}
}

func TestSummary_Empty(t *testing.T) {
	// Absolute counts are not a delta: a revision that changed nothing still describes a
	// graph with facts in it.
	if !(Summary{FactCount: 3787, InsightCount: 114}).Empty() {
		t.Error("absolute counts must not make a summary look like a change")
	}
	if (Summary{FactsChanged: 1}).Empty() {
		t.Error("a changed fact is a change")
	}
}

func TestEntry_AccessorsTolerateANonGitTree(t *testing.T) {
	// A directory that is not a repository is a supported target, so every accessor has
	// to answer rather than panic.
	e := Entry{ID: "sha256:abcdef01"}
	if e.Commit() != "" || e.Ref() != "" {
		t.Error("a non-git revision must report empty git fields")
	}
	if !strings.HasPrefix(e.Short(), "abcdef0") {
		t.Errorf("Short() = %q", e.Short())
	}
}
