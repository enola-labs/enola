// Package history is the read side of enola's architecture history: the sequence of
// snapshots a repository has passed through, and what changed between them.
//
// enola has always held three snapshots and no timeline — the current one, the rotated
// `previous/`, and one pinned `baseline/`. That answers "what did my change just do?"
// and cannot answer "when did this coupling appear?". The history is an append-only log
// of revisions that makes the second question askable.
//
// # It is derived, never authoritative
//
// docs/SNAPSHOTS.md rests on a rule this package must not break: every file enola writes
// is derivable from the tree, and none of them accumulates a history the source has
// forgotten. A history is the first thing enola keeps that the working tree cannot
// reproduce on its own — so it is bounded by three properties:
//
//   - Every entry is REPLAYABLE. It records what a snapshot at that commit contained,
//     and re-running enola at that commit reproduces it. Nothing here is a measurement
//     that could not be taken again.
//   - Nothing that judges the PRESENT reads it. `check`, `diff_snapshot`, freshness and
//     drift keep reading only the current snapshot and the baseline. A verdict about the
//     tree as it is now must not depend on an accumulated file.
//   - Deleting it loses convenience, never truth.
//
// # It is local, and shaped so it need not stay local
//
// Nothing here talks to a network and nothing here requires an account. If a hosted
// history is ever offered it must be additive, which constrains the format now:
//
//   - Entries are keyed by repository IDENTITY (facts.RepoIdentity — the normalized git
//     remote), never by an absolute path, so two machines' histories of one repository
//     describe the same subject.
//   - Seq is machine-local bookkeeping. It is neither identity nor sort order: two
//     machines assign the same Seq to different revisions. Order by At plus git topology.
//   - The exchangeable unit is the entry plus its blob, addressed by ID. How a machine
//     PACKS blobs on disk is a local storage decision that never enters the format.
//
// Together those make two histories of one repository mergeable by union-and-dedup,
// which is the whole of the merge algorithm.
//
// # Boundary
//
// This package is READ-ONLY and imports only internal/facts and the standard library. It must
// stay buildable with CGO_ENABLED=0: a viewer that reads a history has no business
// linking ten tree-sitter grammars, and importing pkg/bootstrap or pkg/dashboard (which
// do) turns a ~4 MB static binary into a ~39 MB cgo one. Writing lives in
// internal/history.
package history

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// LogFileName is the append-only entry log inside a history root.
const LogFileName = "log.jsonl"

// Entry is one revision: a snapshot that was taken, what it was taken over, and how it
// differed from the one before it. One line of log.jsonl, ~350 bytes.
//
// The counts in Summary are all it stores about the delta. Everything else — which facts,
// which edges, which findings — is recomputed by diff.Compute from the reconstructed
// snapshots, so the log can never disagree with diff_snapshot. Two renderings of the same
// change that drift apart is a worse failure than a slower one.
type Entry struct {
	// ID is the snapshot's content fingerprint (facts.SnapshotMeta.SnapshotID,
	// "sha256:"-prefixed). Revision identity, and the dedup key across machines.
	ID string `json:"id"`

	// Repo is the portable repository identity (facts.RepoIdentity): the normalized git
	// remote, or the checkout directory name when there is no remote. NEVER a path.
	Repo string `json:"repo"`

	// Seq is a machine-local ordinal, for display and for naming a revision on the
	// command line. Not identity, not sort order — see the package doc.
	Seq int `json:"seq"`

	// At is when the snapshot was generated (RFC3339 UTC).
	At string `json:"at"`

	// Epoch fingerprints the inputs that decide what a graph CONTAINS, as opposed to
	// what the code says: enola's version, the effective config, the ignore globs and
	// the extractor/explainer sets. A delta across an epoch boundary is rebuild noise,
	// not architectural change. See Epoch.
	Epoch string `json:"epoch"`

	// Git is the VCS state at snapshot time: ref, commit, dirty, remote. Nil for a
	// non-git tree, which is a supported case and not an error.
	Git *facts.GitInfo `json:"git,omitempty"`

	// Parents are the git parent commits of Git.Commit, recorded so a timeline can still
	// be drawn when the repository is unavailable, shallow, or gone. It is a FALLBACK:
	// the real topology is rebuilt from git at render time, because enola only ever
	// observes a sparse subset of the commit graph and the edges between observed
	// revisions have to be derived by ancestry, not remembered.
	Parents []string `json:"parents,omitempty"`

	// Repos records the per-repository commits of a multi-repo graph. A multi-repo
	// snapshot has no single commit — it has a vector of them — so it gets a timeline
	// with a column per repository rather than a fabricated DAG. Empty in single-repo mode.
	Repos []RepoRef `json:"repos,omitempty"`

	// Summary is the delta from the preceding revision, in counts only.
	Summary Summary `json:"summary"`

	// Refs are names pointing at this revision ("baseline", a tag, a user mark).
	Refs []string `json:"refs,omitempty"`

	Origin string `json:"origin,omitempty"`

	// Blob locates the stored facts/insights for this revision. Nil means the revision
	// is header-only — either a P0 history (no blobs written yet) or one thinned by gc,
	// in which case the contents are recoverable with `enola log --backfill`.
	Blob *BlobRef `json:"blob,omitempty"`
}

