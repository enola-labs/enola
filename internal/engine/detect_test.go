package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/extractors/cppextractor"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// writeDeep writes a file, creating every parent directory.
func writeDeep(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A repository whose only sources sit far below every bound detection used to
// impose must produce facts. This is the end of the bug the file-list detectors
// were added for: dotnet/runtime keeps 3,270 C/C++ sources with none inside three
// levels, so the extractor never ran and the language was simply absent — reported
// by nothing louder than one line of log output.
func TestSnapshot_DeepOnlyCppRepoYieldsFacts(t *testing.T) {
	dir := t.TempDir()
	writeDeep(t, dir, "src/main/native/engine/core/widget.h",
		"#pragma once\nnamespace core { class Widget { public: int area() const; }; }\n")
	writeDeep(t, dir, "src/main/native/engine/core/widget.cpp",
		"#include \"widget.h\"\nnamespace core { int Widget::area() const { return 7; } }\n")

	cfg := config.Default()
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(cppextractor.New())

	snap, err := eng.GenerateSnapshot(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	if len(snap.Facts) == 0 {
		t.Fatalf("deep-only C++ repo produced no facts")
	}
	var sawCpp bool
	for _, name := range snap.Meta.Extractors {
		if name == "cpp" {
			sawCpp = true
		}
	}
	if !sawCpp {
		t.Fatalf("extractors = %v, want cpp among them", snap.Meta.Extractors)
	}
	// The census is the metric this bug was sized with, and it must not report the
	// files as claimed-but-unread any more.
	if c := snap.Meta.Census; c != nil && c.SkippedWithCause > 0 {
		t.Errorf("census still reports %d files skipped with cause: %v", c.SkippedWithCause, c.TopSkipCauses)
	}
}

// A Go module that is not at the repository root must still be found. The rule this
// replaces stated a ROOT go.mod, which is not a depth bound but fails identically:
// ente carries its backend at server/go.mod and its CLI at cli/go.mod, and 493 Go
// files went unindexed in a repository whose whole point was the cross-repo edge
// between the Flutter client and that backend.
func TestSnapshot_NonRootGoModuleIsDetected(t *testing.T) {
	dir := t.TempDir()
	writeDeep(t, dir, "server/go.mod", "module example.com/server\n\ngo 1.22\n")
	writeDeep(t, dir, "server/internal/api/handler.go",
		"package api\n\nfunc Handle() int { return 1 }\n")

	cfg := config.Default()
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(goextractor.New())

	snap, err := eng.GenerateSnapshot(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	if len(snap.Facts) == 0 {
		t.Fatalf("non-root Go module produced no facts")
	}
}

// Vendored code must not detect, at any depth. Removing the bounds removed the
// accident that used to enforce this — a three-level scan simply could not see far
// enough into node_modules to be fooled by it — so the guarantee has to be explicit.
func TestSnapshot_VendoredSourcesDoNotDetect(t *testing.T) {
	dir := t.TempDir()
	writeDeep(t, dir, "node_modules/somepkg/native/thing.cpp", "int f() { return 1; }\n")
	writeDeep(t, dir, "vendor/otherpkg/src/thing.cpp", "int g() { return 2; }\n")
	writeDeep(t, dir, "README.md", "# nothing to see\n")

	cfg := config.Default()
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(cppextractor.New())

	snap, err := eng.GenerateSnapshot(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	for _, name := range snap.Meta.Extractors {
		if name == "cpp" {
			t.Fatalf("cpp detected on a repository whose only C++ is vendored")
		}
	}
}

// recordingDetector implements both halves so a test can prove which one the engine
// asked, and with what.
type recordingDetector struct {
	gotNames []string
	usedList bool
	usedWalk bool
}

func (r *recordingDetector) Name() string { return "recorder" }
func (r *recordingDetector) Detect(string) (bool, error) {
	r.usedWalk = true
	return true, nil
}
func (r *recordingDetector) DetectFiles(_ string, files []string) (bool, error) {
	r.usedList = true
	r.gotNames = files
	return true, nil
}
func (r *recordingDetector) Extract(context.Context, string, []string) ([]facts.Fact, error) {
	return nil, nil
}

var _ plugin.FileListDetector = (*recordingDetector)(nil)

// The engine must prefer DetectFiles, and must hand it the PRE-ignore, POST-prune
// names. Both halves are load-bearing and pull in opposite directions, so they are
// asserted together: an ignored file must be visible (a Dart repository is spelled
// by pubspec.yaml, and the bundled config ignores **/*.yaml), while a pruned
// directory must not be (vendoring C++ does not make a repository a C++ project).
func TestDetect_PrefersFileListWithPreIgnorePostPruneNames(t *testing.T) {
	dir := t.TempDir()
	writeDeep(t, dir, "app/main.go", "package main\n\nfunc main() {}\n")
	writeDeep(t, dir, "app/pubspec.yaml", "name: demo\n")
	writeDeep(t, dir, "node_modules/dep/index.ts", "export const x = 1\n")

	cfg := config.Default()
	cfg.Ignore = append(cfg.Ignore, "**/*.yaml")
	// An extractor is only asked about a repository if it is enabled, and the default
	// config enumerates the built-ins by name.
	cfg.Extractors = append(cfg.Extractors, "recorder")
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingDetector{}
	eng.RegisterExtractor(rec)

	if _, err := eng.GenerateSnapshot(context.Background(), dir, false); err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	if !rec.usedList {
		t.Fatalf("engine did not call DetectFiles")
	}
	if rec.usedWalk {
		t.Fatalf("engine fell back to Detect despite FileListDetector being implemented")
	}
	has := func(want string) bool {
		for _, n := range rec.gotNames {
			if n == want {
				return true
			}
		}
		return false
	}
	if !has("app/pubspec.yaml") {
		t.Errorf("ignored file absent from detection names: %v", rec.gotNames)
	}
	if !has("app/main.go") {
		t.Errorf("indexed file absent from detection names: %v", rec.gotNames)
	}
	if has("node_modules/dep/index.ts") {
		t.Errorf("pruned directory leaked into detection names: %v", rec.gotNames)
	}
}

// CurrentMeta answers the same question as the snapshot and must answer it the same
// way. It does not merely produce a nicer log line: its extractor list is compared
// against the recorded snapshot's by diff.CompareMeta, WarnExtractorSet is in
// check.BlockingKinds, and so a disagreement makes `enola check` report "not
// comparable" on every run of exactly the repositories this change was made for.
func TestCurrentMeta_AgreesWithTheSnapshotOnADeepRepo(t *testing.T) {
	dir := t.TempDir()
	writeDeep(t, dir, "src/main/native/engine/core/widget.cpp",
		"namespace core { int area() { return 7; } }\n")

	cfg := config.Default()
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(cppextractor.New())

	snap, err := eng.GenerateSnapshot(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	meta := eng.CurrentMeta(dir)
	if meta == nil {
		t.Fatal("CurrentMeta returned nil")
	}
	if len(meta.Extractors) != len(snap.Meta.Extractors) {
		t.Fatalf("CurrentMeta extractors %v != snapshot extractors %v", meta.Extractors, snap.Meta.Extractors)
	}
	for i := range meta.Extractors {
		if meta.Extractors[i] != snap.Meta.Extractors[i] {
			t.Fatalf("CurrentMeta extractors %v != snapshot extractors %v", meta.Extractors, snap.Meta.Extractors)
		}
	}
}
