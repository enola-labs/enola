package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/version"
)

// Baseline subdirectories under the .enola output dir. `previous` is rotated
// automatically on every snapshot write (one step back); `baseline` is pinned
// explicitly via SetBaseline and survives subsequent generate_snapshot runs, so
// an agent can pin once at task start and diff after several rounds of edits.
const (
	PreviousSubdir = "previous"
	BaselineSubdir = "baseline"
)

// snapshotArtifactFiles are the on-disk files that constitute a persisted
// snapshot. receipt.json rides along so a pinned/previous baseline carries its
// provenance + quality receipt, which compare_receipts diffs against the current.
var snapshotArtifactFiles = []string{"facts.jsonl", "insights.json", "snapshot.meta.json", "receipt.json"}

// OutputDir returns the absolute .enola output directory for repoPath.
func (e *Engine) OutputDir(repoPath string) string {
	return filepath.Join(repoPath, e.cfg.Output.Dir)
}

// ResolveBaselineDir maps a baseline selector to the directory holding that
// snapshot's artifacts: "" / "pinned" → the explicit SetBaseline pin, "previous" → the
// automatically-rotated preceding run, anything else → an explicit path.
//
// Shared so every caller resolves a selector identically. The MCP tools
// (diff_snapshot, compare_receipts) and the `enola check` CLI gate must agree on what
// `previous` means, or the same word would name different snapshots depending on which
// surface the caller used.
func ResolveBaselineDir(outDir, selector string) string {
	switch strings.ToLower(strings.TrimSpace(selector)) {
	case "", "pinned":
		return filepath.Join(outDir, BaselineSubdir)
	case "previous":
		return filepath.Join(outDir, PreviousSubdir)
	default:
		return selector
	}
}

// SetBaseline pins the current on-disk snapshot as the diff baseline by copying
// the snapshot artifacts from the output dir into <output>/baseline/. Returns an
// error if no snapshot has been written yet.
func (e *Engine) SetBaseline(repoPath string) error {
	outDir := e.OutputDir(repoPath)
	if _, err := os.Stat(filepath.Join(outDir, "facts.jsonl")); err != nil {
		return fmt.Errorf("no snapshot to pin as baseline (run generate_snapshot first): %w", err)
	}
	return copyArtifacts(outDir, filepath.Join(outDir, BaselineSubdir))
}

// rotatePrevious copies the existing snapshot artifacts (if any) from outDir into
// outDir/previous/ before they are overwritten, giving a one-step-back baseline
// for diffing without any explicit pin. It is a no-op when no prior snapshot
// exists. Only the root artifact files are copied, so the previous/ and
// baseline/ subdirs are never nested into themselves.
func rotatePrevious(outDir string) error {
	if _, err := os.Stat(filepath.Join(outDir, "facts.jsonl")); err != nil {
		return nil // nothing to rotate
	}
	return copyArtifacts(outDir, filepath.Join(outDir, PreviousSubdir))
}