// RepoRef is one repository's position in a multi-repo graph at snapshot time.
type RepoRef struct {
	Label  string `json:"label"`
	Commit string `json:"commit,omitempty"`
	Dirty  bool   `json:"dirty,omitempty"`
}

// BlobRef locates a revision's stored snapshot within the local pack layout. Written in
// P1; carried here so a P0 reader parses a P1 log without changes.
type BlobRef struct {
	Segment int `json:"segment"`
	Member  int `json:"member"`
}

// Summary is the delta from the preceding revision, as counts.
//
// Absolute FactCount/InsightCount ride along because they are what a log line shows for
// the FIRST revision, where there is no delta to report, and what `--stat` needs without
// touching a blob.
type Summary struct {
	FactsAdded   int `json:"facts_added,omitempty"`
	FactsRemoved int `json:"facts_removed,omitempty"`
	FactsChanged int `json:"facts_changed,omitempty"`
	EdgesAdded   int `json:"edges_added,omitempty"`
	EdgesRemoved int `json:"edges_removed,omitempty"`

	// Findings counted here are the STRUCTURAL-CAUSE buckets only (diff.FindingsNew /
	// FindingsResolved). The incidental buckets — findings that moved because a
	// statistical threshold drifted or a top-N list re-ranked — are deliberately left
	// out: a log line that reports them reads as "this change caused a regression" for a
	// change that caused nothing.
	FindingsNew      int `json:"findings_new,omitempty"`
	FindingsResolved int `json:"findings_resolved,omitempty"`

	// ByKind is the NET fact delta per kind (added minus removed), for `--stat`.
	ByKind map[string]int `json:"by_kind,omitempty"`

	FactCount    int `json:"fact_count"`
	InsightCount int `json:"insight_count"`

	// Initial marks a revision with no predecessor to compare against — the first
	// snapshot of a repository. Its counts describe the whole graph, not a change, and a
	// renderer that shows them as a delta reports the entire codebase as newly added.
	Initial bool `json:"initial,omitempty"`

	// Incomparable marks a delta computed across snapshots that diff.compareMeta judged
	// not comparable — almost always an epoch change (a new enola version, a different
	// extractor set, edited ignore globs). The counts are real arithmetic over two fact
	// sets and a fiction as a description of anyone's change.
	Incomparable bool `json:"incomparable,omitempty"`
}

// Empty reports whether a revision changed nothing structural.
func (s Summary) Empty() bool {
	return s.FactsAdded == 0 && s.FactsRemoved == 0 && s.FactsChanged == 0 &&
		s.EdgesAdded == 0 && s.EdgesRemoved == 0 &&
		s.FindingsNew == 0 && s.FindingsResolved == 0
}

// Headline is the one-line description of a revision, for `log --oneline`.
//
// Derived rather than stored: a persisted string is one more thing that can disagree with
// the counts it was derived from, and it would have to be re-derived anyway the first
// time the phrasing changes. Findings lead when present — a new cycle is the thing worth
// noticing in a line that also says "+12 facts".
func (s Summary) Headline() string {
	if s.Initial {
		return fmt.Sprintf("initial: %d facts, %d findings", s.FactCount, s.InsightCount)
	}
	var parts []string
	if s.FindingsNew > 0 {
		parts = append(parts, fmt.Sprintf("%d new finding%s", s.FindingsNew, plural(s.FindingsNew)))
	}
	if s.FindingsResolved > 0 {
		parts = append(parts, fmt.Sprintf("%d resolved", s.FindingsResolved))
	}
	if f := signed(s.FactsAdded, s.FactsRemoved); f != "" {
		parts = append(parts, f+" facts")
	}
	if s.FactsChanged > 0 {
		parts = append(parts, fmt.Sprintf("~%d changed", s.FactsChanged))
	}
	if e := signed(s.EdgesAdded, s.EdgesRemoved); e != "" {
		parts = append(parts, e+" edges")
	}
	if len(parts) == 0 {
		return "no architectural change"
	}
	return strings.Join(parts, " · ")
}

