package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enola-labs/enola/internal/facts"
	pkghistory "github.com/enola-labs/enola/pkg/history"
)

// withContents attaches a one-fact payload to an append.
func withContents(o Options, id string, line int) Options {
	o.Contents = contents(id, []string{factLine("A", line)}, nil)
	return o
}

// gcFixture records n revisions, each in its own segment (a fresh epoch every time), which
// is the shape that makes segment-level assertions unambiguous.
func gcFixture(t *testing.T, root string, n int) []pkghistory.Entry {
	t.Helper()
	for i := 0; i < n; i++ {
		e := pkghistory.Entry{
			ID: "sha256:" + string(rune('a'+i)) + "0000000", Repo: "github.com/org/repo",
			At:    time.Now().Add(-time.Duration(n-i) * 24 * time.Hour).UTC().Format(time.RFC3339),
			Epoch: "epoch" + string(rune('a'+i)),
			Git:   gitAt("commit" + string(rune('a'+i))),
		}
		if _, err := Append(root, e, Options{
			WorkingKeep: -1, BlobKeep: -1,
			Contents: contents(e.ID, []string{factLine("A", 10+i)}, nil),
		}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// The default pass removes GARBAGE and nothing else — a segment directory no revision
// refers to, which an interrupted write or a hand edit leaves behind. That is the one
// category whose removal cannot lose anything a reader could still reach.
func TestGC_RemovesOrphanedSegmentsAndNothingElse(t *testing.T) {
	root := t.TempDir()
	entries := gcFixture(t, root, 3)

	// A segment on disk that no entry points at.
	orphan := pkghistory.SegmentDir(root, 999)
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "0001.rev.gz"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := GC(root, GCOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.OrphanSegments) != 1 || rep.OrphanSegments[0] != 999 {
		t.Errorf("want segment 999 collected, got %v", rep.OrphanSegments)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("the orphan is still on disk")
	}
	// Every real revision survives untouched.
	for i, e := range entries {
		if _, err := pkghistory.Load(root, e); err != nil {
			t.Errorf("revision %d became unreadable: %v", i, err)
		}
	}
}

// A dry run must produce the same report and change nothing — otherwise it cannot be used
// to decide whether to run the real thing.
func TestGC_DryRunChangesNothing(t *testing.T) {
	root := t.TempDir()
	gcFixture(t, root, 2)
	orphan := pkghistory.SegmentDir(root, 999)
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	dry, err := GC(root, GCOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.OrphanSegments) != 1 {
		t.Fatalf("dry run must still identify the orphan, got %v", dry.OrphanSegments)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Error("a dry run removed something")
	}

	real, err := GC(root, GCOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(real.OrphanSegments) != len(dry.OrphanSegments) {
		t.Errorf("the real run found %d orphans where the dry run found %d",
			len(real.OrphanSegments), len(dry.OrphanSegments))
	}
}

// Thinning drops contents and keeps the timeline. A revision that loses its blob still has
// its summary line, so the history stays complete and only replay is lost.
func TestGC_ThinningKeepsTheTimeline(t *testing.T) {
	root := t.TempDir()
	gcFixture(t, root, 4) // dated 4, 3, 2 and 1 days ago

	rep, err := GC(root, GCOptions{ThinOlderThan: 36 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ThinnedSegments) == 0 {
		t.Fatal("nothing was thinned despite three revisions being older than the cutoff")
	}

	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("thinning must not remove log lines, got %d of 4", len(entries))
	}
	// The newest is inside the window and must still replay.
	if _, err := pkghistory.Load(root, entries[len(entries)-1]); err != nil {
		t.Errorf("the newest revision was thinned: %v", err)
	}
}

// A segment goes only when EVERY member is past the cutoff: one member inside the window
// needs the whole chain to reconstruct it.
func TestGC_ASegmentWithALiveMemberSurvives(t *testing.T) {
	root := t.TempDir()
	// Two revisions in ONE segment (same epoch), one old and one recent.
	for i, age := range []time.Duration{72 * time.Hour, time.Hour} {
		e := pkghistory.Entry{
			ID: "sha256:" + string(rune('a'+i)) + "0000000", Repo: "r",
			At:    time.Now().Add(-age).UTC().Format(time.RFC3339),
			Epoch: "one-epoch", Git: gitAt("c" + string(rune('a'+i))),
		}
		if _, err := Append(root, e, Options{WorkingKeep: -1, BlobKeep: -1,
			Contents: contents(e.ID, []string{factLine("A", 10+i)}, nil)}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Blob.Segment != entries[1].Blob.Segment {
		t.Skip("the fixture did not chain; the delta-ratio rule cut a segment")
	}

	rep, err := GC(root, GCOptions{ThinOlderThan: 48 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ThinnedSegments) != 0 {
		t.Errorf("a segment holding a revision inside the window must survive, thinned %v", rep.ThinnedSegments)
	}
}

// Working revisions are the agent-loop residue: the bulk of a busy history and the least
// interesting part afterwards. Pruning removes them from the LOG; the segments follow only
// when nothing refers to them, because a segment is a chain.
func TestGC_PruneWorkingRemovesThemFromTheLog(t *testing.T) {
	root := t.TempDir()
	opts := Options{WorkingKeep: -1, BlobKeep: -1}

	committed := pkghistory.Entry{ID: "sha256:c0000000", Repo: "r",
		At: time.Now().UTC().Format(time.RFC3339), Epoch: "e1", Git: gitAt("c1")}
	if _, err := Append(root, committed, withContents(opts, committed.ID, 10)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		w := pkghistory.Entry{ID: "sha256:w" + string(rune('0'+i)) + "000000", Repo: "r",
			At: time.Now().UTC().Format(time.RFC3339), Epoch: "e1",
			Git: &facts.GitInfo{Commit: "c1", Ref: "main", Dirty: true}}
		if _, err := Append(root, w, withContents(opts, w.ID, 20+i)); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := GC(root, GCOptions{PruneWorking: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.PrunedEntries != 3 {
		t.Errorf("want 3 working revisions pruned, got %d", rep.PrunedEntries)
	}
	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != committed.ID {
		t.Errorf("want only the committed revision left, got %+v", entries)
	}
}

func TestGC_EmptyHistoryIsNotAnError(t *testing.T) {
	rep, err := GC(t.TempDir(), GCOptions{})
	if err != nil {
		t.Fatalf("an empty history must be reportable, not an error: %v", err)
	}
	if rep.Revisions != 0 {
		t.Errorf("want nothing reported, got %+v", rep)
	}
}

// The report describes the state BEFORE the pass, so a dry run and a real run agree on the
// numbers and differ only in whether anything happened.
func TestGC_ReportCountsWhatIsThere(t *testing.T) {
	root := t.TempDir()
	gcFixture(t, root, 3)

	rep, err := GC(root, GCOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Revisions != 3 || rep.Replayable != 3 || rep.Thinned != 0 {
		t.Errorf("got revisions=%d replayable=%d thinned=%d, want 3/3/0",
			rep.Revisions, rep.Replayable, rep.Thinned)
	}
	if rep.BytesBefore == 0 {
		t.Error("stored bytes were not measured")
	}
}