// copyArtifacts publishes the snapshot artifact files from srcDir to dstDir as a
// single atomic step: everything is copied into a sibling temp directory first, and
// only a complete set is renamed into place. Individually-missing sources are
// tolerated (older snapshots may lack insights/meta).
//
// It copied file-by-file directly into dstDir until a reader could observe the
// halfway state. Two things made that matter:
//
//   - A partially-copied baseline reads as a real one. LoadSnapshotDir only requires
//     facts.jsonl to exist, so a truncated copy is diffed against rather than
//     rejected, and the delta is silently wrong. Once the baseline is pinned in the
//     background at session start, a short session can reach exactly that window.
//   - A failed copy destroyed the previous baseline. Writing in place means the old
//     artifacts are already overwritten when the error happens; staging means a
//     failure leaves the existing baseline untouched.
//
// The swap is remove-then-rename, so there is a brief moment where dstDir does not
// exist. That is deliberate and safe: an ABSENT baseline is handled everywhere (it
// reads as "no baseline pinned"), whereas a PARTIAL one is the failure this exists to
// prevent. Renaming a directory onto a non-empty one is not portable, so a swap
// without any window is not available here.
//
// Replacing rather than overlaying also means dstDir ends up holding exactly the
// current artifact set: a stray file written by an older enola no longer survives
// indefinitely because nothing overwrites it.
func copyArtifacts(srcDir, dstDir string) error {
	parent := filepath.Dir(dstDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", parent, err)
	}
	// Staged inside the output dir so the rename stays on one filesystem — across a
	// mount boundary os.Rename fails with EXDEV and the atomicity is lost.
	tmpDir, err := os.MkdirTemp(parent, ".tmp-"+filepath.Base(dstDir)+"-")
	if err != nil {
		return fmt.Errorf("creating staging dir in %s: %w", parent, err)
	}
	// Cleans up every failure path, and is a no-op once the rename has moved it.
	defer func() { _ = os.RemoveAll(tmpDir) }()

	for _, name := range snapshotArtifactFiles {
		if err := copyFile(filepath.Join(srcDir, name), filepath.Join(tmpDir, name)); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
	}

	if err := os.RemoveAll(dstDir); err != nil {
		return fmt.Errorf("clearing %s: %w", dstDir, err)
	}
	if err := os.Rename(tmpDir, dstDir); err != nil {
		return fmt.Errorf("publishing %s: %w", dstDir, err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// LoadSnapshotDir reads a persisted snapshot (facts.jsonl plus, when present,
// insights.json and snapshot.meta.json) from dir into an in-memory Snapshot for
// diffing. Missing insights/meta are tolerated; a missing facts.jsonl is an error.
func LoadSnapshotDir(dir string) (*facts.Snapshot, error) {
	factsPath := filepath.Join(dir, "facts.jsonl")
	if _, err := os.Stat(factsPath); err != nil {
		return nil, fmt.Errorf("no snapshot at %s: %w", dir, err)
	}
	store := facts.NewStore()
	if err := store.ReadJSONLFile(factsPath); err != nil {
		return nil, fmt.Errorf("reading facts from %s: %w", factsPath, err)
	}
	// FactsRef, not All. The store was created here, is never mutated, and goes out of
	// scope at the return — the returned slice is the only thing that keeps it alive —
	// so there is nothing for a copy to protect against.
	//
	// It mattered because this runs at the END of every snapshot: recordHistory loads
	// the previous snapshot to summarize the delta, and on a 1.9M-fact graph All's deep
	// copy of every fact, Props map and Relations slice cost as much again as the load
	// itself. Measured on the kernel, the history step took footprint from 8.5 GB to
	// 12.1 GB — after the snapshot was finished and FreeOSMemory had already run.
	snap := &facts.Snapshot{Facts: store.FactsRef()}

	if data, err := os.ReadFile(filepath.Join(dir, "insights.json")); err == nil {
		var ins []facts.Insight
		if err := json.Unmarshal(data, &ins); err == nil {
			snap.Insights = ins
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "snapshot.meta.json")); err == nil {
		var meta facts.SnapshotMeta
		if err := json.Unmarshal(data, &meta); err == nil {
			snap.Meta = meta
		}
	}
	return snap, nil
}

// CurrentMeta returns the IDENTITY half of the SnapshotMeta this engine would write
// for repoPath — enola version, config and ignore-glob hashes, the detected extractor
// set, the repo path and its git state — without parsing a single file.
//
// It exists so "could a snapshot taken now be compared against this baseline?" can be
// answered before deciding to take one. The session-start hook asks it to find out
// whether the baseline it is about to grade against is still usable, and `enola
// doctor` asks it to tell a human the same thing before a session ends rather than
// after. Both would otherwise have to build a full snapshot to learn that the
// comparison was never going to work.
//
// Extractor detection is replicated from runExtractors deliberately rather than
// approximated: the recorded set is "enabled AND detected". It walks the repository
// to do it, which is why the walk is here rather than a bounded probe per extractor:
// detection now answers from the walked names (plugin.FileListDetector), and this
// list is compared against the recorded snapshot's by diff.CompareMeta. Answering
// the question a second way would make the two disagree on exactly the repositories
// the file-list detectors were added for — and WarnExtractorSet is fatal to
// check.BlockingKinds, so the disagreement would report "not comparable" on every
// run, freeze no baseline, and never resolve. One walk is also cheaper than the
// per-extractor bounded walks it replaces: 1.44s of them on dotnet/runtime.
//
// A walk error is not fatal here. This function answers "is a comparison possible",
// and the honest answer when the tree cannot be read is the pre-walk one — every
// extractor falls back to its own Detect, exactly as before.
//
// Counting fields (facts, files, timings) are left zero — this is not a snapshot and
// must not be mistaken for one; only the fields diff.CompareMeta reads are populated.
func (e *Engine) CurrentMeta(repoPath string) *facts.SnapshotMeta {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return nil
	}
	_, _, allNames, _, walkErr := e.walkRepo(context.Background(), absRepo)
	if walkErr != nil {
		allNames = nil
	}
	var used []string
	for _, ext := range e.extractors.All() {
		if !e.cfg.IsExtractorEnabled(ext.Name()) {
			continue
		}
		if detected, err := e.detect(ext, absRepo, allNames); err == nil && detected {
			used = append(used, ext.Name())
		}
	}
	git := gitInfo(absRepo, e.cfg.Output.Dir)
	var remote string
	if git != nil {
		remote = git.Remote
	}
	return &facts.SnapshotMeta{
		RepoPath:         absRepo,
		RepoLabel:        repoLabelFor(absRepo, remote),
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Extractors:       used,
		EnolaVersion:     version.Version,
		ExtractorVersion: cacheVersion,
		Git:              git,
		ConfigHash:       computeConfigHash(e.cfg),
		IgnoreGlobHash:   computeIgnoreGlobHash(e.cfg),
	}
}
