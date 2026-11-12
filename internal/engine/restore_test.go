package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/explainers/cycles"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
	"github.com/enola-labs/enola/internal/facts"
)

// TestRestoreFromDir verifies a restart-style restore rebuilds the full snapshot
// from disk WITHOUT re-running extractors: facts, insights, and the generated_at
// timestamp all come back. This is the regression against the old facts-only load
// that left Meta with only RepoPath (no generated_at, no insights).
func TestRestoreFromDir(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module testmod\n\ngo 1.21\n")
	// An import cycle so the cycles explainer emits at least one insight to restore.
	writeFile(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\nimport \"testmod/pkg/b\"\n\nfunc A() { b.B() }\n")
	writeFile(t, filepath.Join(repo, "pkg", "b", "b.go"), "package b\n\nimport \"testmod/pkg/a\"\n\nfunc B() { _ = a.A }\n")

	cfg := config.Default()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(goextractor.New())
	eng.RegisterExplainer(cycles.New())

	ctx := context.Background()
	orig, err := eng.GenerateSnapshot(ctx, repo, false)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatalf("write artifacts: %v", err)
	}
	if orig.Meta.FactCount == 0 {
		t.Fatal("precondition: expected some facts")
	}
	if len(orig.Insights) == 0 {
		t.Fatal("precondition: expected the cycle to produce an insight")
	}

	// Restore into a BRAND-NEW engine (no extractors registered) — proves nothing
	// is re-extracted, only read from disk.
	fresh, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	label := filepath.Base(repo)
	dir := eng.OutputDir(repo)
	if err := fresh.RestoreFromDir(dir, map[string]string{label: repo}, label); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if got := fresh.Store().Count(); got != orig.Meta.FactCount {
		t.Errorf("restored fact count = %d, want %d", got, orig.Meta.FactCount)
	}
	snap := fresh.Snapshot()
	if snap == nil {
		t.Fatal("restored snapshot is nil")
	}
	if snap.Meta.GeneratedAt == "" {
		t.Error("restored Meta.GeneratedAt is empty (regression: timestamp not restored)")
	}
	if snap.Meta.GeneratedAt != orig.Meta.GeneratedAt {
		t.Errorf("restored generated_at = %q, want %q", snap.Meta.GeneratedAt, orig.Meta.GeneratedAt)
	}
	if len(snap.Insights) != len(orig.Insights) {
		t.Errorf("restored insights = %d, want %d", len(snap.Insights), len(orig.Insights))
	}
	if snap.Meta.RepoPath == "" {
		t.Error("restored Meta.RepoPath is empty")
	}
}

// TestStaleness_Age checks the age signal in isolation (no git, no global receipt).
// HOME is redirected to an empty temp dir so LoadGlobalReceipt misses and Staleness
// falls back to the in-memory snapshot's generated_at.
func TestStaleness_Age(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := config.Default()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	// Older than 24h -> stale.
	eng.SetSnapshot(&facts.Snapshot{Meta: facts.SnapshotMeta{
		GeneratedAt: now.Add(-48 * time.Hour).Format(time.RFC3339),
	}})
	if st := eng.Staleness(24*time.Hour, now); !st.TooOld || !st.Stale() {
		t.Errorf("48h-old snapshot: got TooOld=%v Stale=%v, want both true", st.TooOld, st.Stale())
	}

	// Within 24h -> fresh.
	eng.SetSnapshot(&facts.Snapshot{Meta: facts.SnapshotMeta{
		GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
	}})
	if st := eng.Staleness(24*time.Hour, now); st.TooOld || st.Stale() {
		t.Errorf("1h-old snapshot: got TooOld=%v Stale=%v, want both false", st.TooOld, st.Stale())
	}
}

