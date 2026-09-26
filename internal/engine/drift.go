package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
)

// Drift reports how a repository's files have moved since its snapshot was taken:
// which were added, removed, or edited. It is the answer to "does the graph I am
// about to diff still describe the code on disk?".
//
// It is deliberately a FILESYSTEM comparison, not a VCS one. The snapshot already
// records a content hash per walked file (facts.SnapshotMeta.FileHashes), so the
// question can be answered exactly, from enola's own data, for repos that are not
// git working trees at all. The git-derived signals cannot answer it: a commit that
// has not moved says nothing about the working tree, and Git.Dirty is a single
// boolean, so a tree that was ALREADY modified when the snapshot ran cannot be
// distinguished from the same tree modified further (see internal/engine/freshness.go
// and the freshness dossier's GAP-FR-03).
//
// Unknown means the comparison could not be made — the snapshot recorded no file
// hashes, which is the case for a pre-receipt snapshot or a restore whose
// snapshot.meta.json failed to load. Callers must surface that as "cannot verify"
// and never as a clean tree: silence and confirmation are different answers.
type Drift struct {
	Added    []string // walked now, absent from the snapshot
	Removed  []string // in the snapshot, absent now
	Modified []string // present in both, different content hash
	Unknown  bool     // no recorded hashes to compare against
}

// Any reports whether any file was added, removed, or edited. It is false when
// Unknown — an unanswerable comparison is not evidence of change.
func (d Drift) Any() bool {
	return len(d.Added) > 0 || len(d.Removed) > 0 || len(d.Modified) > 0
}

// Count is the total number of drifted paths.
func (d Drift) Count() int {
	return len(d.Added) + len(d.Removed) + len(d.Modified)
}

// Summary renders a one-line description for a comparability warning, naming counts
// and a bounded sample of paths so the caller can see WHICH files moved without the
// message becoming unbounded on a large refactor.
func (d Drift) Summary(maxPaths int) string {
	if d.Unknown {
		return "the snapshot recorded no file hashes, so whether it still matches the working tree cannot be verified"
	}
	if !d.Any() {
		return ""
	}
	parts := make([]string, 0, 3)
	if n := len(d.Modified); n > 0 {
		parts = append(parts, fmt.Sprintf("%d modified", n))
	}
	if n := len(d.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("%d added", n))
	}
	if n := len(d.Removed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", n))
	}
	sample := make([]string, 0, maxPaths)
	for _, group := range [][]string{d.Modified, d.Added, d.Removed} {
		for _, p := range group {
			if len(sample) >= maxPaths {
				break
			}
			sample = append(sample, p)
		}
	}
	msg := ""
	for i, p := range parts {
		if i > 0 {
			msg += ", "
		}
		msg += p
	}
	if len(sample) > 0 {
		msg += " (e.g. "
		for i, p := range sample {
			if i > 0 {
				msg += ", "
			}
			msg += p
		}
		if d.Count() > len(sample) {
			msg += ", …"
		}
		msg += ")"
	}
	return msg
}

// Drift re-walks repoPath under the current config and compares each file's content
// hash against the hashes the loaded snapshot recorded, returning the exact set of
// differences.
//
// This re-reads and re-hashes every walked file, which costs roughly the walk+hash
// stages of a snapshot. That is why it is an on-demand call for the tools that are
// ABOUT to make a claim about the change — diff_snapshot, and anything else computing
// its own delta — rather than part of the per-tool-call freshness banner: the banner is
// a cheap proactive nudge, this is an exact answer at a decision point. See
// internal/drift for the shared caller.
//
// The snapshot's recorded hashes cover the repo that snapshot indexed. In multi-repo
// (append) mode that is the most recently indexed repo, so a caller asking about a
// different repo in the graph gets Unknown rather than a wrong answer.
func (e *Engine) Drift(repoPath string) (Drift, error) {
	b := e.current.Load()
	if b.snapshot == nil {
		return Drift{Unknown: true}, nil
	}
	return e.DriftFromMeta(repoPath, b.snapshot.Meta)
}

// DriftFromMeta is Drift against a meta read from disk rather than the published
// snapshot: a cluster's pin asks every member's own snapshot.meta.json whether the
// tree it describes is the tree on disk, without loading any member's facts.
func (e *Engine) DriftFromMeta(repoPath string, meta facts.SnapshotMeta) (Drift, error) {
	recorded := make(map[string]string, len(meta.FileHashes))
	for _, fh := range meta.FileHashes {
		recorded[fh.Path] = fh.Hash
	}
	if len(recorded) == 0 {
		return Drift{Unknown: true}, nil
	}

	files, _, _, _, err := e.walkRepo(context.Background(), repoPath)
	if err != nil {
		return Drift{}, fmt.Errorf("walking %s: %w", repoPath, err)
	}
	current := e.computeFileHashes(context.Background(), repoPath, files)

	var d Drift
	for path, curHash := range current {
		prevHash, existed := recorded[path]
		switch {
		case !existed:
			d.Added = append(d.Added, path)
		case prevHash != curHash:
			d.Modified = append(d.Modified, path)
		}
	}
	for path := range recorded {
		if _, stillThere := current[path]; !stillThere {
			d.Removed = append(d.Removed, path)
		}
	}

	// Deterministic order: these paths reach a user-facing warning.
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	sort.Strings(d.Modified)
	return d, nil
}
