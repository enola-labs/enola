package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

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
	// Reason says why drift is Unknown, when it is for a reason other than the
	// snapshot recording no hashes.
	Reason string
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
		if d.Reason != "" {
			return d.Reason
		}
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
// addDriftWarning in internal/server for the caller.
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
	// The walk below applies THIS engine's ignore globs. A snapshot taken under
	// another set recorded files this walk skips, or skipped files it walks, and every
	// one of them would read as removed or added: a server whose config ignores *.md
	// reported 139 files of a repository indexed under another config as removed, all
	// of them on disk. Under a different ignore set the comparison measures the config,
	// not the tree.
	if meta.IgnoreGlobHash != "" && meta.IgnoreGlobHash != computeIgnoreGlobHash(e.cfg) {
		return Drift{Unknown: true, Reason: "the snapshot was taken under different ignore globs than this server's config, " +
			"so whether it still matches the working tree cannot be verified here"}, nil
	}

	files, _, _, _, err := e.walkRepo(context.Background(), repoPath)
	if err != nil {
		return Drift{}, fmt.Errorf("walking %s: %w", repoPath, err)
	}
	current := e.driftHashes(context.Background(), repoPath, files)

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

// driftRacyWindow is how recent a file's modification or change time may be before
// its hash is not trusted to stand for it: a coarse filesystem clock can give an
// edit made after a read the same time as the read.
const driftRacyWindow = 2 * time.Second

type hashedFile struct {
	size  int64
	modNs int64
	stamp stamp
	hash  string
}

// driftMemo remembers what the drift walk last hashed, per file, so an engine that
// keeps running and answers many calls reads a file again only after it changed.
// Only the drift walk uses it: a snapshot's own hashes are always read from the bytes.
//
// A file counts as unchanged only when its size, modification time, change time,
// inode and device all match. The modification time and size can be set back by a
// tool (Chtimes, touch -r, a restore); the change time cannot, because the kernel
// sets it on any content or metadata change. Where the platform gives no change
// time (see fileStamp) nothing is remembered and every walk hashes, so no platform
// can answer unchanged for a file it did not check.
type driftMemo struct {
	mu    sync.Mutex
	clock func() time.Time
	files map[string]hashedFile
	reads atomic.Int64
}

func (m *driftMemo) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

func (m *driftMemo) known(path string, info os.FileInfo) (string, bool) {
	st, ok := fileStamp(info)
	if !ok {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.files[path]
	if !ok || h.size != info.Size() || h.modNs != info.ModTime().UnixNano() || h.stamp != st {
		return "", false
	}
	return h.hash, true
}

func (m *driftMemo) remember(path string, info os.FileInfo, hash string) {
	st, ok := fileStamp(info)
	if !ok {
		return
	}
	now := m.now()
	if now.Sub(info.ModTime()) < driftRacyWindow || now.Sub(time.Unix(0, st.ctimeNs)) < driftRacyWindow {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.files == nil {
		m.files = make(map[string]hashedFile)
	}
	m.files[path] = hashedFile{size: info.Size(), modNs: info.ModTime().UnixNano(), stamp: st, hash: hash}
}

// driftHashes is the current content hash of each file named, as computeFileHashes
// gives it, reading only the files that are not the file last hashed here: a file
// with the size, modification time, change time, inode and device it had when it was
// last read keeps its hash. A file modified or changed within driftRacyWindow of being
// read is read again next time.
func (e *Engine) driftHashes(ctx context.Context, repoPath string, files []string) map[string]string {
	hashes := make(map[string]string, len(files))
	for _, rel := range files {
		if ctx.Err() != nil {
			break
		}
		abs := filepath.Join(repoPath, rel)
		info, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if hash, ok := e.drift.known(abs, info); ok {
			hashes[rel] = hash
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		e.drift.reads.Add(1)
		e.drift.remember(abs, info, hash)
		hashes[rel] = hash
	}
	return hashes
}
