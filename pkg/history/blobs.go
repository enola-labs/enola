package history

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
)

// ErrThinned is returned when a revision's stored contents are gone but its header
// remains — either dropped by retention, or recorded before blobs existed.
//
// A distinct error because it is not a failure: the timeline still shows the revision and
// says what it changed, and the contents are re-derivable by re-snapshotting that commit.
// A caller that reports it as corruption sends somebody looking for a bug.
var ErrThinned = errors.New("this revision's contents are no longer stored")

// SegDirName is the directory holding segments inside a history root.
const SegDirName = "seg"

// SegmentDir returns the directory holding one segment's revision files.
func SegmentDir(root string, segment int) string {
	return filepath.Join(root, SegDirName, fmt.Sprintf("%06d", segment))
}

// RevisionPath returns the file holding one revision's stored contents.
func RevisionPath(root string, segment, member int) string {
	return filepath.Join(SegmentDir(root, segment), fmt.Sprintf("%04d.rev.gz", member))
}

// ReadRevisionFile reads and decodes one stored revision.
func ReadRevisionFile(path string) (*Revision, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrThinned
		}
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = gz.Close() }()

	rev, err := DecodeRevision(gz)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rev, nil
}

// Load reconstructs the snapshot an entry recorded.
//
// It applies members 1..N of the entry's segment in order — member 1 being a patch against
// the empty set, so there is no separate "load the base" path — and then verifies the
// result against the facts hash the revision itself carries.
//
// The verification is what makes chaining safe. A chain that has drifted for any reason (a
// dropped member, a corrupt file, a writer bug, a change to how facts serialize) is caught
// exactly here, at the revision where it happened, instead of silently producing a
// plausible snapshot of a past that never existed. It is checked against the facts.jsonl
// hash rather than the snapshot ID because snapshot_id also mixes in the enola version and
// the config hash, neither of which can affect whether the facts were reassembled
// correctly — hashing the reconstructed bytes is the tighter question.
func Load(root string, e Entry) (*facts.Snapshot, error) {
	if e.Blob == nil {
		return nil, ErrThinned
	}
	factLines, insightLines, rev, err := loadLines(root, e.Blob.Segment, e.Blob.Member)
	if err != nil {
		return nil, err
	}

	// The stored revision must be the one this entry refers to, not merely a well-formed
	// revision at that address.
	//
	// A BlobRef is a position — segment and member — and a position is only meaningful
	// while the numbering it was issued under holds. Delete a segment directory by hand,
	// restore a partial backup, or copy half a history between machines, and a later
	// revision can be written at an address an older entry still points to. Everything
	// downstream then agrees: the file is valid, its internal hash checks out, and the
	// caller is handed a snapshot belonging to a different revision. The symptom is
	// silence — `show` reporting that a revision which added eighty-two facts changed
	// nothing, because it diffed that revision against itself.
	//
	// Found exactly that way, in live use, one command after this code was written.
	if rev.Receipt.SnapshotID != "" && e.ID != "" && rev.Receipt.SnapshotID != e.ID {
		return nil, fmt.Errorf(
			"revision %s points at stored contents belonging to %s (segment %d member %d) — the history's segment numbering has been disturbed, so this revision's contents are effectively lost",
			ShortID(e.ID), ShortID(rev.Receipt.SnapshotID), e.Blob.Segment, e.Blob.Member)
	}

	parsedFacts, err := decodeFactLines(factLines)
	if err != nil {
		return nil, err
	}
	insights, err := decodeInsightLines(insightLines)
	if err != nil {
		return nil, err
	}

	return &facts.Snapshot{
		Meta:     metaFrom(rev.Receipt),
		Facts:    parsedFacts,
		Insights: insights,
	}, nil
}

// LoadLines applies the chain up to member and returns the reconstructed canonical lines
// plus the final revision's own record.
//
// Exported because the WRITER needs it: the next patch is computed against the previous
// revision's lines, and those must be the stored strings themselves. Deriving them by
// re-marshaling reconstructed facts would put a marshal round-trip back into the write path
// — precisely the step this format exists to avoid, and one whose failures would be written
// into the history rather than caught on the way out of it.
func LoadLines(root string, segment, member int) (factLines, insightLines []string, last *Revision, err error) {
	return loadLines(root, segment, member)
}

