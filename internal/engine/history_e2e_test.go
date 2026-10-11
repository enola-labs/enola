package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/explainers/cycles"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/history"
)

// historyRepo builds a small Go repository plus an engine whose history is recorded into
// a directory this test owns.
//
// HOME is redirected as well as history.dir being set: the default root is under
// ~/.enola, and a test that wrote there would pollute the developer's real history and
// pick up revisions from other tests.
func historyRepo(t *testing.T) (repo string, hist string, eng *engine.Engine) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	repo = t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module testmod\n\ngo 1.21\n")
	writeFile(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\nfunc A() {}\n")

	hist = filepath.Join(t.TempDir(), "history")
	on := true
	cfg := config.Default()
	cfg.History.Enabled = &on
	cfg.History.Dir = hist

	var err error
	eng, err = engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The engine records nothing by itself; the composition root wires the history.
	bootstrap.RecordHistory(eng, cfg)
	eng.RegisterExtractor(goextractor.New())
	eng.RegisterExplainer(cycles.New())
	return repo, hist, eng
}

func snapshot(t *testing.T, eng *engine.Engine, repo string) {
	t.Helper()
	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatalf("write artifacts: %v", err)
	}
}

// The loop end to end: two snapshots with a real edit between them produce two revisions,
// the first marked as the initial state and the second carrying the delta.
func TestHistory_RecordsARevisionPerSnapshot(t *testing.T) {
	repo, hist, eng := historyRepo(t)

	snapshot(t, eng, repo)
	writeFile(t, filepath.Join(repo, "pkg", "b", "b.go"), "package b\n\nimport \"testmod/pkg/a\"\n\nfunc B() { a.A() }\n")
	snapshot(t, eng, repo)

	entries, err := history.Read(hist)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 revisions, got %d", len(entries))
	}

	// The first snapshot of a repository did not ADD its whole graph — it found it.
	if !entries[0].Summary.Initial {
		t.Error("the first revision must be marked initial")
	}
	if entries[0].Summary.FactCount == 0 {
		t.Error("the initial revision must carry the absolute fact count")
	}

	second := entries[1]
	if second.Summary.Initial {
		t.Error("the second revision must be a delta, not another initial state")
	}
	if second.Summary.FactsAdded == 0 {
		t.Errorf("adding a package that imports another must add facts: %+v", second.Summary)
	}
	if second.Summary.EdgesAdded == 0 {
		t.Errorf("the new import must show as an added edge: %+v", second.Summary)
	}
	if second.ID == entries[0].ID {
		t.Error("two different graphs must not share a revision ID")
	}
	if second.Epoch != entries[0].Epoch {
		t.Error("nothing about enola changed between these snapshots, so the epoch must not have")
	}
}

// defaultConfigRepo builds a repo and an engine on the DEFAULT config, with HOME
// redirected so the default history root lands somewhere the test owns.
func defaultConfigRepo(t *testing.T, cfg *config.Config) (repo, home string, eng *engine.Engine) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	repo = t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module testmod\n\ngo 1.21\n")
	writeFile(t, filepath.Join(repo, "a.go"), "package main\n\nfunc main() {}\n")

	var err error
	eng, err = engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The engine records nothing by itself; the composition root wires the history.
	bootstrap.RecordHistory(eng, cfg)
	eng.RegisterExtractor(goextractor.New())
	return repo, home, eng
}

// The strongest assertion available for the storage layer: what the history reconstructs
// must be byte-identical to what the snapshot actually wrote to facts.jsonl.
//
// Everything else in P1 is machinery in service of this. It compares against the receipt's
// own recorded output hash for facts.jsonl — a number computed by WriteArtifacts from the
// bytes it wrote, with no knowledge that a history exists — so the two sides cannot agree
// by sharing a bug.
func TestHistory_ReconstructsTheExactBytesTheSnapshotWrote(t *testing.T) {
	repo, hist, eng := historyRepo(t)

	snapshot(t, eng, repo)
	writeFile(t, filepath.Join(repo, "pkg", "b", "b.go"), "package b\n\nimport \"testmod/pkg/a\"\n\nfunc B() { a.A() }\n")
	snapshot(t, eng, repo)
	// A third, with a line-only move: an edit above a symbol shifts every symbol below it,
	// which is the commonest change of all and the one a semantic delta would discard.
	writeFile(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\n// a comment that pushes A down a line\nfunc A() {}\n")
	snapshot(t, eng, repo)

	entries, err := history.Read(hist)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 revisions, got %d", len(entries))
	}

	for i, e := range entries {
		if e.Blob == nil {
			t.Fatalf("revision %d stored no contents", i)
		}
		factLines, _, rev, err := history.LoadLines(hist, e.Blob.Segment, e.Blob.Member)
		if err != nil {
			t.Fatalf("revision %d: %v", i, err)
		}
		want := rev.Receipt.OutputHashes["facts.jsonl"]
		if want == "" {
			t.Fatalf("revision %d recorded no facts.jsonl hash to check against", i)
		}
		if got := history.HashLines(factLines); got != want {
			t.Errorf("revision %d does not reproduce the bytes the snapshot wrote:\n got %s\nwant %s", i, got, want)
		}
	}

	// And the last one must chain rather than stand alone, or none of the above exercised
	// the reconstruction path.
	if last := entries[len(entries)-1]; last.Blob.Member == 1 {
		t.Error("the final revision is its own base — the chain was never exercised")
	}
}

