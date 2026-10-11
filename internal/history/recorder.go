package history

// Recording a snapshot as a revision: the step that turns "a snapshot was written" into
// "the architecture history has one more entry". The engine does not know this package.
// It hands whoever is wired to it the facts of what it just wrote, and the composition
// root wires a Recorder.

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
	pkghistory "github.com/enola-labs/enola/pkg/history"
)

// RecorderOptions is the history configuration a Recorder runs under.
type RecorderOptions struct {
	// Dir overrides where the history is kept; see pkghistory.Root.
	Dir string
	// Blobs stores each revision's contents, not only its summary line.
	Blobs bool
	// WorkingKeep, RevisionKeep and BlobKeep are the retention settings Append applies.
	WorkingKeep, RevisionKeep, BlobKeep int
}

// Record is one written snapshot, as the recorder needs to see it.
type Record struct {
	RepoPath string
	// Meta is the snapshot's metadata as written, output hashes included.
	Meta facts.SnapshotMeta
	// Snapshot is the published snapshot. Its Meta is replaced by Meta above.
	Snapshot *facts.Snapshot
	// FactsPath is the facts.jsonl just written, read back for the revision's contents.
	FactsPath string
	// PreviousDir holds the rotated preceding snapshot, the parent revision.
	PreviousDir string
	// Parents and Repos are asked for only when a revision is actually appended, since
	// each costs a git invocation and a cluster writes one union many times.
	Parents func() []string
	Repos   func() []pkghistory.RepoRef
}

// Recorder appends revisions and remembers, for one run, what it already did.
type Recorder struct {
	opts RecorderOptions

	mu sync.Mutex
	// recorded remembers which snapshot id this run recorded into which history root,
	// so a cluster that writes one union to many repo dirs sharing a history store
	// appends the revision once.
	recorded map[string]string
	// summaries remembers this run's previous/ deltas; see summarizeOnce.
	summaries map[string]pkghistory.Summary
}

// NewRecorder returns a recorder for one engine's run.
func NewRecorder(opts RecorderOptions) *Recorder {
	return &Recorder{opts: opts}
}

// Record appends one revision to the repository's architecture history.
//
// The engine calls it at the END of WriteArtifacts, after every artifact is on disk, and every
// failure inside it is logged and swallowed. A history is a convenience built on top of
// a snapshot; a snapshot that fails because its history could not be written would have
// inverted that relationship.
//
// The delta is computed against previous/, which WriteArtifacts has just rotated, so it
// holds the immediately preceding run — the parent revision by construction. That is the
// same pair diff_snapshot compares under `--baseline=previous`, computed by the same
// function, so the log's counts and the diff's counts cannot drift apart.
func (r *Recorder) Record(rec Record) {
	if rec.Snapshot == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	root, err := pkghistory.Root(rec.RepoPath, r.opts.Dir)
	if err != nil {
		log.Printf("[history] warning: cannot locate history dir: %v", err)
		return
	}
	if r.recorded[root] == rec.Meta.SnapshotID {
		return
	}

	// Summarize against the finalized meta (the published snapshot's Meta lacks
	// OutputHashes until the republish, and may have moved on).
	current := *rec.Snapshot
	current.Meta = rec.Meta

	entry := pkghistory.Entry{
		ID:      rec.Meta.SnapshotID,
		Repo:    facts.RepoIdentity(rec.Meta),
		At:      rec.Meta.GeneratedAt,
		Epoch:   pkghistory.Epoch(rec.Meta),
		Git:     rec.Meta.Git,
		Summary: r.summarizeOnce(&current, previousSideFor(rec.PreviousDir)),
	}
	if rec.Parents != nil {
		entry.Parents = rec.Parents()
	}
	if rec.Repos != nil {
		entry.Repos = rec.Repos()
	}

	opts := Options{
		WorkingKeep:  r.opts.WorkingKeep,
		RevisionKeep: r.opts.RevisionKeep,
		BlobKeep:     r.opts.BlobKeep,
		Contents:     r.contents(rec.Meta, &current, rec.FactsPath),
	}
	recorded, err := Append(root, entry, opts)
	if err != nil {
		log.Printf("[history] warning: could not record history: %v", err)
		return
	}
	if r.recorded == nil {
		r.recorded = map[string]string{}
	}
	r.recorded[root] = rec.Meta.SnapshotID
	if recorded {
		log.Printf("[history] recorded history revision %s (%s)", entry.Short(), entry.Summary.Headline())
	}
}

