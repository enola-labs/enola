package history

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	pkghistory "github.com/enola-labs/enola/pkg/history"
)

// GCOptions selects what a collection pass may remove. The zero value removes only
// GARBAGE — segment directories no entry refers to — because that is the one category
// whose removal cannot lose anything a reader could still reach.
type GCOptions struct {
	// DryRun computes the report and changes nothing.
	DryRun bool

	// ThinOlderThan drops the stored CONTENTS of revisions older than this, keeping their
	// header lines. Zero means keep everything.
	ThinOlderThan time.Duration

	// PruneWorking removes unanchored revisions (dirty trees, non-git snapshots) from the
	// log entirely — the agent-loop residue, which is the bulk of a busy history and the
	// least interesting part of it afterwards.
	PruneWorking bool

	// PruneClusterCopies removes multi-repo revisions from the log. In a repository's own
	// history those are copies: a cluster snapshot was once appended to every member's
	// history, and a cluster now has a history of its own. Never set it for a cluster's
	// history, where every revision is multi-repo.
	PruneClusterCopies bool

	// Now is the clock, injectable so a test can age a fixture without sleeping.
	Now time.Time
}

func (o GCOptions) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

// GCReport is what a collection pass found and did. Counts describe the state BEFORE the
// pass; the Removed fields describe the pass itself, so a dry run and a real run report
// the same numbers and differ only in whether anything happened.
type GCReport struct {
	Revisions  int // entries in the log
	Replayable int // entries whose contents can still be loaded
	Thinned    int // entries whose contents are already gone
	Working    int // unanchored revisions
	MultiRepo  int // revisions of a multi-repo graph
	Segments   int // segment directories on disk

	OrphanSegments  []int // referred to by no entry — garbage from an interrupted write or a hand edit
	ThinnedSegments []int // dropped because every member is older than ThinOlderThan
	PrunedEntries   int   // log lines removed

	BytesBefore int64
	BytesFreed  int64
}

// GC removes what a history no longer needs, and reports what it found.
//
// Segments are the only unit of removal, because a segment is a chain: dropping its first
// revision strands every member after it. That constraint is why thinning is expressed in
// whole segments and why pruning a working revision frees no bytes on its own — it removes
// a log line, and the segment goes only when nothing refers to it any more.
//
// It exists because the alternative turned out to be a hand-written script. Repairing a
// history twice during development — once after a backfill wrote a false timeline, once
// after a retention bug stranded most of it — meant rewriting log.jsonl and deleting
// directories by hand, against a format whose invariants are precisely what a hand edit
// gets wrong.
func GC(root string, opts GCOptions) (GCReport, error) {
	var rep GCReport

	entries, err := pkghistory.Read(root)
	if err != nil {
		if isNoHistory(err) {
			return rep, nil
		}
		return rep, err
	}
	segments, err := pkghistory.Segments(root)
	if err != nil {
		return rep, err
	}
	rep.Revisions, rep.Segments = len(entries), len(segments)
	rep.BytesBefore = segmentBytes(root, segments)

	live := map[int]bool{}
	for _, s := range segments {
		live[s] = true
	}
	for _, e := range entries {
		if e.Working() {
			rep.Working++
		}
		if len(e.Repos) > 1 {
			rep.MultiRepo++
		}
		switch {
		case e.Blob == nil, !live[e.Blob.Segment]:
			rep.Thinned++
		default:
			rep.Replayable++
		}
	}

	// 1. Pruning first: it decides which entries remain, and therefore which segments are
	//    still referred to.
	kept := entries
	if opts.PruneWorking || opts.PruneClusterCopies {
		kept = kept[:0:0]
		for _, e := range entries {
			if (opts.PruneWorking && e.Working()) || (opts.PruneClusterCopies && len(e.Repos) > 1) {
				rep.PrunedEntries++
				continue
			}
			kept = append(kept, e)
		}
	}

	// 2. Age thinning: a segment goes only when EVERY member is older than the cutoff,
	//    since one member inside the window needs the whole chain.
	referenced := map[int][]pkghistory.Entry{}
	for _, e := range kept {
		if e.Blob != nil {
			referenced[e.Blob.Segment] = append(referenced[e.Blob.Segment], e)
		}
	}
	if opts.ThinOlderThan > 0 {
		cutoff := opts.now().Add(-opts.ThinOlderThan)
		for _, seg := range segments {
			members := referenced[seg]
			if len(members) == 0 {
				continue // an orphan; handled below
			}
			allOld := true
			for _, m := range members {
				at, err := time.Parse(time.RFC3339, m.At)
				if err != nil || !at.Before(cutoff) {
					allOld = false
					break
				}
			}
			if allOld {
				rep.ThinnedSegments = append(rep.ThinnedSegments, seg)
				delete(referenced, seg)
			}
		}
	}

	// 3. Orphans: on disk, referred to by nothing that remains.
	for _, seg := range segments {
		if _, ok := referenced[seg]; !ok && !contains(rep.ThinnedSegments, seg) {
			rep.OrphanSegments = append(rep.OrphanSegments, seg)
		}
	}

	rep.BytesFreed = segmentBytes(root, append(append([]int(nil), rep.OrphanSegments...), rep.ThinnedSegments...))
	if opts.DryRun {
		return rep, nil
	}

	for _, seg := range append(append([]int(nil), rep.OrphanSegments...), rep.ThinnedSegments...) {
		if err := os.RemoveAll(pkghistory.SegmentDir(root, seg)); err != nil {
			return rep, fmt.Errorf("removing segment %d: %w", seg, err)
		}
	}
	if rep.PrunedEntries > 0 {
		if err := rewrite(filepath.Join(root, pkghistory.LogFileName), kept); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func contains(ss []int, n int) bool {
	for _, s := range ss {
		if s == n {
			return true
		}
	}
	return false
}

// segmentBytes totals the on-disk size of the named segments.
func segmentBytes(root string, segments []int) int64 {
	var total int64
	for _, seg := range segments {
		dir := pkghistory.SegmentDir(root, seg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
	}
	return total
}
