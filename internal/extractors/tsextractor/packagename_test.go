package tsextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// extractRepoAllFiles runs the extractor with the FULL repo file list (as the
// engine's walkRepo produces), not just the TypeScript files — package.json is
// read off-glob and must be present for package_name to be emitted.
func extractRepoAllFiles(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	rel := make([]string, 0, len(files))
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, name)
	}
	sort.Strings(rel)
	got, err := New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return got
}

func moduleProp(ff []facts.Fact, dir, prop string) any {
	for _, f := range ff {
		if f.Kind == facts.KindModule && f.Name == dir {
			return f.Props[prop]
		}
	}
	return nil
}

// A module fact carries the name of the npm package its directory belongs to.
// The cross-repo import linker reads it to recognize the repo's own @scope.
func TestExtract_ModulePackageName(t *testing.T) {
	ff := extractRepoAllFiles(t, map[string]string{
		"package.json": `{"name": "@acme/sdk", "version": "1.0.0"}`,
		"src/index.ts": "export const x = 1;\n",
	})
	if got := moduleProp(ff, "src", "package_name"); got != "@acme/sdk" {
		t.Errorf("package_name = %v, want @acme/sdk", got)
	}
}

// In a monorepo each directory resolves to its NEAREST package.json, the same
// rule npm itself applies — not the repo root's.
func TestExtract_ModulePackageNameNearestAncestor(t *testing.T) {
	ff := extractRepoAllFiles(t, map[string]string{
		"package.json":              `{"name": "@acme/root"}`,
		"packages/api/package.json": `{"name": "@acme/api"}`,
		"packages/api/src/index.ts": "export const a = 1;\n",
		"tools/build.ts":            "export const b = 2;\n",
	})
	if got := moduleProp(ff, "packages/api/src", "package_name"); got != "@acme/api" {
		t.Errorf("packages/api/src package_name = %v, want @acme/api", got)
	}
	// No package.json under tools/ — falls back to the root package.
	if got := moduleProp(ff, "tools", "package_name"); got != "@acme/root" {
		t.Errorf("tools package_name = %v, want @acme/root", got)
	}
}

// TestExtract_ModulePackageName_WhenJSONIsIgnored is the regression guard for a bug that
// was invisible from inside this package for as long as it existed.
//
// package.json is read DIRECTLY FROM DISK, not from the engine's file list, because the
// list is ignore-glob-filtered and a config may legitimately drop JSON as data noise —
// the bundled mcp-arch.yaml ignores "**/*.json". While this function read from the list,
// such a config produced no package_name on any module, so the cross-repo linker's
// own-@scope guard could not fire and a repo importing a sibling package it publishes
// itself was reported as depending on another repo.
//
// Nothing caught it. Every other test here (and the golden fixtures, which build their
// engine from config.Default() and so have no such glob) hands the extractor a file list
// that already contains package.json — which is precisely the input that cannot
// distinguish reading it from the list from reading it from disk.
//
// So this test passes ONLY the TypeScript files, exactly as a repo snapshotted under a
// JSON-ignoring config would.
func TestExtract_ModulePackageName_WhenJSONIsIgnored(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", `{"name": "@acme/sdk"}`)
	write("src/index.ts", "export const a = 1;\n")
	// A dependency's package.json must never be mistaken for one the repo publishes.
	write("node_modules/left-pad/package.json", `{"name": "left-pad"}`)
	// Nor a fixture repo's: those are whole miniature codebases.
	write("testdata/fixture/package.json", `{"name": "@other/fixture"}`)
	write("testdata/fixture/src/index.ts", "export const b = 2;\n")

	// The engine's list under a "**/*.json"-ignoring config: TypeScript only.
	got, err := New().Extract(context.Background(), dir, []string{"src/index.ts"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if p := moduleProp(got, "src", "package_name"); p != "@acme/sdk" {
		t.Errorf("package_name = %v, want @acme/sdk — package.json must be read from disk, "+
			"not from the ignore-glob-filtered file list", p)
	}
	for _, f := range got {
		if f.Kind == facts.KindModule && f.Props["package_name"] == "left-pad" {
			t.Error("a node_modules package.json was read as one the repo publishes")
		}
		if f.Kind == facts.KindModule && f.Props["package_name"] == "@other/fixture" {
			t.Error("a testdata fixture's package.json was attributed to the host repo")
		}
	}
}

// A module is published when its nearest package.json describes a library: not
// private, and stating what it hands consumers or what they must bring. An
// application's package.json usually omits "private" too, so that alone is not
// enough.
func TestExtract_ModulePublished(t *testing.T) {
	ff := extractRepoAllFiles(t, map[string]string{
		"package.json":                 `{"name": "acme-monorepo", "private": true}`,
		"packages/sdk/package.json":    `{"name": "@acme/sdk", "peerDependencies": {"rxjs": "^7"}}`,
		"packages/sdk/src/index.ts":    "export const a = 1;\n",
		"packages/hidden/package.json": `{"name": "@acme/hidden", "private": true, "exports": "./index.js"}`,
		"packages/hidden/src/index.ts": "export const h = 1;\n",
		"apps/web/package.json":        `{"name": "web", "dependencies": {"react": "^18"}}`,
		"apps/web/src/main.ts":         "export const w = 1;\n",
		"tools/build.ts":               "export const b = 2;\n",
	})
	for dir, want := range map[string]bool{
		"packages/sdk/src": true, "packages/hidden/src": false, "apps/web/src": false, "tools": false,
	} {
		if got := moduleProp(ff, dir, facts.PropPublished) == true; got != want {
			t.Errorf("%s: published = %v, want %v", dir, got, want)
		}
	}
}