// signed renders an add/remove pair as "+3", "-2" or "+3/-2"; "" when both are zero.
func signed(added, removed int) string {
	switch {
	case added > 0 && removed > 0:
		return fmt.Sprintf("+%d/-%d", added, removed)
	case added > 0:
		return fmt.Sprintf("+%d", added)
	case removed > 0:
		return fmt.Sprintf("-%d", removed)
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Working reports whether this revision is unanchored — taken over an uncommitted tree,
// or over a directory that is not a git repository at all. Both are the same thing for
// retention: there is no commit that will still mean something tomorrow, and an agent
// loop produces them by the hundred. Committed revisions are permanent; working ones are
// kept as a bounded ring per base commit (see internal/history).
func (e Entry) Working() bool { return e.Git == nil || e.Git.Dirty }

// Commit is the revision's git commit, or "" when it was taken outside a git tree.
func (e Entry) Commit() string {
	if e.Git == nil {
		return ""
	}
	return e.Git.Commit
}

// Ref is the branch or symbolic ref the revision was taken on, or "".
func (e Entry) Ref() string {
	if e.Git == nil {
		return ""
	}
	return e.Git.Ref
}

// Short is the abbreviated revision ID for display: the first 7 hex digits, with the
// "sha256:" prefix removed. Matches how git abbreviates, and how enola abbreviates a
// snapshot ID everywhere else.
func (e Entry) Short() string { return ShortID(e.ID) }

// ShortID abbreviates a "sha256:"-prefixed hash to its first 7 hex digits.
func ShortID(id string) string {
	h := strings.TrimPrefix(id, "sha256:")
	if len(h) > 7 {
		return h[:7]
	}
	return h
}

// Epoch fingerprints everything OTHER than the source that decides what a graph
// contains: which enola built it, the effective config, the ignore globs, and the
// extractor and explainer sets that ran.
//
// It exists because a delta is only a description of somebody's change when both sides
// were produced the same way. Bump a cacheVersion, improve an extractor, enable a
// language, edit an ignore glob — and hundreds of facts appear to change because the
// PARSER changed. diff.compareMeta already detects each of those and says so
// (WarnVersionMismatch, WarnExtractorSet, WarnExplainerSet, WarnIgnoreGlobs); this
// reduces the same inputs to one comparable token, so a log can group revisions into
// stretches that mean something relative to each other and mark the seams between them.
//
// ExtractorVersion is the load-bearing one for anybody running a build they made
// themselves, and it was missing from the first version of this function — a gap that
// showed up the first time it mattered. EnolaVersion is the constant "dev" in every local
// build, the config had not changed, and the extractor set had not changed, so a fix that
// removed 21 fabricated facts was recorded in this repository's own history as somebody
// deleting 21 things from the codebase: the exact misreading the epoch exists to prevent.
// A released binary is covered by EnolaVersion alone; a development one is covered by
// nothing else.
//
// Explainers are included even though they do not change a single fact: findings are
// keyed by their source, so an explainer present on one side only contributes its ENTIRE
// output as a delta, and Summary counts findings.
//
// IgnoreGlobHash is included even though ConfigHash is documented as a superset of it,
// because a snapshot written before receipts carries no ConfigHash at all and the glob
// hash is then the only discriminator left.
func Epoch(m facts.SnapshotMeta) string {
	h := sha256.New()
	write := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	write(m.EnolaVersion)
	write(m.ExtractorVersion)
	write(m.ConfigHash)
	write(m.IgnoreGlobHash)
	writeSet := func(vals []string) {
		cp := append([]string(nil), vals...)
		sort.Strings(cp)
		for _, v := range cp {
			write(v)
		}
		write("|")
	}
	writeSet(m.Extractors)
	writeSet(m.Explainers)
	return hex.EncodeToString(h.Sum(nil))[:12]
}
