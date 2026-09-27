package server_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// A CLI `--generate` (or `--generate --refresh`) runs in its own process and
// rewrites the artifacts and the workspace receipt under a server that keeps
// serving the graph it loaded at start. This is the sibling-process shape: one
// engine writes, a second engine serves the same workspace, and the served facts
// must follow the disk without a restart.
func TestE2E_ServerReloadsWhenASiblingProcessRewritesTheArtifacts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	ctx := context.Background()

	repo := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), t.TempDir())

	writer, writerCfg := newTestEngine(t)
	writerCfg.Repo = repo
	generateAndWrite := func() {
		t.Helper()
		if _, err := writer.GenerateSnapshot(ctx, repo, false); err != nil {
			t.Fatalf("writer GenerateSnapshot: %v", err)
		}
		if err := writer.WriteArtifacts(repo); err != nil {
			t.Fatalf("writer WriteArtifacts: %v", err)
		}
		if err := writer.WriteGlobalReceipt(); err != nil {
			t.Fatalf("writer WriteGlobalReceipt: %v", err)
		}
	}
	generateAndWrite()
	gr, err := engine.LoadWorkspaceReceipt(repo)
	if err != nil {
		t.Fatalf("workspace receipt: %v", err)
	}
	firstID := gr.SnapshotID

	served, servedCfg := newTestEngine(t)
	servedCfg.Repo = repo
	s := connect(t, served, servedCfg)

	got := text(s.call(t, "query_facts", map[string]any{"kind": "module"}))
	if !strings.Contains(got, "module") || strings.Contains(got, "Found 0") {
		t.Fatalf("a server started empty over a workspace with artifacts on disk must answer from them:\n%s", got)
	}
	if id := served.Snapshot().Meta.SnapshotID; id != firstID {
		t.Fatalf("served snapshot %q, disk %q", id, firstID)
	}

	if err := os.RemoveAll(filepath.Join(repo, "pkg")); err != nil {
		t.Fatal(err)
	}
	generateAndWrite()
	gr, err = engine.LoadWorkspaceReceipt(repo)
	if err != nil {
		t.Fatalf("workspace receipt: %v", err)
	}
	if gr.SnapshotID == firstID {
		t.Fatalf("the rewrite did not change the snapshot id; the fixture edit did not move the graph")
	}

	s.call(t, "query_facts", map[string]any{"kind": "module"})
	if id := served.Snapshot().Meta.SnapshotID; id != gr.SnapshotID {
		t.Fatalf("after the sibling rewrite the server serves %q, disk holds %q", id, gr.SnapshotID)
	}
}

// Agent sessions started in one directory share the workspace receipt, so a sibling
// session snapshotting OTHER code rewrites it too. That is not a refresh of this
// server's graph: it keeps serving its own and says, once, what happened.
func TestE2E_ServerKeepsItsGraphWhenASiblingSessionSnapshotsOtherRepos(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	ctx := context.Background()
	workspace := t.TempDir()

	snapshot := func(eng *bootstrap.Engine, repo string) string {
		t.Helper()
		snap, err := eng.GenerateSnapshot(ctx, repo, false)
		if err != nil {
			t.Fatalf("GenerateSnapshot(%s): %v", repo, err)
		}
		if err := eng.WriteArtifacts(repo); err != nil {
			t.Fatalf("WriteArtifacts(%s): %v", repo, err)
		}
		if err := eng.WriteGlobalReceipt(); err != nil {
			t.Fatalf("WriteGlobalReceipt(%s): %v", repo, err)
		}
		return snap.Meta.SnapshotID
	}

	served, servedCfg := newTestEngine(t)
	servedCfg.Repo = workspace
	ownID := snapshot(served, copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), t.TempDir()))
	s := connect(t, served, servedCfg)

	sibling, siblingCfg := newTestEngine(t)
	siblingCfg.Repo = workspace
	snapshot(sibling, copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_gin_sample"), t.TempDir()))

	first := text(s.call(t, "query_facts", map[string]any{"kind": "module", "output_mode": "summary"}))
	if id := served.Snapshot().Meta.SnapshotID; id != ownID {
		t.Fatalf("a sibling session's snapshot of other code replaced this server's graph: serving %q, own %q", id, ownID)
	}
	if !strings.Contains(first, "Another agent session in this workspace") {
		t.Errorf("the skipped reload was not reported to the agent:\n%s", first)
	}
	second := text(s.call(t, "query_facts", map[string]any{"kind": "module", "output_mode": "summary"}))
	if strings.Contains(second, "Another agent session in this workspace") {
		t.Errorf("the notice repeated on the next call:\n%s", second)
	}
}
