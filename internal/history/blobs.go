package history

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
	pkghistory "github.com/enola-labs/enola/pkg/history"
)

// Segment sizing. Both are constants rather than config: they are storage tuning with no
// user-visible meaning, and a knob nobody can evaluate is worse than a default.
const (
	// segmentLen caps how many revisions chain from one base. Reconstruction applies up to
	// this many patches, each of which is map arithmetic over a few thousand strings, so 64
	// costs single-digit milliseconds; the number that matters is the other side of the
	// trade, where a longer chain means fewer ~128 KB bases on disk.
	segmentLen = 64

	// segmentMaxDeltaRatio starts a fresh segment when one revision's patch exceeds this
	// share of its segment's base. A patch that large means the graph was rewritten — a
	// language enabled, a big refactor landed — and continuing to chain would store most of
	// a snapshot as a diff against something it no longer resembles.
	segmentMaxDeltaRatio = 0.25

	// segmentMinBaseForRatio is the base size below which the ratio is not consulted at
	// all. A ratio is a statement about proportion, and proportion is meaningless when the
	// quantity is small: a 400-byte base "exceeded" by a 200-byte patch is two facts and a
	// line move, not a rewrite, and cutting a segment for it would give every revision of a
	// small graph its own base — costing more than the chaining it avoids, and (found by a
	// test that could then no longer damage a chain, because no chain was ever built)
	// quietly turning the multi-revision path into a series of single-revision ones.
	segmentMinBaseForRatio = 32 * 1024
)

// DefaultBlobKeep is roughly how many recent revisions keep their stored contents.
//
// Headers are ~600 bytes and kept forever; contents are ~4 KB a revision plus a ~128 KB
// base per segment, which is the part that needs a bound. 200 revisions is a few working
// weeks at a size nobody has to think about (~1 MB for a repo the size of this one), and
// everything dropped is re-derivable by re-snapshotting the commit.
const DefaultBlobKeep = 200

// Contents is a snapshot's storable payload.
//
// Handed in by the engine rather than derived here: WriteArtifacts has already serialized
// the facts in order to write facts.jsonl, and re-serializing a few thousand facts to store
// the same bytes again would be waste on the hot path.
type Contents struct {
	// FactLines are the canonical lines of facts.jsonl.
	FactLines []string
	// InsightLines are the canonical insight lines (pkghistory.InsightLines).
	InsightLines []string
	// Receipt is this revision's provenance, stored per revision so a reconstructed pair
	// can be judged comparable exactly as a live pair would be.
	Receipt facts.Receipt
}

// blobState describes where the previous revision was stored and what it held — the parent
// the next patch is computed against.
type blobState struct {
	segment      int
	member       int
	epoch        string
	baseSize     int
	factLines    []string
	insightLines []string
	factsHash    string
}

// writeBlob stores one revision's contents and returns the reference to record on its entry.
//
// entries is the log as already read by Append, so the whole operation stays one pass under
// one lock.
func writeBlob(root string, entries []pkghistory.Entry, e pkghistory.Entry, c Contents, keep int) (*pkghistory.BlobRef, error) {
	state, err := readBlobState(root, entries)
	if err != nil {
		return nil, err
	}

	// Sort on the way in. A reconstruction always comes back sorted (Patch.Apply says so),
	// so hashing the caller's ORDER rather than the canonical one produces a blob that can
	// never verify — it fails the integrity check on first read, reporting damage where
	// there is none. Today's only caller hands over facts.jsonl, which WriteJSONL already
	// sorted, so the bug is invisible in production and waits for the second caller.
	factLines := sortedLines(c.FactLines)
	insightLines := sortedLines(c.InsightLines)

	rev := pkghistory.Revision{
		FactsHash: pkghistory.HashLines(factLines),
		Receipt:   c.Receipt,
	}

	segment, member := state.segment, state.member+1
	factsPatch := pkghistory.ComputePatch(state.factLines, factLines)
	switch {
	case state.segment == 0:
		// No parent to chain from. Number the new segment above every number ever ISSUED,
		// not merely above what is on disk: a history whose segment directories were
		// removed — by hand, by a restored backup, by a half-finished copy between
		// machines — would otherwise restart at 1 and hand an address to a new revision
		// that older entries still point at. The log is what survives that, and the log
		// records every reference it ever handed out. pkg/history.Load checks identity on
		// the way out too; this is what stops the collision being created in the first
		// place.
		segment, member = nextSegment(root, entries), 1
	case member > segmentLen,
		state.epoch != e.Epoch,
		state.baseSize >= segmentMinBaseForRatio &&
			float64(factsPatch.Size()) > segmentMaxDeltaRatio*float64(state.baseSize):
		segment, member = state.segment+1, 1
	}

	if member == 1 {
		// A new segment's first revision is a patch against the empty set. Not a special
		// case bolted on — it is what "a patch against nothing" already means — so the
		// encoder, decoder and apply path stay single.
		rev.Facts = pkghistory.ComputePatch(nil, factLines)
		rev.Insights = pkghistory.ComputePatch(nil, insightLines)
	} else {
		rev.Parent = state.factsHash
		rev.Facts = factsPatch
		rev.Insights = pkghistory.ComputePatch(state.insightLines, insightLines)
	}

	if err := writeRevisionFile(pkghistory.RevisionPath(root, segment, member), rev); err != nil {
		return nil, err
	}
	if err := pruneSegments(root, keep); err != nil {
		return nil, err
	}
	return &pkghistory.BlobRef{Segment: segment, Member: member}, nil
}