// TestStaleness_CommitMoved checks the VCS-drift signal: a fresh-by-age snapshot
// whose recorded commit no longer matches HEAD is flagged as changed.
func TestStaleness_CommitMoved(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolateGit(t)
	t.Setenv("HOME", t.TempDir())

	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	writeFile(t, filepath.Join(repo, "f.txt"), "hello\n")
	git("add", ".")
	git("commit", "-m", "init")

	cfg := config.Default()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	eng.SetSnapshot(&facts.Snapshot{Meta: facts.SnapshotMeta{
		RepoPath:    repo,
		GeneratedAt: now.Format(time.RFC3339), // fresh by age
		Git:         &facts.GitInfo{Commit: "0000000000000000000000000000000000000000"},
	}})

	st := eng.Staleness(24*time.Hour, now)
	if st.TooOld {
		t.Errorf("snapshot is fresh by age, TooOld should be false")
	}
	if len(st.Changed) != 1 || st.Changed[0].Reason != "commit moved" {
		t.Errorf("got Changed=%+v, want one 'commit moved'", st.Changed)
	}
	if !st.Stale() {
		t.Error("commit moved should make Stale() true")
	}
}

// writeForeignGlobalReceipt writes ~/.enola/receipt.json describing a repo that is
// NOT in the loaded graph — exactly what a second enola server in another agent
// terminal leaves behind, since WriteGlobalReceipt rebuilds `repos` from its own
// graph rather than merging. generatedAt is deliberately old and the commit is
// deliberately wrong, so that adopting this file produces a visibly wrong verdict.
//
// The three staleness tests above set HOME to an empty temp dir specifically so
// LoadGlobalReceipt MISSES; this helper is what exercises the branch where it hits.
func writeForeignGlobalReceipt(t *testing.T, home, foreignRepoPath, generatedAt string) {
	t.Helper()
	receipt := map[string]any{
		"generated_at":  generatedAt,
		"enola_version": "dev",
		"snapshot_id":   "sha256:sibling-terminal",
		"fact_count":    4,
		"repos": []map[string]any{{
			"label": "foreign-sibling",
			"path":  foreignRepoPath,
			"git": map[string]any{
				"ref":    "main",
				"commit": "0000000000000000000000000000000000000000",
				"dirty":  false,
			},
			"added_at":   generatedAt,
			"fact_count": 4,
		}},
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".enola")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "receipt.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotGit_OwnOutputDirIsNotTreeDirt is the regression for enola recording its
// OWN artifacts as working-tree dirt. gitInfo decides dirtiness from any
// `git status --porcelain` output, and --porcelain lists untracked paths — so the
// .enola/ directory the snapshot itself creates (extractor-cache save, engine.go:328,
// which runs BEFORE the receipt reads git state) made a pristine committed repo record
// dirty: true.
//
// The repo here deliberately does NOT gitignore the output dir, which is the case that
// regresses. The second snapshot matters as much as the first: by then .enola/ already
// exists before the run begins, so call ordering is no longer the operative cause and
// only excluding the directory fixes it.
func TestSnapshotGit_OwnOutputDirIsNotTreeDirt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolateGit(t)
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module dirtmod\n\ngo 1.21\n")
	writeFile(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\nfunc A() {}\n")
	initGitRepo(t, repo)

	cfg := config.Default()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(goextractor.New())

	for _, run := range []string{"first", "second"} {
		snap, err := eng.GenerateSnapshot(context.Background(), repo, false)
		if err != nil {
			t.Fatalf("%s snapshot: %v", run, err)
		}
		if err := eng.WriteArtifacts(repo); err != nil {
			t.Fatalf("%s WriteArtifacts: %v", run, err)
		}
		if snap.Meta.Git == nil {
			t.Fatalf("%s run: no git info recorded", run)
		}
		if snap.Meta.Git.Dirty {
			t.Errorf("%s run: committed-clean repo recorded dirty:true — enola's own %s/ counted as tree dirt",
				run, cfg.Output.Dir)
		}
	}
}

// TestSnapshotGit_RealChangeStillDirty guards the fix above against over-suppressing:
// excluding the output dir must not blind the flag to actual uncommitted source changes.
func TestSnapshotGit_RealChangeStillDirty(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolateGit(t)
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module dirtmod2\n\ngo 1.21\n")
	writeFile(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\nfunc A() {}\n")
	initGitRepo(t, repo)

	// A real, uncommitted modification to a TRACKED file.
	writeFile(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\nfunc A() {}\nfunc B() {}\n")

	cfg := config.Default()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(goextractor.New())

	snap, err := eng.GenerateSnapshot(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Meta.Git == nil {
		t.Fatal("no git info recorded")
	}
	if !snap.Meta.Git.Dirty {
		t.Error("a modified tracked file must still record dirty:true")
	}
}

// TestStaleness_DetectsEditAfterCleanSnapshotWithoutGitignore is the payoff. A repo that
// does not gitignore the output dir previously recorded dirty:true at snapshot time,
// which killed the `!r.Git.Dirty && cur.Dirty` arm for its whole life — so a later edit
// produced no staleness signal at all. With the baseline recorded correctly, the arm is
// live again.
func TestStaleness_DetectsEditAfterCleanSnapshotWithoutGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolateGit(t)
	t.Setenv("HOME", t.TempDir())

	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module dirtmod3\n\ngo 1.21\n")
	writeFile(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\nfunc A() {}\n")
	initGitRepo(t, repo)

	cfg := config.Default()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(goextractor.New())
	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatal(err)
	}

	// Edit a tracked file after the snapshot, leaving it uncommitted.
	writeFile(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\nfunc A() {}\nfunc Added() {}\n")

	now := time.Now()
	st := eng.Staleness(24*time.Hour, now)
	if st.TooOld {
		t.Fatalf("snapshot is seconds old, TooOld should be false (Age=%s)", st.Age)
	}
	if len(st.Changed) != 1 || st.Changed[0].Reason != "uncommitted changes" {
		t.Errorf("got Changed=%+v, want one \"uncommitted changes\" for the edited repo", st.Changed)
	}
}

// isolateGit makes every git invocation in this test hermetic: instead of the
// developer's ~/.gitconfig and the machine's /etc/gitconfig, git reads exactly
// the fixture in testdata/gitconfig.
func isolateGit(t *testing.T) {
	t.Helper()
	cfg, err := filepath.Abs(filepath.Join("testdata", "gitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
}

// initGitRepo creates a git repo with one commit and returns its HEAD.
func initGitRepo(t *testing.T, repo string) string {
	t.Helper()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")

	writeFile(t, filepath.Join(repo, "f.txt"), "hello\n")
	git("add", ".")
	git("commit", "-m", "init")
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// TestStaleness_IgnoresForeignGlobalReceipt is the regression for the machine-wide
// receipt cross-talk. ~/.enola/receipt.json is shared by every enola process on the
// machine, so it routinely describes a DIFFERENT graph than the one this server
// loaded. Staleness must judge the loaded graph, never whatever the file names.
//
// Observed before the fix: a graph snapshotted 13 seconds earlier over a clean tree
// reported "graph is 5d old; <foreign> (commit moved)".
func TestStaleness_IgnoresForeignGlobalReceipt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolateGit(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	loaded := t.TempDir()
	head := initGitRepo(t, loaded)
	foreign := t.TempDir()
	initGitRepo(t, foreign)

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	// The foreign receipt is 5 days old and names a commit that matches nothing.
	writeForeignGlobalReceipt(t, home, foreign, now.Add(-5*24*time.Hour).Format(time.RFC3339))

	cfg := config.Default()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The loaded graph is fresh (1h) and its recorded commit is current HEAD.
	eng.SetSnapshot(&facts.Snapshot{Meta: facts.SnapshotMeta{
		RepoPath:    loaded,
		GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
		Git:         &facts.GitInfo{Ref: "main", Commit: head},
	}})

	st := eng.Staleness(24*time.Hour, now)
	if st.TooOld {
		t.Errorf("age was taken from the foreign receipt: TooOld=true for a 1h-old loaded snapshot (Age=%s)", st.Age)
	}
	for _, c := range st.Changed {
		if c.Label == "foreign-sibling" {
			t.Errorf("reported a repo that is not in the loaded graph: %+v", c)
		}
	}
	if st.Stale() {
		t.Errorf("fresh, unchanged loaded graph reported stale: TooOld=%v Changed=%+v", st.TooOld, st.Changed)
	}
}

// TestStaleness_ReportsLoadedRepoDespiteForeignReceipt is the other direction: the
// foreign receipt must not MASK real staleness in the loaded graph. Before the fix
// stalenessEntries returned the receipt's repo list verbatim, so the loaded repo was
// never git-checked at all and a moved HEAD went unreported.
func TestStaleness_ReportsLoadedRepoDespiteForeignReceipt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolateGit(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	loaded := t.TempDir()
	initGitRepo(t, loaded)
	foreign := t.TempDir()
	initGitRepo(t, foreign)

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	writeForeignGlobalReceipt(t, home, foreign, now.Add(-5*24*time.Hour).Format(time.RFC3339))

	cfg := config.Default()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Fresh by age, but the recorded commit does not match the loaded repo's HEAD.
	eng.SetSnapshot(&facts.Snapshot{Meta: facts.SnapshotMeta{
		RepoPath:    loaded,
		GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
		Git:         &facts.GitInfo{Ref: "main", Commit: "0000000000000000000000000000000000000000"},
	}})

	st := eng.Staleness(24*time.Hour, now)
	if len(st.Changed) != 1 {
		t.Fatalf("want exactly one Changed entry for the loaded repo, got %+v", st.Changed)
	}
	if got := st.Changed[0].Label; got != filepath.Base(loaded) {
		t.Errorf("Changed names %q, want the loaded repo %q", got, filepath.Base(loaded))
	}
	if st.Changed[0].Reason != "commit moved" {
		t.Errorf("got Reason=%q, want \"commit moved\"", st.Changed[0].Reason)
	}
}

// TestStaleness_AgeFromLoadedSnapshotNotForeignReceipt isolates the age signal from
// the git signal: same commit on both sides, so only the timestamp can differ. The
// loaded graph is 1h old and the foreign receipt is 5 days old.
func TestStaleness_AgeFromLoadedSnapshotNotForeignReceipt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolateGit(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	loaded := t.TempDir()
	head := initGitRepo(t, loaded)

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	// Foreign receipt points at the LOADED repo path but carries a 5-day-old
	// timestamp, so only the age source distinguishes pass from fail.
	writeForeignGlobalReceipt(t, home, loaded, now.Add(-5*24*time.Hour).Format(time.RFC3339))

	cfg := config.Default()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.SetSnapshot(&facts.Snapshot{Meta: facts.SnapshotMeta{
		RepoPath:    loaded,
		GeneratedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
		Git:         &facts.GitInfo{Ref: "main", Commit: head},
	}})

	st := eng.Staleness(24*time.Hour, now)
	if st.TooOld {
		t.Errorf("age came from the receipt (5d), not the loaded snapshot (1h): Age=%s", st.Age)
	}
	if want := 1 * time.Hour; st.Age != want {
		t.Errorf("Age=%s, want %s (the loaded snapshot's own generated_at)", st.Age, want)
	}
}

// A graph published as built and the same graph restored from disk hold every fact's
// relations in the same order, so a tool answers the same whichever way it got there.
func TestRestoreFromDir_RelationOrderMatchesBuilt(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module testmod\n\ngo 1.21\n")
	writeFile(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\nimport \"testmod/pkg/b\"\n\nfunc Z() {}\n\nfunc A() { b.B(); Z(); M() }\n\nfunc M() {}\n")
	writeFile(t, filepath.Join(repo, "pkg", "b", "b.go"), "package b\n\nfunc B() {}\n")

	cfg := config.Default()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(goextractor.New())
	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatalf("write artifacts: %v", err)
	}
	built := map[string][]facts.Relation{}
	multi := 0
	for _, f := range eng.Store().FactsRef() {
		built[f.Kind+"\x00"+f.Name+"\x00"+f.File] = f.Relations
		if len(f.Relations) > 2 {
			multi++
		}
	}
	if multi == 0 {
		t.Fatal("precondition: expected a fact with several relations")
	}

	fresh, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	label := filepath.Base(repo)
	if err := fresh.RestoreFromDir(eng.OutputDir(repo), map[string]string{label: repo}, label); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, f := range fresh.Store().FactsRef() {
		want, ok := built[f.Kind+"\x00"+f.Name+"\x00"+f.File]
		if !ok {
			t.Errorf("restored %s %s not in the built graph", f.Kind, f.Name)
			continue
		}
		if !reflect.DeepEqual(f.Relations, want) {
			t.Errorf("%s %s: built %v, restored %v", f.Kind, f.Name, want, f.Relations)
		}
	}
}
