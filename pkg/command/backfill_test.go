package command

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/history"
)

// The defect that made the first real backfill worthless, and the reason this function
// exists at all rather than being an inline filepath.Join.
//
// A fact's repo label comes from the base name of the directory the engine walked. Extract
// into the scratch directory itself and every revision is labelled with a different random
// name, so no two consecutive commits share a single fact: every patch is a full rewrite,
// every revision gets its own segment, and retention throws most of them away. The timeline
// still LOOKS like a timeline — it reported "+2509/-2457 facts" for days that touched a
// handful of files — which is what makes it worth a test rather than a comment.
func TestExtractionDir_IsNamedAfterTheRepositoryNotTheScratchDir(t *testing.T) {
	scratch := "/tmp/enola-backfill-217483"

	for _, repo := range []string{"/Users/dev/src/enola", "/Users/dev/src/enola/"} {
		got := extractionDir(scratch, repo)
		if filepath.Base(got) != "enola" {
			t.Errorf("extractionDir(%q) = %q; the last segment must be the repository name, "+
				"or every revision gets a different repo label", repo, got)
		}
		if !strings.HasPrefix(got, scratch) {
			t.Errorf("extractionDir(%q) = %q, which is outside the scratch directory", repo, got)
		}
	}

	// Two different scratch directories, one repository: the label must not move.
	a := filepath.Base(extractionDir("/tmp/enola-backfill-1", "/src/enola"))
	b := filepath.Base(extractionDir("/tmp/enola-backfill-2", "/src/enola"))
	if a != b {
		t.Errorf("the repo label changed between runs: %q vs %q", a, b)
	}
}

// One revision per day, and the LAST commit of each day — the state the day ended in, which
// is what somebody scanning a timeline by date is picturing.
func TestOnePerDay(t *testing.T) {
	commits := []commitInfo{
		{SHA: "a", When: "2026-07-11T09:00:00+02:00"},
		{SHA: "b", When: "2026-07-11T18:00:00+02:00"},
		{SHA: "c", When: "2026-07-12T10:00:00+02:00"},
		{SHA: "d", When: "2026-07-14T10:00:00+02:00"},
	}
	got := onePerDay(commits)
	var shas []string
	for _, c := range got {
		shas = append(shas, c.SHA)
	}
	if strings.Join(shas, ",") != "b,c,d" {
		t.Errorf("got %v, want the last commit of each day (b, c, d)", shas)
	}
}

func TestOnePerDay_EmptyAndSingle(t *testing.T) {
	if got := onePerDay(nil); len(got) != 0 {
		t.Errorf("want nothing from nothing, got %d", len(got))
	}
	one := []commitInfo{{SHA: "a", When: "2026-07-11T09:00:00+02:00"}}
	if got := onePerDay(one); len(got) != 1 || got[0].SHA != "a" {
		t.Errorf("a single commit must survive, got %+v", got)
	}
}

// An unknown --sample must be refused rather than silently treated as "all": a caller who
// mistyped it and got a full backfill of ten thousand commits would have no way to tell.
func TestSelectCommits_RejectsAnUnknownSample(t *testing.T) {
	_, err := selectCommits(t.TempDir(), backfillArgs{sample: "weekly"})
	if err == nil {
		t.Fatal("an unknown sample must be an error")
	}
	if !strings.Contains(err.Error(), "weekly") {
		t.Errorf("the error should name what was rejected, got %q", err)
	}
}

// The first backfilled revision has no predecessor, so its counts describe the whole graph
// rather than a change — the same rule the live recorder follows, and for the same reason:
// a delta here would credit whoever ran the backfill with writing the codebase.
func TestSummarizeAgainst_FirstRevisionIsInitial(t *testing.T) {
	cur := &facts.Snapshot{
		Facts:    []facts.Fact{{Kind: "symbol", Name: "A"}, {Kind: "symbol", Name: "B"}},
		Insights: []facts.Insight{{Title: "x"}},
	}
	s := summarizeAgainst(nil, cur)
	if !s.Initial {
		t.Error("the first backfilled revision must be marked initial")
	}
	if s.FactCount != 2 || s.InsightCount != 1 {
		t.Errorf("want absolute counts, got %+v", s)
	}
	if s.FactsAdded != 0 {
		t.Errorf("an initial revision has no delta, got %d added", s.FactsAdded)
	}
}

