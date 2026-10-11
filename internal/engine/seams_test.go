package engine_test

// Tests for the engine's edges that other packages lean on and that had none:
// what Drift says in words, what an engine answers before it holds a snapshot,
// the per-session receipts, and the artifact accessor. Each is a seam a split of
// the engine would cut along, so each is pinned from outside the package.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
	"github.com/enola-labs/enola/internal/facts"
)

func TestDriftSummary(t *testing.T) {
	cases := []struct {
		name     string
		drift    engine.Drift
		maxPaths int
		want     string
		any      bool
		count    int
	}{
		{name: "clean", drift: engine.Drift{}, maxPaths: 3, want: ""},
		{
			name:  "unknown is not change",
			drift: engine.Drift{Unknown: true}, maxPaths: 3,
			want: "the snapshot recorded no file hashes, so whether it still matches the working tree cannot be verified",
		},
		{
			name:  "unknown with a reason says the reason",
			drift: engine.Drift{Unknown: true, Reason: "indexed under other ignore globs"}, maxPaths: 3,
			want: "indexed under other ignore globs",
		},
		{
			name:  "one modified",
			drift: engine.Drift{Modified: []string{"a.go"}}, maxPaths: 3,
			want: "1 modified (e.g. a.go)", any: true, count: 1,
		},
		{
			name: "every kind, in modified-added-removed order",
			drift: engine.Drift{
				Added: []string{"new.go"}, Removed: []string{"old.go"}, Modified: []string{"a.go", "b.go"},
			},
			maxPaths: 10,
			want:     "2 modified, 1 added, 1 removed (e.g. a.go, b.go, new.go, old.go)", any: true, count: 4,
		},
		{
			name: "sample is capped and says so",
			drift: engine.Drift{
				Added: []string{"new.go"}, Modified: []string{"a.go", "b.go", "c.go"},
			},
			maxPaths: 2,
			want:     "3 modified, 1 added (e.g. a.go, b.go, …)", any: true, count: 4,
		},
		{
			name:  "no sample asked for",
			drift: engine.Drift{Removed: []string{"old.go"}}, maxPaths: 0,
			want: "1 removed", any: true, count: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.drift.Summary(tc.maxPaths); got != tc.want {
				t.Errorf("Summary(%d) = %q, want %q", tc.maxPaths, got, tc.want)
			}
			if got := tc.drift.Any(); got != tc.any {
				t.Errorf("Any() = %v, want %v", got, tc.any)
			}
			if got := tc.drift.Count(); got != tc.count {
				t.Errorf("Count() = %d, want %d", got, tc.count)
			}
		})
	}
}

// An engine that has not snapshotted anything answers "nothing yet" on every
// accessor. None of them may panic or invent an empty-but-valid answer: a caller
// tells "no snapshot" from "an empty snapshot" by these.
func TestEngine_BeforeAnySnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	eng, err := engine.New(config.Default())
	if err != nil {
		t.Fatal(err)
	}

	if snap := eng.Snapshot(); snap != nil {
		t.Errorf("Snapshot() = %+v, want nil", snap)
	}
	if d, err := eng.Drift(repo); err != nil || !d.Unknown || d.Any() {
		t.Errorf("Drift() = %+v, %v; want Unknown and no change", d, err)
	}
	if err := eng.WriteArtifacts(repo); err == nil {
		t.Error("WriteArtifacts must refuse when there is no snapshot")
	}
	if err := eng.WriteGlobalReceipt(); err == nil {
		t.Error("WriteGlobalReceipt must refuse when there is no snapshot")
	}
	if _, err := eng.GetArtifact("facts.jsonl"); err == nil {
		t.Error("GetArtifact must refuse when there is no snapshot")
	}
	if gr := eng.GraphReceipt(); gr != nil {
		t.Errorf("GraphReceipt() = %+v, want nil", gr)
	}
	if _, snap, gr := eng.DashboardState(); snap != nil || gr != nil {
		t.Errorf("DashboardState() snapshot=%v receipt=%v, want both nil", snap, gr)
	}
	if same, loaded := eng.ServesReposOf(&facts.GraphReceipt{}); same || loaded {
		t.Errorf("ServesReposOf = same %v loaded %v, want neither", same, loaded)
	}
	if st := eng.Staleness(time.Hour, time.Now()); st.Stale() {
		t.Errorf("an engine holding nothing cannot be stale; got %+v", st)
	}
	if err := eng.SetBaseline(repo); err == nil || !strings.Contains(err.Error(), "no snapshot to pin") {
		t.Errorf("SetBaseline with nothing on disk = %v, want a 'no snapshot to pin' error", err)
	}
	if _, err := os.Stat(engine.ResolveBaselineDir(eng.OutputDir(repo), "pinned")); !os.IsNotExist(err) {
		t.Errorf("a refused pin must leave no baseline directory behind; stat err = %v", err)
	}
}