// contents assembles the revision's storable payload, or nil when blob storage is
// off — in which case the revision is still recorded as a header, and only `show` and
// `diff` on it are unavailable.
//
// factsPath is the facts.jsonl this snapshot just wrote, read back line by line. Reading
// the artifact is what guarantees the stored lines are the ones the snapshot actually
// produced: any path that re-marshals facts here would be a second serialization that
// could disagree with the first, and the disagreement would be written into the history
// rather than caught. It previously received the in-memory buffer the file had been
// written from, which gave the same guarantee — but only by keeping a whole extra copy
// of the serialization alive, 792 MiB of it on a large graph, for a step that is
// skipped entirely when blobs are off.
func (r *Recorder) contents(meta facts.SnapshotMeta, snap *facts.Snapshot, factsPath string) *Contents {
	if !r.opts.Blobs {
		return nil
	}
	insightLines, err := pkghistory.InsightLines(snap.Insights)
	if err != nil {
		log.Printf("[history] warning: could not serialize insights for the history: %v", err)
		return nil
	}
	factLines, err := readLines(factsPath)
	if err != nil {
		log.Printf("[history] warning: could not read %s back for the history: %v", factsPath, err)
		return nil
	}
	return &Contents{
		FactLines:    factLines,
		InsightLines: insightLines,
		Receipt:      meta.Receipt(),
	}
}

// readLines reads canonical JSONL into its lines, without the trailing empty element the
// final newline would produce. The scanner buffer matches the one Store.ReadJSONL uses,
// so a fact line this engine can write is a fact line it can read back.
func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	var lines []string
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// previousSide describes the rotated preceding snapshot WITHOUT its facts: the small
// parts are read into memory, and the facts stay on disk behind a source that streams
// them on demand.
//
// The facts are the whole reason this type exists. summarize needs counts from them,
// not the facts themselves, and loading a kernel-sized previous snapshot to produce
// seven integers cost 386 MiB — at the end of a run, alongside the snapshot it was
// being compared against. Insights and meta are kept whole because they are small and
// the findings comparison genuinely needs their content.
type previousSide struct {
	facts    diff.FactSource
	insights []facts.Insight
	meta     facts.SnapshotMeta
}

// previousSideFor prepares the rotated preceding snapshot, or nil when there is none
// (the first snapshot of a repository, or a history enabled after the fact).
func previousSideFor(dir string) *previousSide {
	factsPath := filepath.Join(dir, "facts.jsonl")
	if _, err := os.Stat(factsPath); err != nil {
		return nil
	}
	p := &previousSide{
		facts: diff.JSONLSource(func() (io.ReadCloser, error) { return os.Open(factsPath) }),
	}
	if data, err := os.ReadFile(filepath.Join(dir, "insights.json")); err == nil {
		var ins []facts.Insight
		if err := json.Unmarshal(data, &ins); err == nil {
			p.insights = ins
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "snapshot.meta.json")); err == nil {
		var meta facts.SnapshotMeta
		if err := json.Unmarshal(data, &meta); err == nil {
			p.meta = meta
		}
	}
	return p
}

