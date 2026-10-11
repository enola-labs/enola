package intent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/intent"
)

// TestOwnDeclaration_GovernsEveryPackage guards enola's own enola-intent.yaml against the
// one way a layer declaration fails silently.
//
// A package no declared path matches is not a violation — it is UNCLASSIFIED, and an
// unclassified package produces no findings at all. So moving internal/perf somewhere
// else, or adding internal/newthing, does not fail the gate: it quietly removes that code
// from the declaration's reach while every CI job stays green. Nothing in the output
// changes, which is precisely what makes it worth a test rather than a review habit.
//
// The rule enforced here is coverage, not correctness: whether a package sits in the
// RIGHT layer is a judgement, and the layers explainer already fails the build when the
// resulting edges run the wrong way. This only asserts that every package is somewhere.
func TestOwnDeclaration_GovernsEveryPackage(t *testing.T) {
	root := repoRoot(t)

	decl, err := intent.LoadRepoFile(root)
	if err != nil {
		t.Fatalf("loading enola's own %s: %v", intent.RepoFileName, err)
	}
	if decl == nil || len(decl.Layers) == 0 {
		t.Fatalf("enola declares no layers; this repository's CI gates on --fail-on=layers, which would enforce nothing")
	}

	var globs []string
	for _, l := range decl.Layers {
		globs = append(globs, l.Paths...)
	}

	for _, pkg := range goPackageDirs(t, root) {
		if !coveredBy(pkg, globs) {
			t.Errorf("package %q is in no declared layer — nothing governs it, and no finding would say so", pkg)
		}
	}

	// And the reverse: a declared path that no longer exists governs nothing, and reads
	// like protection that is not there.
	for _, g := range globs {
		dir := strings.TrimSuffix(g, "/**")
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir))); err != nil {
			t.Errorf("declared path %q does not exist — the declaration outlived the directory", g)
		}
	}
}

// coveredBy applies the same bounded glob dialect the layers explainer accepts: an exact
// module path, or a prefix/** subtree (which matches the prefix itself).
func coveredBy(pkg string, globs []string) bool {
	for _, g := range globs {
		if prefix, ok := strings.CutSuffix(g, "/**"); ok {
			if pkg == prefix || strings.HasPrefix(pkg, prefix+"/") {
				return true
			}
			continue
		}
		if pkg == g {
			return true
		}
	}
	return false
}

// goPackageDirs lists the repo-relative directories holding non-test Go source, which is
// what the graph turns into modules. Vendored trees, fixtures and the example repos are
// excluded for the same reason the snapshot ignores them: they are not this module.
func goPackageDirs(t *testing.T, root string) []string {
	t.Helper()
	seen := map[string]bool{}
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "examples", "docs", "node_modules", "vendor", ".enola":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || seen[rel] {
			return nil
		}
		seen[rel] = true
		dirs = append(dirs, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("found no Go packages — the walk is wrong, not the declaration")
	}
	return dirs
}

func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