// Recording is ON by default, and that is the whole point rather than an oversight: a
// history answers questions about the PAST, so a version of this that has to be switched
// on first guarantees there is nothing to read the first time anybody wants it — and the
// only way back is re-snapshotting the repository commit by commit.
func TestHistory_RecordsByDefault(t *testing.T) {
	repo, home, eng := defaultConfigRepo(t, config.Default())
	snapshot(t, eng, repo)

	root, err := history.Root(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := history.Read(root)
	if err != nil {
		t.Fatalf("nothing was recorded under the default config: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 revision, got %d", len(entries))
	}
	// Outside the repository: that is what makes the default safe — no dirty git status,
	// no file appearing in a checkout the user may not even own.
	if !strings.HasPrefix(root, home) {
		t.Errorf("the default history must live under the home dir, got %s", root)
	}
	if entries := dirEntriesUnder(t, repo); containsHistory(entries) {
		t.Errorf("the default history must not be written inside the repository: %v", entries)
	}
}

// The off switch has to actually stop it — including leaving no directory behind, since a
// user who turned it off should not find enola still creating files for it.
func TestHistory_CanBeTurnedOff(t *testing.T) {
	cfg := config.Default()
	off := false
	cfg.History.Enabled = &off

	repo, home, eng := defaultConfigRepo(t, cfg)
	snapshot(t, eng, repo)

	if _, err := os.Stat(filepath.Join(home, ".enola", "graphs")); !os.IsNotExist(err) {
		t.Errorf("history was written despite being disabled (stat err = %v)", err)
	}
	_ = repo
}

func dirEntriesUnder(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func containsHistory(names []string) bool {
	for _, n := range names {
		if n == "history" || n == "log.jsonl" {
			return true
		}
	}
	return false
}

// T-AUTH-2, the invariant the whole feature is bounded by (see _BUILDING_HISTORY.md §2
// and the pkg/history package doc): the history is DERIVED. Deleting it must change
// nothing about what enola says about the tree as it is now.
//
// The snapshot ID is the sharp end of that — it is the content fingerprint every
// comparison and every dedup keys on. If a snapshot could ever come out differently
// because a log of past snapshots existed, the graph would have stopped being a function
// of the source, which is the one claim enola cannot afford to lose.
func TestHistory_DeletingItChangesNothingAboutThePresent(t *testing.T) {
	repo, hist, eng := historyRepo(t)

	snapshot(t, eng, repo)
	withHistory, err := eng.GenerateSnapshot(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(hist); err != nil {
		t.Fatal(err)
	}
	withoutHistory, err := eng.GenerateSnapshot(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}

	if withHistory.Meta.SnapshotID != withoutHistory.Meta.SnapshotID {
		t.Fatalf("the snapshot ID depends on the history:\n  with:    %s\n  without: %s",
			withHistory.Meta.SnapshotID, withoutHistory.Meta.SnapshotID)
	}
	if withHistory.Meta.FactCount != withoutHistory.Meta.FactCount {
		t.Errorf("fact count moved when the history was deleted: %d → %d",
			withHistory.Meta.FactCount, withoutHistory.Meta.FactCount)
	}
}

// A snapshot must never fail because its history could not be written. Here the history
// root is made unwritable, which is the everyday version of this: a read-only home
// directory, a full disk, a permission change.
func TestHistory_AFailureToRecordDoesNotFailTheSnapshot(t *testing.T) {
	repo, hist, eng := historyRepo(t)

	// A regular FILE where the history directory should be: MkdirAll fails, and so does
	// everything after it.
	if err := os.MkdirAll(filepath.Dir(hist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hist, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("snapshot generation: %v", err)
	}
	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatalf("WriteArtifacts must not fail because the history could not be written: %v", err)
	}
}

// Re-running a snapshot on an untouched tree is what an agent loop does constantly. It
// produces the same graph at the same commit, and recording it would make the log a
// record of how often the button was pressed.
func TestHistory_ARerunOnAnUntouchedTreeAddsNothing(t *testing.T) {
	repo, hist, eng := historyRepo(t)

	snapshot(t, eng, repo)
	snapshot(t, eng, repo)
	snapshot(t, eng, repo)

	entries, err := history.Read(hist)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 revision for 3 identical snapshots, got %d", len(entries))
	}
}

// The provenance a local build cannot get from its version. Asserted end to end because the
// field is only useful if the ENGINE sets it — a receipt shape that nothing populates would
// pass every unit test in facts and still leave the epoch blind.
func TestHistory_RecordsWhatTheBuildExtractsLike(t *testing.T) {
	repo, hist, eng := historyRepo(t)
	snapshot(t, eng, repo)

	entries, err := history.Read(hist)
	if err != nil {
		t.Fatal(err)
	}
	_, _, rev, err := history.LoadLines(hist, entries[0].Blob.Segment, entries[0].Blob.Member)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Receipt.ExtractorVersion == "" {
		t.Error("the receipt records no extractor version, so the epoch cannot tell two builds apart")
	}
	if rev.Receipt.ExtractorVersion == rev.Receipt.EnolaVersion {
		t.Errorf("the extractor version is just the build version (%q) — it must track extraction behaviour",
			rev.Receipt.ExtractorVersion)
	}
}
