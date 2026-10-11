// Package history writes enola's architecture history — the append-only log of
// revisions a repository has passed through. The read side, and the format contract, are
// in pkg/history.
//
// The split is not ceremony. pkg/history must stay importable by anything that only
// wants to LOOK at a history, which means importing nothing but internal/facts and the
// standard library and building with CGO_ENABLED=0. Writing needs the file lock and the
// engine's notion of when a snapshot happened, so it lives here, inside.
package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/enola-labs/enola/internal/filelock"
	pkghistory "github.com/enola-labs/enola/pkg/history"
)

// DefaultWorkingKeep is how many unanchored (dirty-tree or non-git) revisions are kept
// per base commit before the oldest are dropped.
//
// It exists because the agent loop is the heaviest writer this log will ever have. An
// agent that snapshots every thirty seconds through a four-hour session produces ~480
// revisions of one commit, none of which will mean anything once the work is committed —
// and on a large repository that is the difference between a history somebody keeps and
// one they delete. 20 is enough to see the shape of a working session and small enough
// that the session cannot outgrow its own commit.
const DefaultWorkingKeep = 20

// DefaultRevisionKeep is how many revisions the log keeps IN TOTAL before the oldest are
// dropped, committed ones included.
//
// The per-commit working cap bounds one session's residue; nothing bounded the log
// itself, and an append-only file that only ever grows is a history somebody eventually
// deletes whole. 200 revisions is months of committed work at ~600 bytes a line, the
// same order DefaultBlobKeep already settled on for contents — and everything dropped is
// re-derivable by re-snapshotting the commit, because the history is derived, never
// authoritative.
const DefaultRevisionKeep = 200

// Options tunes one append.
type Options struct {
	// WorkingKeep is the per-commit cap on unanchored revisions. Zero means
	// DefaultWorkingKeep; negative means keep everything (for a caller that has decided
	// it wants the full loop, e.g. while debugging enola itself).
	WorkingKeep int

	// RevisionKeep is the total cap on the log: appending past it drops the oldest
	// revisions, whatever their kind. Zero means DefaultRevisionKeep; negative means
	// keep everything.
	RevisionKeep int

	// Contents is the revision's storable payload. Nil records a header-only revision —
	// the timeline still shows it and says what it changed, but `show` cannot reconstruct
	// it. Set when blob storage is on.
	Contents *Contents

	// BlobKeep is roughly how many recent revisions keep their contents. Zero means
	// DefaultBlobKeep; negative keeps every one.
	BlobKeep int
}

func (o Options) workingKeep() int {
	if o.WorkingKeep == 0 {
		return DefaultWorkingKeep
	}
	return o.WorkingKeep
}

func (o Options) revisionKeep() int {
	if o.RevisionKeep == 0 {
		return DefaultRevisionKeep
	}
	return o.RevisionKeep
}

