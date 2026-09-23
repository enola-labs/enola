package pathglob_test

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/pathglob"
)

// Compile exists to make the walk cheaper, not to decide anything differently.
// If it ever answers differently from Match, files leave or enter the graph
// silently: the walk drops what it should keep, or the receipt names a different
// pattern beside a skipped path than the one that actually matched.
//
// So the two are compared directly, over the pattern lists the bundled config really
// ships and over every path in this repository, plus the shapes a repository does not
// happen to contain. The assertion is on the PATTERN, not just the boolean, because
// first-match order is part of the contract.

// adversarialPaths are shapes the tree does not hold but the matcher must agree on:
// bare basenames, trailing slashes, dotfiles, deep nesting, and the directory names
// the .NET and Scala forms exist for.
var adversarialPaths = []string{
	"", ".", "/", "a", "a/", "/a", "a//b", "./a", "../a",
	"vendor", "vendor/", "vendor/x.go", "vendor/deep/deep/deep/x.go",
	"node_modules/pkg/index.js", "x/node_modules/pkg/index.js",
	"spec/user_spec.rb", "app/jobs/cache_warmup_ab_test.rb", "lib/foo_test.rb",
	"src/test/scala/x/Y.scala", "src/main/scala-3/zio/test/magnolia/M.scala",
	"MyApp.Tests/UnitTest1.cs", "MyApp/Program.cs", "x/MyApp.Tests/deep/UnitTest1.cs",
	".enola/facts.jsonl", "sub/.enola/facts.jsonl", ".github/workflows/ci.yml",
	"a/b/c/d/e/f/g/h/i/j/k.go", "x.min.js", "dist/bundle.js", "build/out.o",
	"Makefile", "go.sum", "a.b.c", "**", "**/x", "x/**",
	"x", "x/x", "x/y/x", "Makefile", "sub/Makefile",
}

func repoPaths(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner is not what this test is about
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("corpus is %d paths, too small to mean anything", len(out))
	}
	return out
}

func TestCompileGlobs_AgreesWithMatchGlob(t *testing.T) {
	cfg := config.Default()
	lists := map[string][]string{
		"ignore":    cfg.Ignore,
		"testGlobs": cfg.TestGlobs,
		"both":      append(append([]string{}, cfg.Ignore...), cfg.TestGlobs...),
		// Reversed, because first-match order is part of the answer and the default
		// lists happen to be ordered so that many conflicts never arise.
		"bothReversed": reversed(append(append([]string{}, cfg.Ignore...), cfg.TestGlobs...)),
	}

	paths := append(repoPaths(t), adversarialPaths...)
	for name, patterns := range lists {
		if len(patterns) == 0 {
			t.Fatalf("%s: no patterns, the comparison would be vacuous", name)
		}
		set := pathglob.Compile(patterns)
		if set.Len() != len(patterns) {
			t.Errorf("%s: compiled %d patterns, want %d", name, set.Len(), len(patterns))
		}
		for _, p := range paths {
			wantPat, wantOK := pathglob.Match(p, patterns)
			gotPat, gotOK := set.Match(p)
			if gotOK != wantOK || gotPat != wantPat {
				t.Errorf("%s: Match(%q) = (%q, %v), Match = (%q, %v)",
					name, p, gotPat, gotOK, wantPat, wantOK)
			}
			if got, want := set.MatchAny(p), pathglob.MatchAny(p, patterns); got != want {
				t.Errorf("%s: MatchAny(%q) = %v, MatchAny = %v", name, p, got, want)
			}
		}
	}
}

// Every supported form, one pattern at a time, so a disagreement names the form it is
// in rather than pointing at a list of 114.
func TestCompileGlobs_AgreesPerForm(t *testing.T) {
	forms := []string{
		"vendor/**",
		"**/build/**",
		"**/*.Tests/**",
		"**/*_test.go",
		"**/spec/**/*_spec.rb",
		"**/*.Tests/**/*.cs",
		"**/src/test/**/*.scala",
		"**/**/*.go",
		"**/",
		"**",
		"*.go",
		"a/b/c.go",
		"a/**/b/**/c.go",
		"**/x/**/y/**",
		"[",    // unparseable: must match nothing, in both
		"**/[", // unparseable behind a prefix
		"",     // empty pattern
	}
	paths := append(repoPaths(t), adversarialPaths...)
	for _, form := range forms {
		patterns := []string{form}
		set := pathglob.Compile(patterns)
		for _, p := range paths {
			wantPat, wantOK := pathglob.Match(p, patterns)
			gotPat, gotOK := set.Match(p)
			if gotOK != wantOK || gotPat != wantPat {
				t.Errorf("form %q: Match(%q) = (%q, %v), Match = (%q, %v)",
					form, p, gotPat, gotOK, wantPat, wantOK)
			}
		}
	}
}

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}

func BenchmarkGlobMatching(b *testing.B) {
	cfg := config.Default()
	patterns := append(append([]string{}, cfg.Ignore...), cfg.TestGlobs...)
	set := pathglob.Compile(patterns)
	paths := []string{
		"internal/extractors/tsextractor/ts.go",
		"node_modules/react/index.js",
		"src/test/scala/com/example/FooSpec.scala",
		"docs/ARCHITECTURE.md",
		strings.Repeat("a/", 12) + "deep.go",
	}

	b.Run("linear", func(b *testing.B) {
		for b.Loop() {
			for _, p := range paths {
				pathglob.Match(p, patterns)
			}
		}
	})
	b.Run("compiled", func(b *testing.B) {
		for b.Loop() {
			for _, p := range paths {
				set.Match(p)
			}
		}
	})
}
