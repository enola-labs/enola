package engine_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
)

// A repository's .enola is shared by every agent session working on it. Each run and
// each pin records the session behind it, and a run's record travels into previous/
// with the rest of the run, so a diff can say whose "before" it compares against.
func TestSessionMarks_RecordWhoRanAndWhoPinned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module testmod\n\ngo 1.21\n")
	writeFile(t, filepath.Join(repo, "pkg", "a", "a.go"), "package a\n\nfunc A() {}\n")

	eng, err := engine.New(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(goextractor.New())
	ctx := context.Background()
	outDir := eng.OutputDir(repo)

	run := func(agent int) {
		t.Helper()
		eng.SetSessionClient(agent)
		if _, err := eng.GenerateSnapshot(ctx, repo, false); err != nil {
			t.Fatal(err)
		}
		if err := eng.WriteArtifacts(repo); err != nil {
			t.Fatal(err)
		}
	}

	run(111)
	if m := engine.ReadSessionMark(outDir, engine.RunMarkFile); m == nil || m.AgentPID != 111 {
		t.Fatalf("run mark = %+v, want agent 111", m)
	}
	if err := eng.SetBaseline(repo); err != nil {
		t.Fatal(err)
	}
	baseDir := engine.ResolveBaselineDir(outDir, "pinned")
	if m := engine.ReadSessionMark(baseDir, engine.PinMarkFile); m == nil || m.AgentPID != 111 || m.At == "" {
		t.Fatalf("pin mark = %+v, want agent 111 with a time", m)
	}

	run(222)
	if m := engine.ReadSessionMark(outDir, engine.RunMarkFile); m == nil || m.AgentPID != 222 {
		t.Fatalf("run mark after the second run = %+v, want agent 222", m)
	}
	prevDir := engine.ResolveBaselineDir(outDir, "previous")
	if m := engine.ReadSessionMark(prevDir, engine.RunMarkFile); m == nil || m.AgentPID != 111 {
		t.Fatalf("previous/ run mark = %+v, want the first run's agent 111", m)
	}
	if m := engine.ReadSessionMark(baseDir, engine.PinMarkFile); m == nil || m.AgentPID != 111 {
		t.Fatalf("a later run moved the pin mark: %+v", m)
	}
}