// summarizeOnce is summarize with one run's results remembered. A cluster writes
// one union to many repository dirs whose previous/ sides are, on a rerun, the
// same earlier union; the delta between the same two snapshots is the same
// number wherever it is computed, and computing it streams the previous facts
// file three times (on a 1.5M-fact union, 26 s per dir). The key is what the
// two sides' receipts say the files hold: the previous meta's own output hashes
// and the current meta's, so a previous/ without those hashes, or a differing
// one, is summarized afresh.
func (r *Recorder) summarizeOnce(current *facts.Snapshot, previous *previousSide) pkghistory.Summary {
	key := summaryKey(current.Meta, previous)
	if key == "" {
		return summarize(current, previous)
	}
	if s, ok := r.summaries[key]; ok {
		return s
	}
	s := summarize(current, previous)
	if r.summaries == nil {
		r.summaries = map[string]pkghistory.Summary{}
	}
	r.summaries[key] = s
	return s
}

func summaryKey(current facts.SnapshotMeta, previous *previousSide) string {
	if previous == nil {
		return ""
	}
	prevFacts, prevInsights := previous.meta.OutputHashes["facts.jsonl"], previous.meta.OutputHashes["insights.json"]
	curFacts, curInsights := current.OutputHashes["facts.jsonl"], current.OutputHashes["insights.json"]
	if prevFacts == "" || prevInsights == "" || curFacts == "" || curInsights == "" {
		return ""
	}
	return strings.Join([]string{current.SnapshotID, curFacts, curInsights, prevFacts, prevInsights, previous.meta.SnapshotID}, "|")
}

// summarize reduces a snapshot pair to the counts a log line needs.
//
// With no predecessor the result is marked Initial and carries absolute counts rather
// than a delta: the first snapshot of a repository did not ADD ten thousand facts, it
// found them, and a renderer shown a delta there reports the entire codebase as the work
// of whoever ran enola first.
// It computes the delta by COUNTING rather than by building it. diff.Compute produces
// the same numbers and is still what diff_snapshot uses, but it materialises every
// added, removed and changed fact plus both sides' edge sets to do it — 261 MiB on a
// warm dotnet/runtime run, on top of the 386 MiB of loading the baseline. A summary
// needs none of that; see diff.Counts.
func summarize(current *facts.Snapshot, previous *previousSide) pkghistory.Summary {
	s := pkghistory.Summary{
		FactCount:    len(current.Facts),
		InsightCount: len(current.Insights),
	}
	if previous == nil {
		s.Initial = true
		return s
	}

	counts, err := diff.CountFacts(previous.facts, current.Facts)
	if err != nil {
		// The previous snapshot is unreadable. Record the revision with absolute
		// counts rather than dropping it: a history with a gap is worse than one
		// whose delta is missing, and the cause is logged.
		log.Printf("[history] warning: could not summarize the delta against previous/: %v", err)
		s.Initial = true
		return s
	}
	s.FactsAdded = counts.FactsAdded
	s.FactsRemoved = counts.FactsRemoved
	s.FactsChanged = counts.FactsChanged
	s.EdgesAdded = counts.EdgesAdded
	s.EdgesRemoved = counts.EdgesRemoved
	// The structural-cause buckets only. The incidental ones exist precisely because a
	// finding can appear without this change causing it, and a log line is the last place
	// that distinction should be dropped.
	findings := diff.ClassifyFindings(previous.insights, current.Insights, counts.TouchedNames)
	s.FindingsNew = findings.New
	s.FindingsResolved = findings.Resolved
	// Not simply !Comparable: elapsed time between two revisions is the normal shape of a
	// timeline, and flagging it would mark most of a release-sampled history as suspect.
	s.Incomparable = diff.CompareMeta(previous.meta, current.Meta).InvalidatesDelta()

	byKind := map[string]int{}
	for kind, n := range counts.AddedByKind {
		byKind[kind] += n
	}
	for kind, n := range counts.RemovedByKind {
		byKind[kind] -= n
	}
	for kind, n := range byKind {
		if n == 0 {
			delete(byKind, kind)
		}
	}
	if len(byKind) > 0 {
		s.ByKind = byKind
	}
	return s
}
