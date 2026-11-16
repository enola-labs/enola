package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
)

// writeSnapshotFiles lays out a snapshot dir as a build wrote it: a receipt (with a
// cross-repo coverage rollup when it was a cluster's), facts and a meta.
func writeSnapshotFiles(t *testing.T, dir string, cluster bool, generatedAt string, extra ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One repository's receipt carries a coverage rollup too, with no services, when an
	// extractor reported what it could not resolve; a cluster's counts its services.
	quality := map[string]any{"files_seen": 1,
		"coverage": map[string]any{"services_total": 0, "extractors_reporting": 2, "extraction_unresolved": 346}}
	if cluster {
		quality["coverage"] = map[string]any{"services_total": 2}
	}
	rcpt, _ := json.Marshal(map[string]any{"generated_at": generatedAt, "quality": quality,
		"output_hashes": map[string]string{"facts.jsonl": "x", "flows.json": "y"}})
	files := map[string]string{"receipt.json": string(rcpt), "facts.jsonl": "{}\n", "snapshot.meta.json": "{}", "flows.json": "[]"}
	for _, name := range extra {
		files[name] = "x"
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// An older enola wrote a cluster's graph into every member: at the top of its output
// dir, rotated into previous/, and pinned in the anchor's baseline/. Pruning finds
// exactly those, moves the cluster's pin to the cluster's own directory, removes the
// rest, and leaves what is the member's own: its extractor cache, and a previous/
// that is its own snapshot.
func TestClusterCopies_FoundMovedAndRemoved(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	api, web := filepath.Join(root, "api"), filepath.Join(root, "web")
	cfg := config.Default()
	outAPI, outWeb := cfg.OutputDirFor(api), cfg.OutputDirFor(web)

	writeSnapshotFiles(t, outAPI, true, "2026-11-02T00:00:00Z", "llm_context.md", "extractor_cache.json")
	writeSnapshotFiles(t, filepath.Join(outAPI, engine.PreviousSubdir), true, "2026-11-01T00:00:00Z")
	writeSnapshotFiles(t, filepath.Join(outAPI, engine.BaselineSubdir), true, "2026-11-01T00:00:00Z", engine.PinMarkFile)
	writeSnapshotFiles(t, outWeb, true, "2026-11-02T00:00:00Z", "extractor_cache.json")
	writeSnapshotFiles(t, filepath.Join(outWeb, engine.PreviousSubdir), false, "2026-10-01T00:00:00Z")
	// web's union files are links to api's, as the old writer left them.
	if err := os.Remove(filepath.Join(outWeb, "facts.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(outAPI, "facts.jsonl"), filepath.Join(outWeb, "facts.jsonl")); err != nil {
		t.Skip("no hard links here")
	}

	copies := engine.FindClusterCopies(cfg, []string{api, web})
	where := map[string]bool{}
	for _, c := range copies {
		where[filepath.Base(c.Member)+"/"+c.Where] = true
	}
	want := []string{"api/snapshot", "api/previous", "api/baseline", "web/snapshot"}
	if len(copies) != len(want) {
		t.Fatalf("found %v, want %v", where, want)
	}
	for _, w := range want {
		if !where[w] {
			t.Errorf("missed %s; found %v", w, where)
		}
	}

	b := engine.MeasureClusterCopies(copies)
	if b.Apparent == 0 || b.Freed == 0 || b.Freed > b.Apparent {
		t.Errorf("measured %+v", b)
	}

	home := cfg.GraphOutputDir([]string{api, web})
	moved, err := engine.PruneClusterCopies(copies, home)
	if err != nil {
		t.Fatal(err)
	}
	if moved == nil || moved.Where != "baseline" {
		t.Fatalf("the cluster's pin was not moved: %+v", moved)
	}
	for _, name := range []string{"facts.jsonl", "receipt.json", engine.PinMarkFile} {
		if _, err := os.Stat(filepath.Join(home, engine.BaselineSubdir, name)); err != nil {
			t.Errorf("the cluster's directory has no baseline/%s: %v", name, err)
		}
	}
	if left := engine.FindClusterCopies(cfg, []string{api, web}); len(left) != 0 {
		t.Errorf("%d copies left after pruning", len(left))
	}
	for _, keep := range []string{
		filepath.Join(outAPI, "extractor_cache.json"),
		filepath.Join(outWeb, "extractor_cache.json"),
		filepath.Join(outWeb, engine.PreviousSubdir, "facts.jsonl"),
	} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("pruning removed the member's own %s", keep)
		}
	}
	for _, gone := range []string{filepath.Join(outAPI, "llm_context.md"), filepath.Join(outWeb, "flows.json")} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("pruning left %s", gone)
		}
	}
}

// A cluster that already has its own pin keeps it: a copy's baseline is removed, not
// moved over it.
func TestClusterCopies_HomePinIsKept(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	api, web := filepath.Join(root, "api"), filepath.Join(root, "web")
	cfg := config.Default()
	writeSnapshotFiles(t, filepath.Join(cfg.OutputDirFor(api), engine.BaselineSubdir), true, "2026-11-01T00:00:00Z")
	home := cfg.GraphOutputDir([]string{api, web})
	writeSnapshotFiles(t, filepath.Join(home, engine.BaselineSubdir), true, "2026-11-05T00:00:00Z")
	before, _ := os.ReadFile(filepath.Join(home, engine.BaselineSubdir, "receipt.json"))

	moved, err := engine.PruneClusterCopies(engine.FindClusterCopies(cfg, []string{api, web}), home)
	if err != nil {
		t.Fatal(err)
	}
	if moved != nil {
		t.Errorf("moved %s over the cluster's own pin", moved.Dir)
	}
	if after, _ := os.ReadFile(filepath.Join(home, engine.BaselineSubdir, "receipt.json")); string(after) != string(before) {
		t.Error("the cluster's own pin was replaced")
	}
	if _, err := os.Stat(filepath.Join(cfg.OutputDirFor(api), engine.BaselineSubdir)); err == nil {
		t.Error("the copy's baseline was left")
	}
}

// A repository's own snapshot, previous run and pin are never taken for a cluster's,
// even when its receipt carries a coverage rollup: gc would delete them.
func TestClusterCopies_OwnSnapshotIsNotACopy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	api, web := filepath.Join(root, "api"), filepath.Join(root, "web")
	cfg := config.Default()
	out := cfg.OutputDirFor(api)
	for _, d := range []string{out, filepath.Join(out, engine.PreviousSubdir), filepath.Join(out, engine.BaselineSubdir)} {
		writeSnapshotFiles(t, d, false, "2026-11-16T00:00:00Z")
	}
	if copies := engine.FindClusterCopies(cfg, []string{api, web}); len(copies) != 0 {
		t.Fatalf("a repository's own snapshots were taken for cluster copies: %+v", copies)
	}
	if _, err := engine.PruneClusterCopies(engine.FindClusterCopies(cfg, []string{api, web}), cfg.GraphOutputDir([]string{api, web})); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{out, filepath.Join(out, engine.PreviousSubdir), filepath.Join(out, engine.BaselineSubdir)} {
		if _, err := os.Stat(filepath.Join(d, "facts.jsonl")); err != nil {
			t.Errorf("pruning removed %s", d)
		}
	}
}