// readBlobState finds the newest revision with stored contents and reconstructs its lines.
//
// The lines come from LoadLines — the stored strings themselves — and never from
// re-marshaling reconstructed facts. Re-marshaling would put a round trip back into the
// write path, where a byte difference is written into the history rather than caught on the
// way out of it.
func readBlobState(root string, entries []pkghistory.Entry) (blobState, error) {
	for i := len(entries) - 1; i >= 0; i-- {
		prev := entries[i]
		if prev.Blob == nil {
			continue
		}
		factLines, insightLines, _, err := pkghistory.LoadLines(root, prev.Blob.Segment, prev.Blob.Member)
		if err != nil {
			if errors.Is(err, pkghistory.ErrThinned) {
				// Its contents were pruned, and pruning drops whole segments oldest-first,
				// so nothing older can serve as a parent either. Start a fresh segment.
				break
			}
			return blobState{}, fmt.Errorf("reading the previous revision's contents: %w", err)
		}
		size, err := baseSizeOf(root, prev.Blob.Segment)
		if err != nil {
			return blobState{}, err
		}
		return blobState{
			segment:      prev.Blob.Segment,
			member:       prev.Blob.Member,
			epoch:        prev.Epoch,
			baseSize:     size,
			factLines:    factLines,
			insightLines: insightLines,
			factsHash:    pkghistory.HashLines(factLines),
		}, nil
	}
	return blobState{}, nil
}

// sortedLines returns a sorted copy, leaving the caller's slice alone.
func sortedLines(lines []string) []string {
	out := append([]string(nil), lines...)
	sort.Strings(out)
	return out
}

// nextSegment returns a segment number that has never been issued: one above the highest
// present on disk AND the highest any log entry refers to. Numbers are never reused, so a
// BlobRef stays unambiguous for as long as the entry carrying it exists.
func nextSegment(root string, entries []pkghistory.Entry) int {
	highest := 0
	if segments, err := pkghistory.Segments(root); err == nil && len(segments) > 0 {
		highest = segments[len(segments)-1]
	}
	for _, e := range entries {
		if e.Blob != nil && e.Blob.Segment > highest {
			highest = e.Blob.Segment
		}
	}
	return highest + 1
}

// baseSizeOf returns the uncompressed patch size of a segment's first revision, the
// denominator for the delta-ratio cut. Zero with no error when it cannot be determined,
// which disables the ratio check rather than cutting a segment on a guess.
func baseSizeOf(root string, segment int) (int, error) {
	rev, err := pkghistory.ReadRevisionFile(pkghistory.RevisionPath(root, segment, 1))
	if err != nil {
		if errors.Is(err, pkghistory.ErrThinned) {
			return 0, nil
		}
		return 0, err
	}
	return rev.Facts.Size(), nil
}

// writeRevisionFile encodes, gzips and publishes one revision, staging into a temp file and
// renaming, so a reader never observes a half-written blob and a failure leaves nothing
// behind to be mistaken for one.
func writeRevisionFile(path string, rev pkghistory.Revision) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	var raw bytes.Buffer
	if err := rev.Encode(&raw); err != nil {
		return err
	}
	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		_ = zw.Close()
		return fmt.Errorf("compressing revision: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("compressing revision: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-rev-")
	if err != nil {
		return fmt.Errorf("staging revision in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(gzipped.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing revision: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing revision: %w", err)
	}
	return os.Rename(tmpName, path)
}

// pruneSegments drops the oldest segments once more than keep revisions' worth of contents
// are stored.
//
// WHOLE segments, because a segment is a chain: dropping its first revision strands every
// member after it, and dropping one from the middle strands everything downstream. The
// segment is the only unit removable without damaging what remains — which is also why a
// pruned revision reads as thinned rather than as corruption.
//
// The window counts REVISIONS, by asking the log how many members each segment actually
// holds, and keeps whole segments until that count reaches `keep`.
//
// It first approximated instead: keep ceil(keep/segmentLen)+1 segments, on the reasoning
// that a segment holds at most segmentLen revisions so this "over-retains rather than
// under-retains". That is true only when segments are FULL, and they very often are not —
// the delta-ratio cut ends a segment early whenever a revision changes a lot, which in a
// growing repository is most of them. Measured on a real 90-revision backfill: segments
// averaged 8 members, so keeping 4 of them retained 12 revisions against a promise of 200,
// and 78 revisions were silently unreplayable. Off by a factor of sixteen, in the direction
// the comment claimed was impossible.
//
// keep < 0 disables pruning.
func pruneSegments(root string, keep int) error {
	if keep < 0 {
		return nil
	}
	if keep == 0 {
		keep = DefaultBlobKeep
	}
	entries, err := pkghistory.Read(root)
	if err != nil {
		return nil // nothing recorded yet; nothing to prune against
	}
	segments, err := pkghistory.Segments(root)
	if err != nil {
		return err
	}

	// How many revisions each segment accounts for, from the log rather than from the
	// files: the log is the record of what was stored where, and it survives a segment
	// being removed.
	members := map[int]int{}
	for _, e := range entries {
		if e.Blob != nil {
			members[e.Blob.Segment]++
		}
	}

	kept := 0
	keepFrom := len(segments) // index of the oldest segment inside the window
	for i := len(segments) - 1; i >= 0; i-- {
		if kept >= keep {
			break
		}
		kept += members[segments[i]]
		keepFrom = i
	}
	for _, seg := range segments[:keepFrom] {
		if err := os.RemoveAll(pkghistory.SegmentDir(root, seg)); err != nil {
			return fmt.Errorf("pruning segment %d: %w", seg, err)
		}
	}
	return nil
}
