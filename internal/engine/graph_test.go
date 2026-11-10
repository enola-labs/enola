package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enola-labs/enola/internal/config"
)

func graphEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func graphRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// Unloading a graph must let go of every reference the engine keeps to it, not only
// the published one: after a build, the scratch store is the published store, and the
// last facts.jsonl write remembers it too. Missing either, an unloaded graph stays
// resident and a memory budget unloads nothing.
func TestAdopt_NilReleasesEveryReference(t *testing.T) {
	repo := graphRepo(t)
	e := graphEngine(t)
	if _, err := e.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteArtifacts(repo); err != nil {
		t.Fatal(err)
	}
	published := e.current.Load().store
	if e.store != published || e.lastFacts.store != published {
		t.Fatalf("precondition: the scratch store and the last write should be the published store")
	}
	e.Adopt(nil)
	if e.Graph() != nil {
		t.Error("the engine still serves a graph")
	}
	if e.store == published {
		t.Error("the scratch store still holds the unloaded graph")
	}
	if e.lastFacts.store == published {
		t.Error("the last facts.jsonl write still holds the unloaded graph")
	}
}

// A graph published by one engine is served by another as it is, and is current only
// while the tree it was built from is unchanged.
func TestGraph_SharedAndCurrent(t *testing.T) {
	repo := graphRepo(t)
	a := graphEngine(t)
	if _, err := a.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatal(err)
	}
	g := a.Graph()
	b := graphEngine(t)
	b.Adopt(g)
	if b.Snapshot() != a.Snapshot() || !b.Graph().Same(g) {
		t.Fatal("the adopting engine does not serve the same graph")
	}
	abs, _ := filepath.Abs(repo)
	if got := g.RepoPaths(); len(got) != 1 || got[0] != abs {
		t.Errorf("RepoPaths = %v, want [%s]", got, abs)
	}
	if !b.Current(g) {
		t.Error("an unchanged tree is not current")
	}
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b.Current(g) {
		t.Error("a tree with a new file is still current")
	}
}

// Engines sharing a drift memo hash a file once between them.
func TestDriftMemo_SharedBetweenEngines(t *testing.T) {
	requireMemo(t)
	repo := graphRepo(t)
	a := graphEngine(t)
	if _, err := a.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatal(err)
	}
	memo := NewDriftMemo()
	// An hour from now every file's change time has settled, so the memo trusts it.
	memo.m.clock = func() time.Time { return time.Now().Add(time.Hour) }
	a.UseDriftMemo(memo)
	b := graphEngine(t)
	b.UseDriftMemo(memo)
	g := a.Graph()
	a.Current(g)
	reads := memo.m.reads.Load()
	b.Adopt(g)
	b.Current(g)
	if got := memo.m.reads.Load(); got != reads {
		t.Errorf("the second engine read %d files the first had hashed", got-reads)
	}
}