func TestSummarizeAgainst_ReportsTheDelta(t *testing.T) {
	prev := &facts.Snapshot{Facts: []facts.Fact{{Kind: "symbol", Name: "A", File: "a.go"}}}
	cur := &facts.Snapshot{Facts: []facts.Fact{
		{Kind: "symbol", Name: "A", File: "a.go"},
		{Kind: "symbol", Name: "B", File: "b.go"},
	}}
	s := summarizeAgainst(prev, cur)
	if s.Initial {
		t.Error("a revision with a predecessor is not initial")
	}
	if s.FactsAdded != 1 || s.FactsRemoved != 0 {
		t.Errorf("want +1/-0, got +%d/-%d", s.FactsAdded, s.FactsRemoved)
	}
	if s.ByKind["symbol"] != 1 {
		t.Errorf("want the per-kind breakdown, got %+v", s.ByKind)
	}
}

// A backfilled entry is stamped with the COMMIT's time, not the moment the backfill ran.
// Stamping the run time collapses every revision into one instant: nothing orders, --since
// filters all-or-nothing, and the timeline describes when somebody typed a command rather
// than when the architecture looked like that.
func TestCommitInfo_CarriesItsOwnTimeAndCommit(t *testing.T) {
	c := commitInfo{SHA: "abc123", When: "2026-07-11T13:33:05+02:00", Ref: "backfill"}
	gi := c.gitInfo()
	if gi.Commit != "abc123" {
		t.Errorf("commit lost: %+v", gi)
	}
	e := history.Entry{At: c.When, Git: gi}
	if e.At != "2026-07-11T13:33:05+02:00" {
		t.Errorf("the entry must carry the commit's time, got %q", e.At)
	}
	if e.Working() {
		t.Error("a backfilled commit is committed, not a working revision")
	}
}

func TestSplitFactLines(t *testing.T) {
	if got := splitFactLines(nil); got != nil {
		t.Errorf("want nothing from nothing, got %v", got)
	}
	if got := splitFactLines([]byte("a\nb\n")); len(got) != 2 || got[1] != "b" {
		t.Errorf("the trailing newline must not become an empty line, got %v", got)
	}
}

// A resumed backfill must not plant a false beginning.
//
// `initial` means "this is where the history starts", and it is set when a revision has no
// predecessor. Resuming an interrupted run used to start with none, so the first revision of
// the resumed batch declared itself the start of the timeline — seen for real when
// backfilling cognee's newest 3 release tags and then the remaining 77 left v1.2.1
// announcing itself as the origin of a three-year history.
//
// The walk therefore covers every SELECTED commit up to the last piece of work, loading
// already-recorded ones purely to carry the predecessor forward.
func TestLastTodoIndex_StopsAfterTheFinalOutstandingCommit(t *testing.T) {
	selected := []commitInfo{{SHA: "a"}, {SHA: "b"}, {SHA: "c"}, {SHA: "d"}}

	// b and d outstanding: the walk must reach d (index 3) and no further.
	done := map[string]bool{"a": true, "c": true}
	if got := lastTodoIndex(selected, done); got != 3 {
		t.Errorf("lastTodoIndex = %d, want 3", got)
	}

	// Only the earliest outstanding: everything after it is already recorded and needs no
	// predecessor loaded.
	if got := lastTodoIndex(selected, map[string]bool{"b": true, "c": true, "d": true}); got != 0 {
		t.Errorf("lastTodoIndex = %d, want 0", got)
	}

	// Nothing outstanding at all — a completed re-run does no work.
	all := map[string]bool{"a": true, "b": true, "c": true, "d": true}
	if got := lastTodoIndex(selected, all); got != -1 {
		t.Errorf("lastTodoIndex = %d, want -1 (nothing to do)", got)
	}
}