// Append records one revision, and reports whether it was recorded.
//
// recorded=false with a nil error is the ordinary "nothing to record" outcome, not a
// failure: re-running generate_snapshot without touching the tree produces a byte-
// identical graph at the same commit, and a log that grows on every re-run would be
// mostly noise about how often somebody pressed the button.
//
// A commit that moves while the graph stays identical IS recorded. That is a real and
// useful statement — this commit changed no architecture — and it costs one line.
//
// The whole operation is serialized on a lock file: several enola servers on one
// repository is the normal case (one per agent terminal), and two of them appending at
// once would interleave a line, or race the compaction below into losing one.
func Append(root string, e pkghistory.Entry, opts Options) (bool, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return false, fmt.Errorf("creating history dir %s: %w", root, err)
	}
	logPath := filepath.Join(root, pkghistory.LogFileName)

	// Blocking, not single-flight: unlike the session-start pin, losing an append loses
	// data. Waiting is measured in milliseconds — the work under the lock is reading a
	// few hundred KB and writing a line.
	lock, err := filelock.Acquire(logPath)
	if err != nil {
		// Degrade rather than fail the snapshot: an unlocked append is no worse than no
		// history at all, and history must never be able to break a snapshot.
		lock = nil
	}
	defer lock.Release()

	// Read refuses a store whose format marker this build does not know, which is what
	// keeps the append from corrupting a newer layout; a marker-less store is format 1
	// and gets its marker stamped below.
	entries, err := pkghistory.Read(root)
	if err != nil && !isNoHistory(err) {
		return false, err
	}
	if err := stampFormat(root); err != nil {
		log.Printf("[history] warning: could not stamp the format marker: %v", err)
	}

	if last, ok := pkghistory.Last(entries); ok && sameRevision(last, e) {
		return false, nil
	}

	e.Seq = nextSeq(entries)

	// Store the contents before the header, so the log never advertises a blob that was
	// not written. The reverse order would make a crash between the two look like
	// corruption on the next read; this way it looks like a revision that was never
	// recorded, which is what it is.
	//
	// A failure to store contents is NOT a failure to record the revision: the header
	// still belongs in the timeline, and losing the whole revision because a compression
	// step failed would be a worse outcome than losing the ability to replay it.
	if opts.Contents != nil {
		blob, err := writeBlob(root, entries, e, *opts.Contents, opts.BlobKeep)
		if err != nil {
			log.Printf("[history] warning: could not store the contents of revision %s: %v", e.Short(), err)
		} else {
			e.Blob = blob
		}
	}

	// Evicting rewrites the log, so it has to happen before the append rather than as a
	// separate pass — otherwise the line just written would itself be a candidate. The
	// total revision cap composes after the working cap: what survives the session
	// ring still counts against the log's overall bound.
	kept, evicted := evictWorking(entries, e, opts.workingKeep())
	kept, capped := capRevisions(kept, opts.revisionKeep())
	if evicted+capped > 0 {
		if err := rewrite(logPath, append(kept, e)); err != nil {
			return false, err
		}
		return true, nil
	}

	line, err := json.Marshal(e)
	if err != nil {
		return false, fmt.Errorf("encoding history entry: %w", err)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false, fmt.Errorf("opening %s: %w", logPath, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("appending to %s: %w", logPath, err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("closing %s: %w", logPath, err)
	}
	return true, nil
}

// RewriteSummary replaces one entry's summary in place, identified by its revision id and
// sequence number.
//
// It exists for one situation: `initial` is a claim that a revision is where the recorded
// history BEGINS, and a later backfill can add revisions older than it, making the claim
// false after the fact. The entry cannot simply be edited on read — its stored counts are
// absolute rather than a delta, so a reader that ignored the flag would report a repository's
// entire graph as one revision's work.
//
// Rewrites the whole log under the lock, like eviction does. A summary is a few hundred
// bytes and this runs once at the end of a backfill, so the cost is a file write against an
// operation that just spent a minute snapshotting.
func RewriteSummary(root, id string, seq int, summary pkghistory.Summary) error {
	logPath := filepath.Join(root, pkghistory.LogFileName)
	lock, err := filelock.Acquire(logPath)
	if err != nil {
		lock = nil
	}
	defer lock.Release()

	entries, err := pkghistory.Read(root)
	if err != nil {
		return err
	}
	found := false
	for i := range entries {
		if entries[i].ID == id && entries[i].Seq == seq {
			entries[i].Summary = summary
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no revision %s (seq %d) to rewrite", pkghistory.ShortID(id), seq)
	}
	return rewrite(logPath, entries)
}

// sameRevision reports whether a new entry describes the same observation as the last one
// already recorded: the same graph, at the same commit, in the same state of cleanliness.
//
// All three matter. The same graph at a DIFFERENT commit is a commit that changed no
// architecture, which is worth a line. The same graph at the same commit but newly dirty
// (or newly clean) is a different thing to have observed. Only all three together mean
// "this snapshot tells us nothing the last one did not".
func sameRevision(last, e pkghistory.Entry) bool {
	if last.ID != e.ID || last.ID == "" {
		return false
	}
	lg, eg := last.Git, e.Git
	if lg == nil || eg == nil {
		return lg == nil && eg == nil
	}
	return lg.Commit == eg.Commit && lg.Dirty == eg.Dirty
}

// nextSeq returns the ordinal for the next entry. It is the greatest Seq seen plus one
// rather than len(entries)+1, so compaction leaves gaps instead of renumbering revisions
// a user may already have on screen.
func nextSeq(entries []pkghistory.Entry) int {
	highest := 0
	for _, e := range entries {
		if e.Seq > highest {
			highest = e.Seq
		}
	}
	return highest + 1
}

// stampFormat writes the store's format marker when none exists yet. An existing marker
// is never touched: a known one is already right, and an unknown one belongs to the
// build that wrote it — Read has refused before this runs.
func stampFormat(root string) error {
	path := filepath.Join(root, pkghistory.FormatFileName)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", pkghistory.FormatVersion)), 0o644)
}

// capRevisions returns the entries to keep under the total revision cap, and how many
// fell off the front. The incoming entry occupies one slot, so the existing ones may
// fill keep-1 — the same accounting evictWorking does. Only log LINES are dropped here:
// a dropped revision's blob segment goes when gc finds nothing referring to it, which is
// the one order of operations that can never strand a reader.
//
// keep < 0 disables the cap entirely.
func capRevisions(entries []pkghistory.Entry, keep int) ([]pkghistory.Entry, int) {
	if keep < 0 {
		return entries, 0
	}
	surplus := len(entries) - (keep - 1)
	if surplus <= 0 {
		return entries, 0
	}
	return entries[surplus:], surplus
}

// evictWorking returns the entries to keep, and how many were dropped, when appending
// incoming. Only unanchored revisions sharing incoming's base commit are candidates:
// committed revisions are permanent, and another commit's working revisions are not this
// commit's business.
//
// keep < 0 disables eviction entirely.
func evictWorking(entries []pkghistory.Entry, incoming pkghistory.Entry, keep int) ([]pkghistory.Entry, int) {
	if keep < 0 || !incoming.Working() {
		return entries, 0
	}
	commit := incoming.Commit()

	// Index the candidates oldest-first, then mark the oldest surplus for removal. The
	// incoming entry occupies one of the slots, so the existing ones may fill keep-1.
	var candidates []int
	for i, e := range entries {
		if e.Working() && e.Commit() == commit {
			candidates = append(candidates, i)
		}
	}
	surplus := len(candidates) - (keep - 1)
	if surplus <= 0 {
		return entries, 0
	}
	drop := make(map[int]struct{}, surplus)
	for _, i := range candidates[:surplus] {
		drop[i] = struct{}{}
	}
	kept := make([]pkghistory.Entry, 0, len(entries)-surplus)
	for i, e := range entries {
		if _, gone := drop[i]; !gone {
			kept = append(kept, e)
		}
	}
	return kept, surplus
}

// rewrite replaces the log with exactly these entries, staging into a temp file in the
// same directory and renaming over the original.
//
// A truncate-and-rewrite in place would leave a reader holding half a history for as long
// as the write takes, and would lose the whole log outright if the process died mid-way.
// The rename is atomic and same-directory, so it cannot fail across a mount boundary the
// way a cross-filesystem rename would.
func rewrite(logPath string, entries []pkghistory.Entry) error {
	dir := filepath.Dir(logPath)
	tmp, err := os.CreateTemp(dir, ".tmp-log-")
	if err != nil {
		return fmt.Errorf("staging history rewrite in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename has moved it

	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			_ = tmp.Close()
			return fmt.Errorf("encoding history entry: %w", err)
		}
		if _, err := tmp.Write(append(line, '\n')); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("writing history rewrite: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing history rewrite: %w", err)
	}
	if err := os.Rename(tmpName, logPath); err != nil {
		return fmt.Errorf("publishing history rewrite: %w", err)
	}
	return nil
}

// isNoHistory reports whether err is the "nothing recorded yet" sentinel — the state
// every repository starts in.
func isNoHistory(err error) bool { return errors.Is(err, pkghistory.ErrNoHistory) }
