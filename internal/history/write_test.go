package history

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	pkghistory "github.com/enola-labs/enola/pkg/history"
)

func rev(id, at, commit string, dirty bool) pkghistory.Entry {
	return pkghistory.Entry{
		ID:   "sha256:" + id,
		Repo: "github.com/org/repo",
		At:   at,
		Git:  &facts.GitInfo{Commit: commit, Ref: "main", Dirty: dirty},
	}
}

func mustAppend(t *testing.T, root string, e pkghistory.Entry, opts Options) bool {
	t.Helper()
	recorded, err := Append(root, e, opts)
	if err != nil {
		t.Fatalf("append %s: %v", e.ID, err)
	}
	return recorded
}

func readAll(t *testing.T, root string) []pkghistory.Entry {
	t.Helper()
	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return entries
}

func TestAppend_CreatesTheHistoryAndAssignsSeq(t *testing.T) {
	root := filepath.Join(t.TempDir(), "history") // not yet created
	mustAppend(t, root, rev("aaa1", "2026-08-01T10:00:00Z", "c1", false), Options{})
	mustAppend(t, root, rev("bbb2", "2026-08-02T10:00:00Z", "c2", false), Options{})

	got := readAll(t, root)
	if len(got) != 2 {
		t.Fatalf("want 2 revisions, got %d", len(got))
	}
	if got[0].Seq != 1 || got[1].Seq != 2 {
		t.Errorf("want seq 1,2 got %d,%d", got[0].Seq, got[1].Seq)
	}
}

// Re-running generate_snapshot without touching the tree produces a byte-identical graph
// at the same commit. Recording that would make the log mostly a record of how often
// somebody pressed the button.
func TestAppend_ARerunThatChangedNothingIsNotRecorded(t *testing.T) {
	root := t.TempDir()
	e := rev("aaa1", "2026-08-01T10:00:00Z", "c1", false)

	if !mustAppend(t, root, e, Options{}) {
		t.Fatal("the first revision must be recorded")
	}
	again := e
	again.At = "2026-08-01T10:05:00Z" // later run, same graph, same commit
	if mustAppend(t, root, again, Options{}) {
		t.Error("an identical re-run must not be recorded")
	}
	if n := len(readAll(t, root)); n != 1 {
		t.Errorf("want 1 revision, got %d", n)
	}
}

// The converse, and the reason dedup keys on more than the snapshot ID: a commit that
// changed no architecture is a real and useful statement, and it costs one line.
func TestAppend_ACommitThatChangedNoArchitectureIsStillRecorded(t *testing.T) {
	root := t.TempDir()
	mustAppend(t, root, rev("aaa1", "2026-08-01T10:00:00Z", "c1", false), Options{})

	docsOnly := rev("aaa1", "2026-08-01T11:00:00Z", "c2", false) // same graph, new commit
	if !mustAppend(t, root, docsOnly, Options{}) {
		t.Fatal("a moved commit must be recorded even when the graph is identical")
	}
	if n := len(readAll(t, root)); n != 2 {
		t.Errorf("want 2 revisions, got %d", n)
	}
}

// Same graph, same commit, but the tree became dirty: a different thing to have observed.
func TestAppend_DirtyStateChangeIsRecorded(t *testing.T) {
	root := t.TempDir()
	mustAppend(t, root, rev("aaa1", "2026-08-01T10:00:00Z", "c1", false), Options{})
	if !mustAppend(t, root, rev("aaa1", "2026-08-01T10:01:00Z", "c1", true), Options{}) {
		t.Fatal("a clean→dirty transition must be recorded")
	}
}

// The agent loop is the heaviest writer this log will ever have: a four-hour session at
// one snapshot per thirty seconds is ~480 revisions of one commit, none of which will
// mean anything once the work is committed. Unbounded, that is how the feature gets
// uninstalled.
func TestAppend_WorkingRevisionsAreCappedPerCommit(t *testing.T) {
	root := t.TempDir()
	const keep = 3

	mustAppend(t, root, rev("base", "2026-08-01T10:00:00Z", "c1", false), Options{WorkingKeep: keep})
	for _, id := range []string{"w1", "w2", "w3", "w4", "w5"} {
		mustAppend(t, root, rev(id, "2026-08-01T10:0"+id[1:]+":00Z", "c1", true), Options{WorkingKeep: keep})
	}

	got := readAll(t, root)
	var working []string
	for _, e := range got {
		if e.Working() {
			working = append(working, e.ID)
		}
	}
	if len(working) != keep {
		t.Fatalf("want %d working revisions retained, got %d (%v)", keep, len(working), working)
	}
	// The newest survive; the oldest are the ones nobody will ask about.
	if working[len(working)-1] != "sha256:w5" {
		t.Errorf("want the newest working revision retained, got %v", working)
	}
	// The committed revision is permanent and must not be evicted alongside them.
	if got[0].ID != "sha256:base" {
		t.Errorf("the committed revision was evicted: %v", got[0].ID)
	}
}

