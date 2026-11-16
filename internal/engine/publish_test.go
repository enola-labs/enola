package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// A reader in another process can open a graph's files while a write publishes the
// next one, and get facts of one write beside the meta of another. The restore checks
// the files it opened against the hashes the meta recorded and refuses a mix, rather
// than serving a graph that never existed.
func TestRestore_RefusesFilesOfTwoWrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repo := copyTree(t, filepath.Join("testdata", "repos", "go_sample"), t.TempDir())
	eng, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatal(err)
	}
	if err := eng.WriteGraph(); err != nil {
		t.Fatal(err)
	}
	dir := eng.OutputDir(repo)
	if _, err := os.Stat(filepath.Join(dir, engine.StagingSubdir)); err == nil {
		t.Error("the staging directory was left behind")
	}

	reader, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.RestoreFromDir(dir, nil, "go_sample"); err != nil {
		t.Fatalf("a whole write was refused: %v", err)
	}

	// The next write's facts.jsonl, published, with this write's meta still in place.
	facts := filepath.Join(dir, "facts.jsonl")
	data, err := os.ReadFile(facts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(facts, append(data, data[:len(data)/2]...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reader.RestoreFromDir(dir, nil, "go_sample"); !errors.Is(err, engine.ErrGraphChanging) {
		t.Fatalf("files of two writes were restored: err = %v", err)
	}
}

// A process starting while a write publishes the next graph restores the one before
// it, from previous/, and answers from that rather than from nothing.
func TestAutoLoad_RestoresPreviousWhileTheGraphIsReplaced(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repo := copyTree(t, filepath.Join("testdata", "repos", "go_sample"), t.TempDir())
	cfgPath := filepath.Join(t.TempDir(), "mcp-arch.yaml")
	if err := os.WriteFile(cfgPath, []byte("repo: "+repo+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, cfg, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 2 { // the second write rotates the first into previous/
		if _, err := eng.GenerateSnapshot(ctx, repo, false); err != nil {
			t.Fatal(err)
		}
		if err := eng.WriteGraph(); err != nil {
			t.Fatal(err)
		}
	}
	want := eng.Store().Count()
	dir := eng.OutputDir(repo)
	if err := os.WriteFile(filepath.Join(dir, "facts.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fresh, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if corpus := bootstrap.AutoLoadSnapshot(fresh, cfg); corpus == nil && fresh.Store().Count() == 0 {
		t.Fatal("nothing was restored while the graph was being replaced")
	}
	if got := fresh.Store().Count(); got != want {
		t.Errorf("restored %d facts, want the %d of the whole graph in previous/", got, want)
	}
}
