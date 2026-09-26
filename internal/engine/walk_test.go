package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/config"
)

// writeTree materializes a repo-relative file map under root, creating parents.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", p, err)
		}
	}
}

// walkFixture builds a repo under a temp dir and walks it with the given ignore
// patterns, returning walkRepo's three results.
func walkFixture(t *testing.T, ignore []string, files map[string]string) ([]string, []string, walkSkips) {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, files)

	cfg := config.Default()
	cfg.Ignore = ignore

	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srcFiles, testFiles, _, skips, err := eng.walkRepo(context.Background(), root)
	if err != nil {
		t.Fatalf("walkRepo: %v", err)
	}
	return srcFiles, testFiles, skips
}

// TestWalkRepo_IgnoredDirectoryIsCounted pins GAP-EN-01.
//
// An ignored DIRECTORY is pruned with filepath.SkipDir, so the walker never
// visits the files inside it: they are neither seen nor skipped, they are in no
// bucket at all. Before this was fixed, `files_skipped` counted only the ignored
// FILES the walker happened to reach — on fairwayhub/golf-ui it reported 9 for a
// repo with 67,270 ignored files, and named not one of them.
//
// A pruned directory is recorded once, as a directory. Walking node_modules/
// purely to count its 55,041 files would cost a stat apiece and say nothing an
// architecture graph wants to know.
func TestWalkRepo_IgnoredDirectoryIsCounted(t *testing.T) {
	files, testFiles, skips := walkFixture(t,
		[]string{"node_modules/**", "**/*.test.ts"},
		map[string]string{
			"node_modules/left-pad/index.js":  "module.exports = 1\n",
			"node_modules/left-pad/README.md": "# left-pad\n",
			"src/app.ts":                      "export const a = 1\n",
			"src/app.test.ts":                 "test('a', () => {})\n",
		})

	if skips.dirCount != 1 {
		t.Errorf("dirCount = %d, want 1 (node_modules/ was pruned and must be counted)", skips.dirCount)
	}
	if skips.count != 1 {
		t.Errorf("count = %d, want 1 (src/app.test.ts is the only ignored FILE the walker reaches)", skips.count)
	}
	if len(files) != 1 || filepath.ToSlash(files[0]) != "src/app.ts" {
		t.Errorf("files = %v, want exactly [src/app.ts]", files)
	}
	for _, f := range files {
		if got := filepath.ToSlash(f); len(got) >= 13 && got[:13] == "node_modules/" {
			t.Errorf("pruned subtree leaked into files: %q", got)
		}
	}
	if len(testFiles) != 1 || filepath.ToSlash(testFiles[0]) != "src/app.test.ts" {
		t.Errorf("testFiles = %v, want [src/app.test.ts] (a .test.ts is ignored for indexing but matches the default TestGlob, so it is collected for reference-only extraction)", testFiles)
	}
}

// TestWalkRepo_SkippedSampleNamesTheGlob pins GAP-EN-02.
//
// isIgnored knew which pattern matched and threw it away, so the receipt could
// say what was skipped but never why. Diagnosing a mis-scoped ignore rule meant
// re-deriving the match by hand.
func TestWalkRepo_SkippedSampleNamesTheGlob(t *testing.T) {
	_, _, skips := walkFixture(t,
		[]string{"node_modules/**", "**/*.test.ts"},
		map[string]string{
			"node_modules/left-pad/index.js": "module.exports = 1\n",
			"src/app.ts":                     "export const a = 1\n",
			"src/app.test.ts":                "test('a', () => {})\n",
		})

	want := []string{
		"node_modules/ (glob: node_modules/**)",
		"src/app.test.ts (glob: **/*.test.ts)",
	}
	for _, w := range want {
		if !slices.Contains(skips.sample, w) {
			t.Errorf("skipped_sample missing %q\ngot: %v", w, skips.sample)
		}
	}
}

// TestWalkRepo_OutputDirNotCountedAsSkippedDir pins the output-dir guard.
//
// .enola/ is enola's own output, not part of the source tree. Counting it would
// make dirs_skipped differ between a repo's first-ever snapshot (no .enola/ yet)
// and every snapshot after it — a phantom delta in diff_snapshot, for no signal.
func TestWalkRepo_OutputDirNotCountedAsSkippedDir(t *testing.T) {
	files, _, skips := walkFixture(t,
		[]string{".enola/**"},
		map[string]string{
			".enola/facts.jsonl":  "{}\n",
			".enola/receipt.json": "{}\n",
			"src/app.ts":          "export const a = 1\n",
		})

	if skips.dirCount != 0 {
		t.Errorf("dirCount = %d, want 0 (the output dir is enola's own artifact)", skips.dirCount)
	}
	if len(skips.sample) != 0 {
		t.Errorf("sample = %v, want empty", skips.sample)
	}
	if len(files) != 1 || filepath.ToSlash(files[0]) != "src/app.ts" {
		t.Errorf("files = %v, want exactly [src/app.ts]", files)
	}
}

// TestMatchGlob_ReturnsMatchedPattern guards the pattern-reporting split of
// matchAnyGlob. The "<prefix>/**/<fileglob>" branch must keep its `continue`:
// letting such a pattern fall through to the branches below matches it only when
// exactly one directory sits between prefix and file, which is an artifact of
// filepath.Match reading "**" as "*". That is the bug of fixed/28.
func TestMatchGlob_ReturnsMatchedPattern(t *testing.T) {
	rubyTestGlobs := []string{"**/spec/**/*_spec.rb", "**/test/**/*_test.rb"}

	tests := []struct {
		name     string
		relPath  string
		patterns []string
		want     string
		wantOK   bool
	}{
		{
			"directory glob names itself",
			"node_modules", []string{"vendor/**", "node_modules/**"},
			"node_modules/**", true,
		},
		{
			"file glob names itself",
			"src/app.test.ts", []string{"node_modules/**", "**/*.test.ts"},
			"**/*.test.ts", true,
		},
		{
			"dir-scoped glob names itself, not a near-miss",
			"spec/models/user_spec.rb", []string{"vendor/**", "**/spec/**/*_spec.rb"},
			"**/spec/**/*_spec.rb", true,
		},
		{
			"nested dir-scoped glob",
			"engines/core/spec/services/report_worker_spec.rb", rubyTestGlobs,
			"**/spec/**/*_spec.rb", true,
		},
		{
			// fixed/28: a production ActiveJob whose name ends in the token "test".
			"production job ending in _ab_test matches nothing",
			"app/jobs/reporting/cache_warmup_ab_test.rb", rubyTestGlobs,
			"", false,
		},
		{
			"first matching pattern wins",
			"vendor/spec/thing_spec.rb", []string{"vendor/**", "**/spec/**/*_spec.rb"},
			"vendor/**", true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := matchGlob(tt.relPath, tt.patterns)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("matchGlob(%q, %v) = (%q, %v), want (%q, %v)",
					tt.relPath, tt.patterns, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestGenerateSnapshot_StopsWhenCancelled: a cancelled request must end the snapshot,
// not just abandon waiting for it. The MCP server holds its snapshot lock for the whole
// call, so a walk that ran on after cancellation blocked every later snapshot in the
// session: pointed at a folder of ~880k files by mistake, for many minutes.
func TestGenerateSnapshot_StopsWhenCancelled(t *testing.T) {
	cfg := config.Default()
	cfg.Repo = filepath.Join("testdata", "repos", "go_sample")
	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := eng.GenerateSnapshot(ctx, cfg.Repo, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("GenerateSnapshot on a cancelled context = %v, want context.Canceled", err)
	}
}