func seamEngine(t *testing.T, repo string, agentPID int) *engine.Engine {
	t.Helper()
	cfg := config.Default()
	cfg.Repo = repo
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(goextractor.New())
	if agentPID > 0 {
		eng.SetSessionClient(agentPID)
	}
	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return eng
}

// Each agent session keeps its own receipt beside the workspace's, so a server
// restarted under one session reloads that session's graph and not whatever a
// sibling wrote last.
func TestSessionReceipts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := driftRepo(t)
	other := driftRepo(t)

	if got, err := engine.SessionReceipts(repo); err == nil && len(got) != 0 {
		t.Errorf("no session has written yet; got %v", got)
	}

	first := seamEngine(t, repo, 111)
	if err := first.WriteGlobalReceipt(); err != nil {
		t.Fatal(err)
	}
	second := seamEngine(t, repo, 222)
	if err := second.WriteGlobalReceipt(); err != nil {
		t.Fatal(err)
	}
	// A CLI run serves no session and must not leave a session receipt.
	cli := seamEngine(t, repo, 0)
	if err := cli.WriteGlobalReceipt(); err != nil {
		t.Fatal(err)
	}

	receipts, err := engine.SessionReceipts(repo)
	if err != nil {
		t.Fatalf("SessionReceipts: %v", err)
	}
	if len(receipts) != 2 || receipts[111] == "" || receipts[222] == "" {
		t.Fatalf("SessionReceipts = %v, want one each for 111 and 222", receipts)
	}
	for pid, path := range receipts {
		want, err := engine.SessionReceiptPath(repo, pid)
		if err != nil {
			t.Fatal(err)
		}
		if path != want {
			t.Errorf("session %d receipt at %s, SessionReceiptPath says %s", pid, path, want)
		}
	}
	if got, err := engine.SessionReceipts(other); err == nil && len(got) != 0 {
		t.Errorf("another workspace must not see these sessions; got %v", got)
	}

	gr, err := engine.LoadSessionReceipt(repo, 111)
	if err != nil {
		t.Fatalf("LoadSessionReceipt: %v", err)
	}
	if len(gr.Repos) != 1 || gr.Writer == nil || gr.Writer.ClientPID != 111 {
		t.Errorf("session receipt = %+v, want one repo written for client 111", gr)
	}
	if _, err := engine.LoadSessionReceipt(repo, 999); err == nil {
		t.Error("a session that never wrote has no receipt to load")
	}

	// The workspace and machine-wide receipts describe the same graph.
	ws, err := engine.LoadWorkspaceReceipt(repo)
	if err != nil {
		t.Fatalf("LoadWorkspaceReceipt: %v", err)
	}
	global, err := engine.LoadGlobalReceipt()
	if err != nil {
		t.Fatalf("LoadGlobalReceipt: %v", err)
	}
	if ws.SnapshotID != gr.SnapshotID || global.SnapshotID != gr.SnapshotID {
		t.Errorf("receipts disagree on the snapshot: session %s, workspace %s, global %s",
			gr.SnapshotID, ws.SnapshotID, global.SnapshotID)
	}
}

func TestServesReposOf(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := driftRepo(t)
	eng := seamEngine(t, repo, 0)

	if same, loaded := eng.ServesReposOf(eng.GraphReceipt()); !same || !loaded {
		t.Errorf("an engine serves the repos of its own receipt; got same %v loaded %v", same, loaded)
	}
	elsewhere := &facts.GraphReceipt{Repos: []facts.GraphRepoEntry{{Label: "x", Path: t.TempDir()}}}
	if same, loaded := eng.ServesReposOf(elsewhere); same || !loaded {
		t.Errorf("a receipt for another repo is another graph; got same %v loaded %v", same, loaded)
	}
	superset := &facts.GraphReceipt{Repos: []facts.GraphRepoEntry{{Label: "a", Path: repo}, {Label: "x", Path: t.TempDir()}}}
	if same, loaded := eng.ServesReposOf(superset); same || !loaded {
		t.Errorf("a receipt naming an extra repo is another graph; got same %v loaded %v", same, loaded)
	}
	if same, loaded := eng.ServesReposOf(&facts.GraphReceipt{}); same || !loaded {
		t.Errorf("an empty receipt names no graph; got same %v loaded %v", same, loaded)
	}
}

