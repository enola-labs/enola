package bootstrap_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// newEngine builds an engine with defaults (no config file on disk).
func newEngine(t *testing.T) *bootstrap.Engine {
	t.Helper()
	eng, _, err := bootstrap.NewEngine(bootstrap.Options{
		ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml"),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return eng
}

// writeGoRepo creates a minimal single-package Go module in a temp dir.
func writeGoRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/smoke\n\ngo 1.25\n")
	write("main.go", "package main\n\nfunc Greet() string { return \"hi\" }\n\nfunc main() { _ = Greet() }\n")
	return dir
}

// engineFor builds an engine whose configured workspace is repo — the equivalent
// of one agent terminal launching a server there. The returned config is the same
// pointer the engine holds, as bootstrap.NewEngine hands out.
func engineFor(t *testing.T, repo string) (*bootstrap.Engine, *config.Config) {
	t.Helper()
	eng, cfg, err := bootstrap.NewEngine(bootstrap.Options{
		ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml"),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	cfg.Repo = repo
	return eng, cfg
}

// writeBiggerGoRepo creates a Go module with more symbols than writeGoRepo, so a
// restore that picked up the wrong workspace is detectable by fact count alone.
func writeBiggerGoRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/bigger\n\ngo 1.25\n")
	write("main.go", "package main\n\nfunc main() { _ = Alpha() }\n")
	write("alpha/alpha.go", "package alpha\n\ntype Alpha struct{}\n\nfunc (a Alpha) One() int { return 1 }\n\nfunc (a Alpha) Two() int { return 2 }\n")
	write("beta/beta.go", "package beta\n\ntype Beta struct{}\n\nfunc (b Beta) Three() int { return 3 }\n\nfunc Helper() {}\n")
	return dir
}

// TestNewEngine_WiresPlugins asserts that NewEngine registers the full OSS
// explainer set and the renderer, and that the go extractor runs. Explainers
// always run, so their presence in the snapshot meta proves they were wired;
// the extractor list reflects only languages detected in the fixture.
func TestNewEngine_WiresPlugins(t *testing.T) {
	eng := newEngine(t)
	snap, err := eng.GenerateSnapshot(context.Background(), writeGoRepo(t), false)
	if err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}

	wantExplainers := []string{
		"cycles", "layers", "crossrepo", "coverage", "god-class",
		"hotspots", "messaging-coverage", "dependency-depth", "exported-surface", "complexity-outliers",
	}
	for _, name := range wantExplainers {
		if !contains(snap.Meta.Explainers, name) {
			t.Errorf("explainer %q not wired; meta.Explainers = %v", name, snap.Meta.Explainers)
		}
	}

	if !contains(snap.Meta.Extractors, "go") {
		t.Errorf("go extractor did not run; meta.Extractors = %v", snap.Meta.Extractors)
	}
	if !contains(snap.Meta.Renderers, "llm_context") {
		t.Errorf("llm_context renderer not wired; meta.Renderers = %v", snap.Meta.Renderers)
	}
}

func TestNewEngine_SmokeGenerate(t *testing.T) {
	eng := newEngine(t)
	snap, err := eng.GenerateSnapshot(context.Background(), writeGoRepo(t), false)
	if err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	if snap == nil {
		t.Fatal("snapshot is nil")
	}
	if eng.Store().Count() == 0 {
		t.Error("expected non-empty fact store after generate")
	}
	// The Greet symbol should have been extracted (name format is module-prefixed).
	found := false
	for _, f := range eng.Store().ByKind(facts.KindSymbol) {
		if strings.Contains(f.Name, "Greet") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a symbol fact for Greet")
	}
}

// TestAutoLoadSnapshot verifies that an existing .enola/facts.jsonl is loaded
// into the engine without a generate call. HOME is isolated so no real global
// receipt exists, exercising the single-repo fallback path.
func TestAutoLoadSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	eng, cfg, err := bootstrap.NewEngine(bootstrap.Options{
		ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml"),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	repo := t.TempDir()
	enolaDir := filepath.Join(repo, cfg.Output.Dir)
	if err := os.MkdirAll(enolaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a small facts file using the same serializer production uses.
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "pkg/x", File: "pkg/x", Props: map[string]any{"language": "go"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "pkg/x.Foo", File: "pkg/x/x.go", Line: 1,
			Props: map[string]any{"symbol_kind": "function", "exported": true}},
	)
	if err := store.WriteJSONLFile(filepath.Join(enolaDir, "facts.jsonl")); err != nil {
		t.Fatalf("WriteJSONLFile: %v", err)
	}

	cfg.Repo = repo
	bootstrap.AutoLoadSnapshot(eng, cfg)

	if eng.Store().Count() != 2 {
		t.Errorf("expected 2 facts loaded, got %d", eng.Store().Count())
	}
	if eng.Snapshot() == nil {
		t.Error("expected snapshot to be set after AutoLoadSnapshot")
	}
}

// TestAutoLoadSnapshot_FromGlobalReceipt verifies the multi-repo restore path: a
// prior session's graph receipt reloads the WHOLE graph — every repo appended to
// it — and not merely cfg.Repo's own snapshot dir.
//
// The proof is that cfg.Repo's own .enola is deleted before the restart, so the
// single-repo fallback cannot be the source; only the receipt, which points at the
// appended repo's dir holding the complete store, can supply the facts. HOME is
// isolated so the receipt written here is the only one seen.
func TestAutoLoadSnapshot_FromGlobalReceipt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Session 1: snapshot the workspace, then append a second repo to the graph.
	repo := writeGoRepo(t)
	appended := writeBiggerGoRepo(t)
	eng1, cfg1 := engineFor(t, repo)
	if _, err := eng1.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	snap, err := eng1.GenerateSnapshot(context.Background(), appended, true)
	if err != nil {
		t.Fatalf("GenerateSnapshot (append): %v", err)
	}
	// In append mode WriteArtifacts writes the entire store, so the appended
	// repo's dir holds every repo's facts.
	if err := eng1.WriteArtifacts(appended); err != nil {
		t.Fatalf("WriteArtifacts: %v", err)
	}
	if err := eng1.WriteGlobalReceipt(); err != nil {
		t.Fatalf("WriteGlobalReceipt: %v", err)
	}

	// Remove the workspace's own artifacts so a single-repo restore is impossible.
	if err := os.RemoveAll(filepath.Join(repo, cfg1.Output.Dir)); err != nil {
		t.Fatal(err)
	}

	// Session 2 (restart) in the same workspace.
	eng2, cfg2 := engineFor(t, repo)
	bootstrap.AutoLoadSnapshot(eng2, cfg2)

	if got := eng2.Store().Count(); got != snap.Meta.FactCount {
		t.Errorf("restored %d facts, want %d (the whole multi-repo graph)", got, snap.Meta.FactCount)
	}
	if len(eng2.RepoPaths()) != 2 {
		t.Errorf("restored %d repo paths, want 2", len(eng2.RepoPaths()))
	}
	restored := eng2.Snapshot()
	if restored == nil || restored.Meta.GeneratedAt == "" {
		t.Fatalf("expected a restored snapshot with generated_at, got %+v", restored)
	}
}

// A restart restores the graph without re-snapshotting, so the corpus that graph
// was extracted from has to come back too — otherwise queries against it are
// priced as if it were tiny. AutoLoadSnapshot returns that measurement from the
// SAME receipt the facts came from, so the two can never describe different repo
// sets.
func TestAutoLoadSnapshot_ReturnsRestoredCorpus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	repo := writeGoRepo(t)
	appended := writeBiggerGoRepo(t)
	eng1, _ := engineFor(t, repo)

	// Mirror the server: artifacts are written for each repo as it is indexed, so
	// every repo ends up with its own snapshot.meta.json carrying its own size.
	if _, err := eng1.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	if err := eng1.WriteArtifacts(repo); err != nil {
		t.Fatalf("WriteArtifacts: %v", err)
	}
	if _, err := eng1.GenerateSnapshot(context.Background(), appended, true); err != nil {
		t.Fatalf("GenerateSnapshot (append): %v", err)
	}
	if err := eng1.WriteArtifacts(appended); err != nil {
		t.Fatalf("WriteArtifacts (append): %v", err)
	}
	if err := eng1.WriteGlobalReceipt(); err != nil {
		t.Fatalf("WriteGlobalReceipt: %v", err)
	}

	// Session 2 (restart) in the same workspace.
	eng2, cfg2 := engineFor(t, repo)
	corpus := bootstrap.AutoLoadSnapshot(eng2, cfg2)

	if len(corpus) != 2 {
		t.Fatalf("restored corpus covers %d repos, want 2: %+v", len(corpus), corpus)
	}
	for path, tokens := range corpus {
		if tokens <= 0 {
			t.Errorf("repo %s restored with a non-positive corpus of %d", path, tokens)
		}
	}
	// The bigger repo must measure bigger — the map carries real sizes, not a
	// placeholder shared by every entry.
	if corpus[appended] <= corpus[repo] {
		t.Errorf("expected the larger repo to measure larger: %d vs %d", corpus[appended], corpus[repo])
	}
}

