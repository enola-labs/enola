package history

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	pkghistory "github.com/enola-labs/enola/pkg/history"
)

// factLine builds a canonical-looking fact line. The exact shape does not matter — the
// storage layer treats a fact as an opaque string, which is the whole point — but it should
// look like the real thing so the tests exercise realistic sizes and escaping.
func factLine(name string, line int) string {
	return fmt.Sprintf(`{"kind":"symbol","name":%q,"file":"internal/x/x.go","line":%d,"props":{"symbol_kind":"function"}}`, name, line)
}

// contents builds a storable payload whose receipt carries the SAME snapshot id as the
// entry it will be attached to. That correspondence is not decoration: pkg/history.Load
// checks it to catch an entry pointing at somebody else's contents, so a fixture that got
// it wrong would be testing a state the engine cannot produce.
func contents(snapshotID string, factLines []string, insightLines []string) *Contents {
	return &Contents{
		FactLines:    factLines,
		InsightLines: insightLines,
		Receipt: facts.Receipt{
			SnapshotID:   snapshotID,
			EnolaVersion: "test",
			GeneratedAt:  "2026-08-03T10:00:00Z",
			RepoPath:     "/repo",
			Extractors:   []string{"go"},
			Explainers:   []string{"cycles"},
			ConfigHash:   "sha256:cfg",
			FactCount:    len(factLines),
		},
	}
}

// storeRevision appends one revision with contents and returns its entry.
func storeRevision(t *testing.T, root, id, commit string, factLines []string, opts Options) pkghistory.Entry {
	t.Helper()
	e := pkghistory.Entry{
		ID:    "sha256:" + id,
		Repo:  "github.com/org/repo",
		At:    "2026-08-03T10:00:00Z",
		Epoch: "epoch1",
		Git:   &facts.GitInfo{Commit: commit, Ref: "main"},
	}
	opts.Contents = contents(e.ID, factLines, []string{`{"title":"finding","confidence":1}`})
	recorded, err := Append(root, e, opts)
	if err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
	if !recorded {
		t.Fatalf("revision %s was not recorded", id)
	}
	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	return entries[len(entries)-1]
}

// THE test this phase exists to pass. Every stored revision must reconstruct to exactly the
// lines it was given — verified by the hash the revision itself carries, which is the same
// check `enola show` performs. A chain that drifts anywhere fails here, at the revision
// where it drifted, rather than silently rendering a past that never existed.
func TestBlobs_EveryRevisionReconstructsExactly(t *testing.T) {
	root := t.TempDir()
	opts := Options{WorkingKeep: -1}

	// A realistic sequence: growth, a removal, a line-only move (the common case — 65 of
	// 82 changed lines on the real repository), and a revision that changes nothing.
	states := [][]string{
		{factLine("A", 10), factLine("B", 20)},
		{factLine("A", 10), factLine("B", 20), factLine("C", 30)},
		{factLine("A", 10), factLine("C", 30)},
		{factLine("A", 11), factLine("C", 31)}, // line-only move
		{factLine("A", 11), factLine("C", 31)}, // unchanged graph, new commit
	}

	var stored []pkghistory.Entry
	for i, want := range states {
		e := storeRevision(t, root, fmt.Sprintf("%08d", i), fmt.Sprintf("commit%d", i), want, opts)
		stored = append(stored, e)
	}

	// Assert the revisions actually CHAINED. Without this the test passes just as happily
	// when every revision has become its own base, which is exactly what a too-eager
	// segment cut produced — and a round-trip that never applies a patch tests nothing
	// this phase built.
	chained := 0
	for _, e := range stored {
		if e.Blob != nil && e.Blob.Member > 1 {
			chained++
		}
	}
	if chained < len(states)-1 {
		t.Fatalf("only %d of %d revisions chained — the reconstruction path is not being exercised",
			chained, len(states)-1)
	}

	for i, e := range stored {
		if e.Blob == nil {
			t.Fatalf("revision %d has no stored contents", i)
		}
		got, _, _, err := pkghistory.LoadLines(root, e.Blob.Segment, e.Blob.Member)
		if err != nil {
			t.Fatalf("revision %d: %v", i, err)
		}
		if len(got) != len(states[i]) {
			t.Fatalf("revision %d reconstructed %d lines, want %d", i, len(got), len(states[i]))
		}
		want := pkghistory.HashLines(sortedCopy(states[i]))
		if gotHash := pkghistory.HashLines(got); gotHash != want {
			t.Errorf("revision %d does not reconstruct exactly:\n got %v\nwant %v", i, got, states[i])
		}
	}
}