// Another commit's working revisions are not this commit's business — capping across
// commits would silently delete the record of an earlier session.
func TestAppend_EvictionIsScopedToOneCommit(t *testing.T) {
	root := t.TempDir()
	opts := Options{WorkingKeep: 2}
	mustAppend(t, root, rev("a1", "2026-08-01T10:00:00Z", "c1", true), opts)
	mustAppend(t, root, rev("a2", "2026-08-01T10:01:00Z", "c1", true), opts)
	mustAppend(t, root, rev("b1", "2026-08-01T11:00:00Z", "c2", true), opts)
	mustAppend(t, root, rev("b2", "2026-08-01T11:01:00Z", "c2", true), opts)
	mustAppend(t, root, rev("b3", "2026-08-01T11:02:00Z", "c2", true), opts)

	byCommit := map[string]int{}
	for _, e := range readAll(t, root) {
		byCommit[e.Commit()]++
	}
	if byCommit["c1"] != 2 {
		t.Errorf("c1's working revisions were evicted by c2's: %d left", byCommit["c1"])
	}
	if byCommit["c2"] != 2 {
		t.Errorf("want 2 revisions for c2, got %d", byCommit["c2"])
	}
}

// A directory that is not a git repository is a supported target, and every one of its
// revisions is unanchored — so the same cap has to apply, or a non-git tree accumulates
// one permanent revision per snapshot forever.
func TestAppend_NonGitRevisionsAreCappedToo(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"n1", "n2", "n3", "n4"} {
		e := pkghistory.Entry{ID: "sha256:" + id, Repo: "repo", At: "2026-08-01T10:00:0" + id[1:] + "Z"}
		mustAppend(t, root, e, Options{WorkingKeep: 2})
	}
	if n := len(readAll(t, root)); n != 2 {
		t.Fatalf("want 2 retained, got %d", n)
	}
}

// Negative keeps everything, for someone debugging enola's own loop.
func TestAppend_NegativeKeepDisablesEviction(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"w1", "w2", "w3", "w4"} {
		mustAppend(t, root, rev(id, "2026-08-01T10:00:0"+id[1:]+"Z", "c1", true), Options{WorkingKeep: -1})
	}
	if n := len(readAll(t, root)); n != 4 {
		t.Fatalf("want every working revision kept, got %d", n)
	}
}

// Compaction rewrites the log, so Seq must not be derived from the line count: a user
// looking at @7 on screen must still find @7 after an eviction, and renumbering would
// silently move it to a different revision.
func TestAppend_SeqSurvivesCompaction(t *testing.T) {
	root := t.TempDir()
	opts := Options{WorkingKeep: 2}
	for _, id := range []string{"w1", "w2", "w3"} {
		mustAppend(t, root, rev(id, "2026-08-01T10:00:0"+id[1:]+"Z", "c1", true), opts)
	}
	got := readAll(t, root)
	if len(got) != 2 {
		t.Fatalf("want 2 revisions, got %d", len(got))
	}
	if got[0].Seq != 2 || got[1].Seq != 3 {
		t.Errorf("want the surviving seqs 2,3 (a gap where w1 was), got %d,%d", got[0].Seq, got[1].Seq)
	}
}

// Several enola servers on one repository is the normal case — one per agent terminal.
// Two of them appending at once must not interleave a line or lose a revision.
func TestAppend_ConcurrentWritersLoseNothing(t *testing.T) {
	root := t.TempDir()
	const writers = 8

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct commits, so nothing is deduped or evicted and every append must
			// survive.
			id := string(rune('a'+i)) + "0000000"
			_, _ = Append(root, rev(id, "2026-08-01T10:00:00Z", "c"+id, false), Options{})
		}(i)
	}
	wg.Wait()

	got := readAll(t, root) // parses strictly: an interleaved line fails here
	if len(got) != writers {
		t.Fatalf("want %d revisions, got %d — an append was lost", writers, len(got))
	}
	seen := map[string]bool{}
	for _, e := range got {
		if seen[e.ID] {
			t.Errorf("duplicate revision %s", e.ID)
		}
		seen[e.ID] = true
	}
}

// The lock file is enola's own bookkeeping and must not be mistaken for a log.
func TestAppend_LockFileSitsBesideTheLog(t *testing.T) {
	root := t.TempDir()
	mustAppend(t, root, rev("aaa1", "2026-08-01T10:00:00Z", "c1", false), Options{})

	if _, err := os.Stat(filepath.Join(root, pkghistory.LogFileName+".lock")); err != nil {
		t.Errorf("expected a lock file beside the log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, pkghistory.LogFileName)); err != nil {
		t.Errorf("the log itself is missing: %v", err)
	}
}