// DashboardState hands out one generation: the store, the snapshot and the
// receipt it returns describe each other even if another snapshot is published
// right after.
func TestDashboardStateIsOneGeneration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := driftRepo(t)
	eng := seamEngine(t, repo, 0)

	store, snap, gr := eng.DashboardState()
	if store == nil || snap == nil || gr == nil {
		t.Fatalf("DashboardState = %v, %v, %v; want all three", store, snap, gr)
	}
	writeFile(t, filepath.Join(repo, "pkg", "c", "c.go"), "package c\n\nfunc C() {}\n")
	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatal(err)
	}

	if store.Count() != snap.Meta.FactCount || gr.FactCount != snap.Meta.FactCount {
		t.Errorf("one generation must agree on its fact count: store %d, snapshot %d, receipt %d",
			store.Count(), snap.Meta.FactCount, gr.FactCount)
	}
	if gr.SnapshotID != snap.Meta.SnapshotID {
		t.Errorf("receipt names snapshot %s, the snapshot is %s", gr.SnapshotID, snap.Meta.SnapshotID)
	}
	if next := eng.Store(); next == store || next.Count() <= store.Count() {
		t.Errorf("the next generation must be a new, larger store; had %d facts, now %d", store.Count(), next.Count())
	}
}

// GetArtifact serves from memory what WriteArtifacts puts on disk. The server
// and the dashboard read one, the gate reads the other, so they must be the same
// bytes.
func TestGetArtifactMatchesWhatIsWritten(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := driftRepo(t)
	eng := seamEngine(t, repo, 0)
	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatal(err)
	}
	outDir := eng.OutputDir(repo)

	for _, name := range []string{"facts.jsonl", "insights.json", "receipt.json", "snapshot.meta.json"} {
		served, err := eng.GetArtifact(name)
		if err != nil {
			t.Errorf("GetArtifact(%s): %v", name, err)
			continue
		}
		onDisk, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Errorf("%s was not written: %v", name, err)
			continue
		}
		if !bytes.Equal(bytes.TrimSpace(served), bytes.TrimSpace(onDisk)) {
			t.Errorf("%s: what GetArtifact serves differs from what WriteArtifacts wrote", name)
		}
	}
	if _, err := eng.GetArtifact("no-such-artifact"); err == nil {
		t.Error("an unknown artifact name must be an error")
	}
}

func TestReadSessionMark_MissingOrCorruptIsNil(t *testing.T) {
	dir := t.TempDir()
	if m := engine.ReadSessionMark(dir, engine.RunMarkFile); m != nil {
		t.Errorf("a missing mark = %+v, want nil", m)
	}
	writeFile(t, filepath.Join(dir, engine.RunMarkFile), "{not json")
	if m := engine.ReadSessionMark(dir, engine.RunMarkFile); m != nil {
		t.Errorf("a corrupt mark = %+v, want nil", m)
	}
}

func TestLoadSnapshotDir_MissingIsAnError(t *testing.T) {
	if _, err := engine.LoadSnapshotDir(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("loading a directory that holds no snapshot must be an error")
	}
}

// The snapshot recorder is the engine's only connection to the architecture
// history. It is called once per WriteArtifacts, after the artifacts it names are
// on disk, and it is handed the metadata as written.
func TestSnapshotRecorder_IsCalledAfterTheArtifactsAreWritten(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := driftRepo(t)
	eng := seamEngine(t, repo, 0)

	var records []engine.SnapshotRecord
	var factsOnDisk []bool
	eng.SetSnapshotRecorder(func(rec engine.SnapshotRecord) {
		records = append(records, rec)
		_, err := os.Stat(rec.FactsPath)
		factsOnDisk = append(factsOnDisk, err == nil)
	})

	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("one write produced %d records, want 1", len(records))
	}
	rec := records[0]
	if !factsOnDisk[0] {
		t.Error("the recorder ran before facts.jsonl was written")
	}
	if rec.Snapshot == nil || rec.Meta.SnapshotID == "" || rec.Meta.SnapshotID != eng.Snapshot().Meta.SnapshotID {
		t.Errorf("record names snapshot %q, the engine holds %q", rec.Meta.SnapshotID, eng.Snapshot().Meta.SnapshotID)
	}
	if rec.Meta.OutputHashes["facts.jsonl"] == "" {
		t.Error("the record's meta must carry the output hashes of what was written")
	}
	if want := filepath.Join(eng.OutputDir(repo), engine.PreviousSubdir); rec.PreviousDir != want {
		t.Errorf("PreviousDir = %s, want %s", rec.PreviousDir, want)
	}
	// A single repository has no vector of repo states; its own git info says it all.
	if got := rec.Repos(); got != nil {
		t.Errorf("single-repo Repos() = %v, want nil", got)
	}

	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Errorf("a second write produced %d records in total, want 2: deduplication is the recorder's job", len(records))
	}
}

// An engine nobody wired a recorder to writes its artifacts and records nothing.
func TestSnapshotRecorder_IsOptional(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := driftRepo(t)
	eng := seamEngine(t, repo, 0)
	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatalf("WriteArtifacts without a recorder: %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(home, ".enola", "graphs", "*", "history")); len(entries) != 0 {
		t.Errorf("a bare engine recorded a history at %v", entries)
	}
}