func sortedCopy(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

// Load returns a real Snapshot, with the receipt's provenance carried through — that is
// what lets diff.compareMeta judge two reconstructed revisions comparable exactly as it
// would judge two live ones.
func TestBlobs_LoadReturnsAUsableSnapshot(t *testing.T) {
	root := t.TempDir()
	e := storeRevision(t, root, "aaaa1111", "c1",
		[]string{factLine("A", 10), factLine("B", 20)}, Options{})

	snap, err := pkghistory.Load(root, e)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Facts) != 2 {
		t.Fatalf("want 2 facts, got %d", len(snap.Facts))
	}
	if snap.Facts[0].Name != "A" || snap.Facts[0].Line != 10 {
		t.Errorf("facts did not survive as facts: %+v", snap.Facts[0])
	}
	if len(snap.Insights) != 1 {
		t.Errorf("want the insight back, got %d", len(snap.Insights))
	}
	if snap.Meta.EnolaVersion != "test" || snap.Meta.ConfigHash != "sha256:cfg" {
		t.Errorf("provenance lost — compareMeta cannot judge this pair: %+v", snap.Meta)
	}
}

// A new epoch must start a fresh segment. Chaining a patch across a rebuild would store the
// churn of a different enola as though it were a change to the code, and the base it
// chained from describes a graph the new extractor no longer produces.
func TestBlobs_EpochChangeCutsASegment(t *testing.T) {
	root := t.TempDir()
	opts := Options{WorkingKeep: -1}

	first := storeRevision(t, root, "aaaa1111", "c1", []string{factLine("A", 10)}, opts)

	e := pkghistory.Entry{
		ID:    "sha256:bbbb2222",
		Repo:  "github.com/org/repo",
		At:    "2026-08-03T11:00:00Z",
		Epoch: "epoch2", // a different enola / config / extractor set
		Git:   &facts.GitInfo{Commit: "c2", Ref: "main"},
	}
	opts.Contents = contents(e.ID, []string{factLine("A", 10), factLine("B", 20)}, nil)
	if _, err := Append(root, e, opts); err != nil {
		t.Fatal(err)
	}

	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	second := entries[len(entries)-1]
	if second.Blob.Segment == first.Blob.Segment {
		t.Fatalf("an epoch change must open a new segment (both in %d)", first.Blob.Segment)
	}
	if second.Blob.Member != 1 {
		t.Errorf("the first revision of a new segment must be member 1, got %d", second.Blob.Member)
	}
}

// Retention drops WHOLE segments: a segment is a chain, so removing its first revision
// strands every member after it and removing one from the middle strands everything
// downstream.
func TestBlobs_PruningDropsWholeSegments(t *testing.T) {
	root := t.TempDir()

	// keep=0 with segmentLen=64 retains 1 segment beyond the newest; force several
	// segments by changing the epoch each time, then prune hard.
	for i := 0; i < 4; i++ {
		e := pkghistory.Entry{
			ID:    fmt.Sprintf("sha256:%08d", i),
			Repo:  "github.com/org/repo",
			At:    fmt.Sprintf("2026-08-03T1%d:00:00Z", i),
			Epoch: fmt.Sprintf("epoch%d", i),
			Git:   &facts.GitInfo{Commit: fmt.Sprintf("c%d", i), Ref: "main"},
		}
		if _, err := Append(root, e, Options{
			WorkingKeep: -1,
			BlobKeep:    1, // one segment's worth
			Contents:    contents(e.ID, []string{factLine("A", 10+i)}, nil),
		}); err != nil {
			t.Fatal(err)
		}
	}

	segments, err := pkghistory.Segments(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) > 2 {
		t.Fatalf("want the window bounded to ~2 segments, got %v", segments)
	}

	// The pruned revisions keep their header and report themselves as thinned — a state a
	// caller must be able to tell apart from damage.
	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("pruning contents must not remove headers: got %d entries", len(entries))
	}
	if _, err := pkghistory.Load(root, entries[0]); err == nil {
		t.Error("the oldest revision's contents should be gone")
	} else if !isThinned(err) {
		t.Errorf("a pruned revision must read as thinned, not as damage: %v", err)
	}

	// …and the newest must still reconstruct.
	if _, err := pkghistory.Load(root, entries[len(entries)-1]); err != nil {
		t.Errorf("the newest revision must still reconstruct: %v", err)
	}
}