// TestAutoLoadSnapshot_PrefersOwnWorkspace is the cross-terminal regression test.
// A user typically runs one server per agent terminal; the machine-wide receipt
// describes only whichever generated last. A restart in workspace A must come back
// holding A's graph, not the graph the other terminal happened to snapshot.
func TestAutoLoadSnapshot_PrefersOwnWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Terminal 1 snapshots workspace A.
	repoA := writeGoRepo(t)
	engA, cfgA := engineFor(t, repoA)
	snapA, err := engA.GenerateSnapshot(context.Background(), repoA, false)
	if err != nil {
		t.Fatalf("GenerateSnapshot(A): %v", err)
	}
	if err := engA.WriteArtifacts(repoA); err != nil {
		t.Fatalf("WriteArtifacts(A): %v", err)
	}
	if err := engA.WriteGlobalReceipt(); err != nil {
		t.Fatalf("WriteGlobalReceipt(A): %v", err)
	}

	// Terminal 2 then snapshots a much larger workspace B, becoming the last
	// writer of ~/.enola/receipt.json.
	repoB := writeBiggerGoRepo(t)
	engB, _ := engineFor(t, repoB)
	snapB, err := engB.GenerateSnapshot(context.Background(), repoB, false)
	if err != nil {
		t.Fatalf("GenerateSnapshot(B): %v", err)
	}
	if err := engB.WriteArtifacts(repoB); err != nil {
		t.Fatalf("WriteArtifacts(B): %v", err)
	}
	if err := engB.WriteGlobalReceipt(); err != nil {
		t.Fatalf("WriteGlobalReceipt(B): %v", err)
	}
	if snapA.Meta.FactCount == snapB.Meta.FactCount {
		t.Fatalf("test setup: both workspaces have %d facts, so the restore cannot be told apart", snapA.Meta.FactCount)
	}

	// Terminal 1 restarts. It must come back with A's graph.
	restarted, cfgRestart := engineFor(t, cfgA.Repo)
	bootstrap.AutoLoadSnapshot(restarted, cfgRestart)

	if got := restarted.Store().Count(); got != snapA.Meta.FactCount {
		t.Errorf("restored %d facts, want %d (workspace A); %d would be workspace B's graph",
			got, snapA.Meta.FactCount, snapB.Meta.FactCount)
	}
	if snap := restarted.Snapshot(); snap == nil || snap.Meta.RepoPath != snapA.Meta.RepoPath {
		t.Errorf("restored repo path = %+v, want %s", snap, snapA.Meta.RepoPath)
	}
}