// loadLines applies the chain up to member and returns the canonical lines plus the final
// revision's own record.
func loadLines(root string, segment, member int) (factLines, insightLines []string, last *Revision, err error) {
	for m := 1; m <= member; m++ {
		rev, err := ReadRevisionFile(RevisionPath(root, segment, m))
		if err != nil {
			return nil, nil, nil, err
		}
		if factLines, err = rev.Facts.Apply(factLines); err != nil {
			return nil, nil, nil, fmt.Errorf("segment %d member %d: %w", segment, m, err)
		}
		if insightLines, err = rev.Insights.Apply(insightLines); err != nil {
			return nil, nil, nil, fmt.Errorf("segment %d member %d insights: %w", segment, m, err)
		}
		if got := HashLines(factLines); got != rev.FactsHash {
			return nil, nil, nil, fmt.Errorf(
				"segment %d member %d: reconstruction does not match the recorded facts hash (got %s, want %s) — the stored history is damaged at this revision",
				segment, m, ShortID(got), ShortID(rev.FactsHash))
		}
		last = rev
	}
	if last == nil {
		return nil, nil, nil, fmt.Errorf("segment %d has no member %d", segment, member)
	}
	return factLines, insightLines, last, nil
}

func decodeFactLines(lines []string) ([]facts.Fact, error) {
	out := make([]facts.Fact, 0, len(lines))
	for i, l := range lines {
		var f facts.Fact
		if err := json.Unmarshal([]byte(l), &f); err != nil {
			return nil, fmt.Errorf("decoding reconstructed fact %d: %w", i, err)
		}
		out = append(out, f)
	}
	return out, nil
}

func decodeInsightLines(lines []string) ([]facts.Insight, error) {
	out := make([]facts.Insight, 0, len(lines))
	for i, l := range lines {
		var in facts.Insight
		if err := json.Unmarshal([]byte(l), &in); err != nil {
			return nil, fmt.Errorf("decoding reconstructed insight %d: %w", i, err)
		}
		out = append(out, in)
	}
	return out, nil
}

// InsightLines renders insights as a canonical line multiset, the form patches are computed
// over. Sorted, so the multiset is a pure function of the insight set exactly as
// facts.Store.WriteJSONL makes facts.jsonl a pure function of the fact set.
//
// Insight ORDER is not preserved by a round trip through this, and does not need to be:
// insights take no part in snapshot_id, and diff.Compute groups and sorts findings itself,
// so a reconstructed snapshot produces an identical diff either way.
func InsightLines(insights []facts.Insight) ([]string, error) {
	lines := make([]string, 0, len(insights))
	for _, in := range insights {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encoding insight %q: %w", in.Title, err)
		}
		lines = append(lines, string(b))
	}
	sort.Strings(lines)
	return lines, nil
}

// metaFrom rebuilds the SnapshotMeta fields diff.compareMeta reads from a stored receipt,
// so a reconstructed pair can be judged comparable exactly as a live pair would be.
//
// The counting and per-file fields stay zero: the receipt does not carry them (it omits the
// per-file hash list by design), and inventing them would put numbers into a Meta that
// nothing measured.
func metaFrom(r facts.Receipt) facts.SnapshotMeta {
	return facts.SnapshotMeta{
		RepoPath:       r.RepoPath,
		GeneratedAt:    r.GeneratedAt,
		Duration:       r.Duration,
		Extractors:     r.Extractors,
		Explainers:     r.Explainers,
		Renderers:      r.Renderers,
		FactCount:      r.FactCount,
		InsightCount:   r.InsightCount,
		EnolaVersion:   r.EnolaVersion,
		SnapshotID:     r.SnapshotID,
		Git:            r.Git,
		ConfigHash:     r.ConfigHash,
		IgnoreGlobHash: r.IgnoreGlobHash,
		OutputHashes:   r.OutputHashes,
	}
}

// Segments lists the segment numbers present under a history root, ascending.
func Segments(root string) ([]int, error) {
	entries, err := os.ReadDir(filepath.Join(root, SegDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing segments: %w", err)
	}
	var out []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(e.Name(), "%d", &n); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out, nil
}