func isThinned(err error) bool { return errors.Is(err, pkghistory.ErrThinned) }

// Corruption must be REPORTED, at the revision where it happened. A damaged member that
// merely produced a different snapshot would render a past that never existed.
func TestBlobs_DamageIsReportedNotRendered(t *testing.T) {
	root := t.TempDir()
	opts := Options{WorkingKeep: -1}
	storeRevision(t, root, "aaaa1111", "c1", []string{factLine("A", 10)}, opts)
	second := storeRevision(t, root, "bbbb2222", "c2", []string{factLine("A", 10), factLine("B", 20)}, opts)

	// Overwrite the second member with the first member's bytes: valid gzip, valid format,
	// wrong contents — the failure a hash check exists to catch and a format check cannot.
	path := pkghistory.RevisionPath(root, second.Blob.Segment, second.Blob.Member)
	src, err := os.ReadFile(pkghistory.RevisionPath(root, second.Blob.Segment, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := pkghistory.Load(root, second); err == nil {
		t.Fatal("a damaged chain must not reconstruct silently")
	}
}

// A stored revision must be the one an entry REFERS to, not merely a well-formed revision
// at that address.
//
// A BlobRef is a position, and a position only means something while the numbering it was
// issued under holds. Remove a segment by hand or restore a partial backup, and a later
// revision can land at an address an older entry still points to — at which point every
// check downstream agrees: valid file, valid internal hash, wrong revision. The symptom is
// silence, which is how this was found: `show` reporting that a revision adding 82 facts
// changed nothing, because it had diffed that revision against itself.
func TestBlobs_AStaleReferenceIsCaughtNotFollowed(t *testing.T) {
	root := t.TempDir()
	opts := Options{WorkingKeep: -1}
	first := storeRevision(t, root, "aaaa1111", "c1", []string{factLine("A", 10)}, opts)

	// Simulate the disturbance: wipe the segments, keep the log. The next revision must
	// NOT be written where `first` still points.
	if err := os.RemoveAll(filepath.Join(root, pkghistory.SegDirName)); err != nil {
		t.Fatal(err)
	}
	second := storeRevision(t, root, "bbbb2222", "c2", []string{factLine("B", 20)}, opts)

	if second.Blob.Segment == first.Blob.Segment && second.Blob.Member == first.Blob.Member {
		t.Fatalf("the new revision reused the address %d/%d that an older entry still points to",
			first.Blob.Segment, first.Blob.Member)
	}
	// And even if an address were reused, loading through the stale entry must fail loudly
	// rather than hand back somebody else's snapshot.
	stale := first
	stale.Blob = second.Blob
	if _, err := pkghistory.Load(root, stale); err == nil {
		t.Error("loading a revision through a mismatched blob reference must be an error")
	}
}

// Blob storage is optional. Without contents the revision is still recorded — the timeline
// keeps it and says what it changed — and only replay is unavailable.
func TestBlobs_HeaderOnlyRevisionIsStillRecorded(t *testing.T) {
	root := t.TempDir()
	e := pkghistory.Entry{
		ID:    "sha256:aaaa1111",
		Repo:  "github.com/org/repo",
		At:    "2026-08-03T10:00:00Z",
		Epoch: "epoch1",
		Git:   &facts.GitInfo{Commit: "c1", Ref: "main"},
	}
	if _, err := Append(root, e, Options{}); err != nil {
		t.Fatal(err)
	}
	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want the revision recorded, got %d", len(entries))
	}
	if entries[0].Blob != nil {
		t.Error("no contents were supplied, so no blob should be referenced")
	}
	if _, err := os.Stat(filepath.Join(root, pkghistory.SegDirName)); !os.IsNotExist(err) {
		t.Error("no segment directory should exist")
	}
}

// A revision that changes nothing structural still stores, and its patch is empty — the
// cheapest possible entry, which is what makes recording every snapshot affordable.
func TestBlobs_AnUnchangedGraphStoresAnEmptyPatch(t *testing.T) {
	root := t.TempDir()
	same := []string{factLine("A", 10)}
	opts := Options{WorkingKeep: -1}
	storeRevision(t, root, "aaaa1111", "c1", same, opts)
	second := storeRevision(t, root, "aaaa1111", "c2", same, opts) // same graph, new commit

	rev, err := pkghistory.ReadRevisionFile(pkghistory.RevisionPath(root, second.Blob.Segment, second.Blob.Member))
	if err != nil {
		t.Fatal(err)
	}
	if !rev.Facts.Empty() {
		t.Errorf("want an empty fact patch, got add=%v del=%v", rev.Facts.Add, rev.Facts.Del)
	}
	if rev.Parent == "" {
		t.Error("a chained member must record its parent")
	}
}

// gitAt is a committed git state at one commit, for fixtures that build entries directly.
func gitAt(commit string) *facts.GitInfo {
	return &facts.GitInfo{Commit: commit, Ref: "main"}
}

// Contents may arrive in any order, because a reconstruction always comes back SORTED.
//
// writeBlob used to hash the caller's order while Apply returned the canonical one, so a
// caller handing over unsorted lines wrote a blob that failed its own integrity check the
// first time it was read — reporting DAMAGE for a history that was perfectly intact. It
// stayed invisible because the only caller passes facts.jsonl, which WriteJSONL has already
// sorted; it surfaced from a test fixture that happened to list a dependency after a symbol.
//
// Asserted explicitly rather than left to the fixture that found it: a defect whose only
// coverage is incidental fails, when it returns, as somebody else's test breaking for a
// reason that names the wrong thing.
func TestBlobs_ContentsNeedNotArriveSorted(t *testing.T) {
	root := t.TempDir()

	// Deliberately reversed: "dependency" sorts before "symbol", so this is not canonical.
	dep := `{"kind":"dependency","name":"x -> y","file":"pkg/x/x.go","line":3}`
	unsorted := []string{factLine("A", 10), dep}

	e := pkghistory.Entry{
		ID: "sha256:aaaa1111", Repo: "github.com/org/repo",
		At: "2026-08-03T10:00:00Z", Epoch: "epoch1", Git: gitAt("c1"),
	}
	if _, err := Append(root, e, Options{WorkingKeep: -1, Contents: contents(e.ID, unsorted, nil)}); err != nil {
		t.Fatal(err)
	}

	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	// The integrity check runs inside Load; an order-sensitive hash fails here.
	snap, err := pkghistory.Load(root, entries[0])
	if err != nil {
		t.Fatalf("a revision stored from unsorted input must still verify: %v", err)
	}
	if len(snap.Facts) != 2 {
		t.Errorf("want both facts back, got %d", len(snap.Facts))
	}
}

// Retention promises REVISIONS and must deliver revisions, not segments.
//
// It first kept ceil(keep/segmentLen)+1 segments, reasoning that a segment holds at most
// segmentLen revisions so the approximation could only over-retain. Segments are frequently
// cut early by the delta-ratio rule, so they are often far from full: on a real 90-revision
// backfill they averaged 8 members, and keeping 4 segments retained 12 revisions against a
// promised 200.
func TestBlobs_RetentionCountsRevisionsNotSegments(t *testing.T) {
	root := t.TempDir()

	// 12 revisions, each in its own segment (a new epoch every time), so segments are as
	// far from full as they can be — the case the old approximation got wrong.
	for i := 0; i < 12; i++ {
		e := pkghistory.Entry{
			ID: fmt.Sprintf("sha256:%08d", i), Repo: "github.com/org/repo",
			At:    fmt.Sprintf("2026-08-03T%02d:00:00Z", i),
			Epoch: fmt.Sprintf("epoch%d", i), Git: gitAt(fmt.Sprintf("c%d", i)),
		}
		if _, err := Append(root, e, Options{
			WorkingKeep: -1,
			BlobKeep:    10,
			Contents:    contents(e.ID, []string{factLine("A", 10+i)}, nil),
		}); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	replayable := 0
	for _, e := range entries {
		if _, err := pkghistory.Load(root, e); err == nil {
			replayable++
		}
	}
	if replayable < 10 {
		t.Errorf("BlobKeep=10 retained only %d replayable revisions of %d — the window counts "+
			"revisions, and one-member segments must not shrink it", replayable, len(entries))
	}
}