// TestAutoLoadSnapshot_IgnoresForeignGlobalReceipt covers the second half of the
// same cross-talk: a workspace with no receipt of its own must NOT adopt the
// machine-wide one when that receipt describes someone else's repo. Restoring it
// would silently hand this server another agent terminal's graph — answering
// every query about the wrong codebase.
func TestAutoLoadSnapshot_IgnoresForeignGlobalReceipt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Another terminal snapshots its workspace and writes the machine-wide receipt.
	foreign := writeBiggerGoRepo(t)
	engForeign, _ := engineFor(t, foreign)
	if _, err := engForeign.GenerateSnapshot(context.Background(), foreign, false); err != nil {
		t.Fatalf("GenerateSnapshot(foreign): %v", err)
	}
	if err := engForeign.WriteArtifacts(foreign); err != nil {
		t.Fatalf("WriteArtifacts(foreign): %v", err)
	}
	if err := engForeign.WriteGlobalReceipt(); err != nil {
		t.Fatalf("WriteGlobalReceipt(foreign): %v", err)
	}

	// A server starts in a never-snapshotted workspace. It must come up empty
	// rather than inheriting the foreign graph.
	mine, cfg := engineFor(t, t.TempDir())
	bootstrap.AutoLoadSnapshot(mine, cfg)

	if got := mine.Store().Count(); got != 0 {
		t.Errorf("restored %d facts into a workspace that has no snapshot; "+
			"the machine-wide receipt of another server was adopted", got)
	}
}

func TestLoadDashboardSnapshotKeepsOrdinaryRepoScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := writeGoRepo(t)
	other := writeBiggerGoRepo(t)
	eng, cfg := engineFor(t, repo)
	one, err := eng.GenerateSnapshot(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.GenerateSnapshot(context.Background(), other, true); err != nil {
		t.Fatal(err)
	}
	if err := eng.WriteArtifacts(other); err != nil {
		t.Fatal(err)
	}
	if err := eng.WriteGlobalReceipt(); err != nil {
		t.Fatal(err)
	}

	restarted, restartCfg := engineFor(t, cfg.Repo)
	if !bootstrap.LoadDashboardSnapshot(restarted, restartCfg) {
		t.Fatal("dashboard snapshot was not restored")
	}
	if got := restarted.Store().Count(); got != one.Meta.FactCount {
		t.Fatalf("dashboard restored %d facts, want the selected repo's %d", got, one.Meta.FactCount)
	}
	if got := len(restarted.RepoPaths()); got != 1 {
		t.Fatalf("dashboard restored %d repositories, want 1", got)
	}
}

// TestNewServer exercises the public server constructor and its accessors,
// which enterprise code relies on to register license-gated tools before Run.
func TestNewServer(t *testing.T) {
	eng, cfg, err := bootstrap.NewEngine(bootstrap.Options{
		ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml"),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	srv, err := bootstrap.NewServer(eng, cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.MCP() == nil {
		t.Error("MCP() returned nil; enterprise code needs it to register extra tools")
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
