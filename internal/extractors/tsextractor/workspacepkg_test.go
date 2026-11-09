package tsextractor

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func memberDirs(roots []workspaceRoot) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, r := range roots {
		m := map[string]string{}
		for name, p := range r.pkgs {
			m[name] = p.dir
		}
		out[r.dir] = m
	}
	return out
}

func TestCollectWorkspacesNPM(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"package.json":                  `{"name": "acme", "private": true, "workspaces": ["packages/*"]}`,
		"packages/core/package.json":    `{"name": "@acme/core", "exports": {".": "./src/index.ts"}}`,
		"packages/feature/package.json": `{"name": "@acme/feature"}`,
		// Not a member: a fixture outside the globs must not capture its name.
		"fixtures/react/package.json": `{"name": "react"}`,
		// Not a member: node_modules is never walked.
		"node_modules/@acme/core/package.json": `{"name": "@acme/core"}`,
	})
	got := memberDirs(collectWorkspaces(root))
	want := map[string]string{"@acme/core": "packages/core", "@acme/feature": "packages/feature"}
	if len(got) != 1 || len(got[""]) != len(want) {
		t.Fatalf("roots = %v, want one root with %v", got, want)
	}
	for name, dir := range want {
		if got[""][name] != dir {
			t.Errorf("%s -> %q, want %q", name, got[""][name], dir)
		}
	}
}

func TestCollectWorkspacesYarnObjectAndPnpm(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"yarn/package.json":              `{"name": "y", "workspaces": {"packages": ["libs/**"], "nohoist": ["**/x"]}}`,
		"yarn/libs/a/package.json":       `{"name": "@y/a"}`,
		"yarn/libs/group/b/package.json": `{"name": "@y/b"}`,
		"pnpm/package.json":              `{"name": "p"}`,
		"pnpm/pnpm-workspace.yaml":       "packages:\n  - 'apps/*'\n  - '!apps/skip'\n",
		"pnpm/apps/web/package.json":     `{"name": "@p/web"}`,
		"pnpm/apps/skip/package.json":    `{"name": "@p/skip"}`,
	})
	got := memberDirs(collectWorkspaces(root))
	if got["yarn"]["@y/a"] != "yarn/libs/a" || got["yarn"]["@y/b"] != "yarn/libs/group/b" {
		t.Errorf("yarn members = %v", got["yarn"])
	}
	if got["pnpm"]["@p/web"] != "pnpm/apps/web" {
		t.Errorf("pnpm members = %v", got["pnpm"])
	}
	if _, ok := got["pnpm"]["@p/skip"]; ok {
		t.Errorf("negated glob still a member: %v", got["pnpm"])
	}
}

func TestCollectWorkspacesNone(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"package.json":     `{"name": "solo", "dependencies": {"react": "*"}}`,
		"sub/package.json": `{"name": "@solo/sub"}`,
	})
	if roots := collectWorkspaces(root); len(roots) != 0 {
		t.Fatalf("no workspaces declared, got %v", memberDirs(roots))
	}
}

func TestWorkspaceMemberGlobs(t *testing.T) {
	cases := []struct {
		dir   string
		globs []string
		want  bool
	}{
		{"packages/core", []string{"packages/*"}, true},
		{"packages/core", []string{"./packages/*/"}, true},
		{"packages/a/b", []string{"packages/*"}, false},
		{"packages/a/b", []string{"packages/**"}, true},
		{"packages", []string{"packages/**"}, true},
		{"apps/web", []string{"apps/*", "!apps/web"}, false},
		{"apps/web", []string{"!apps/web", "apps/*"}, true},
		{"tools", []string{"tools"}, true},
	}
	for _, c := range cases {
		if got := workspaceMember(c.dir, c.globs); got != c.want {
			t.Errorf("workspaceMember(%q, %v) = %v, want %v", c.dir, c.globs, got, c.want)
		}
	}
}

func TestAffectsKeyWorkspaceManifests(t *testing.T) {
	e := &TSExtractor{}
	var hit []string
	for _, f := range []string{"package.json", "packages/core/package.json", "pnpm-workspace.yaml", "src/a.ts", "tsconfig.json"} {
		if e.AffectsKey(f) {
			hit = append(hit, f)
		}
	}
	sort.Strings(hit)
	want := []string{"package.json", "packages/core/package.json", "pnpm-workspace.yaml"}
	if len(hit) != len(want) {
		t.Fatalf("AffectsKey hits %v, want %v", hit, want)
	}
}

// workspaceAliases builds the alias map a file in dir resolves under, the way
// Extract does: tsconfig roots, then workspace packages, then the indexed files.
func workspaceAliases(t *testing.T, root, dir string, files map[string]string) map[string]tsAlias {
	t.Helper()
	writeTree(t, root, files)
	known := map[string]bool{}
	for f := range files {
		if isTypeScriptFile(f) {
			known[f] = true
		}
	}
	roots := withWorkspacePackages(root, collectTSAliasRoots(root))
	bindAliasFiles(roots, known)
	return aliasesForDir(roots, dir)
}

func TestResolveWorkspaceImports(t *testing.T) {
	base := map[string]string{
		"package.json":                   `{"name": "acme", "private": true, "workspaces": ["packages/*"]}`,
		"packages/feature/package.json":  `{"name": "@acme/feature"}`,
		"packages/feature/src/index.ts":  `export const v = 1`,
		"packages/core/src/index.ts":     `export const ok = 1`,
		"packages/core/src/extra.ts":     `export const extra = 2`,
		"packages/core/src/ui/button.ts": `export const b = 3`,
	}
	cases := []struct {
		name, manifest, spec, want, via string
	}{
		{"exports subpath map", `{"name": "@acme/core", "exports": {".": "./src/index.ts", "./extra": "./src/extra.ts"}}`,
			"@acme/core/extra", "packages/core/src/extra.ts", aliasViaWorkspace},
		{"exports root string", `{"name": "@acme/core", "exports": "./src/index.ts"}`,
			"@acme/core", "packages/core/src/index.ts", aliasViaWorkspace},
		{"exports conditions, dist mapped to src", `{"name": "@acme/core", "exports": {".": {"types": "./dist/index.d.ts", "import": "./dist/esm/index.js"}}}`,
			"@acme/core", "packages/core/src/index", aliasViaWorkspace},
		{"source condition preferred", `{"name": "@acme/core", "exports": {".": {"import": "./dist/index.js", "source": "./src/extra.ts"}}}`,
			"@acme/core", "packages/core/src/extra.ts", aliasViaWorkspace},
		{"exports pattern", `{"name": "@acme/core", "exports": {"./*": "./src/*.ts", "./ui/*": {"import": "./dist/ui/*.js"}}}`,
			"@acme/core/ui/button", "packages/core/src/ui/button", aliasViaWorkspace},
		{"main without exports", `{"name": "@acme/core", "main": "./dist/index.js"}`,
			"@acme/core", "packages/core/src/index", aliasViaWorkspace},
		{"no entry at all: conventional src/index", `{"name": "@acme/core"}`,
			"@acme/core", "packages/core/src/index", aliasViaWorkspace},
		{"deep import without exports", `{"name": "@acme/core"}`,
			"@acme/core/src/extra", "packages/core/src/extra", aliasViaWorkspace},
		{"unexported subpath: package directory", `{"name": "@acme/core", "exports": {".": "./src/index.ts"}}`,
			"@acme/core/extra", "packages/core", aliasViaWorkspaceDir},
		{"build output nothing maps: package directory", `{"name": "@acme/core", "exports": {".": "./generated/bundle.js"}}`,
			"@acme/core", "packages/core", aliasViaWorkspaceDir},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{"packages/core/package.json": c.manifest}
			for k, v := range base {
				files[k] = v
			}
			aliases := workspaceAliases(t, t.TempDir(), "packages/feature/src", files)
			got, external, a := resolveImportPathVia(c.spec, "packages/feature/src", aliases)
			if external || got != c.want || a.via != c.via {
				t.Errorf("%s -> (%q, external=%v, via=%q), want (%q, internal, %q)", c.spec, got, external, a.via, c.want, c.via)
			}
		})
	}
}

func TestResolveWorkspaceImportsTSConfigPathsWin(t *testing.T) {
	aliases := workspaceAliases(t, t.TempDir(), "packages/feature/src", map[string]string{
		"package.json":               `{"name": "acme", "workspaces": ["packages/*"]}`,
		"tsconfig.json":              `{"compilerOptions": {"paths": {"@acme/core": ["./packages/core/src/extra.ts"]}}}`,
		"packages/core/package.json": `{"name": "@acme/core", "exports": "./src/index.ts"}`,
		"packages/core/src/index.ts": `export const ok = 1`,
		"packages/core/src/extra.ts": `export const extra = 2`,
	})
	got, _, a := resolveImportPathVia("@acme/core", "packages/feature/src", aliases)
	if got != "packages/core/src/extra.ts" || a.via != aliasViaTSConfig {
		t.Errorf("tsconfig paths must win: got %q via %q", got, a.via)
	}
}

func TestResolveWorkspaceImportsLeaveOthersExternal(t *testing.T) {
	aliases := workspaceAliases(t, t.TempDir(), "packages/feature/src", map[string]string{
		"package.json":                `{"name": "acme", "workspaces": ["packages/*"]}`,
		"packages/core/package.json":  `{"name": "@acme/core"}`,
		"packages/core/index.ts":      `export const ok = 1`,
		"fixtures/react/package.json": `{"name": "react"}`,
	})
	for _, spec := range []string{"react", "@acme/core-utils", "@acme", "@tanstack/react-query"} {
		if got, external, _ := resolveImportPathVia(spec, "packages/feature/src", aliases); !external || got != spec {
			t.Errorf("%s -> (%q, external=%v), want itself, external", spec, got, external)
		}
	}
	if got, _, _ := resolveImportPathVia("@acme/core", "packages/feature/src", aliases); got != "packages/core/index" {
		t.Errorf("@acme/core -> %q, want packages/core/index", got)
	}
}

func TestResolveWorkspaceImportsTSConfigOutDir(t *testing.T) {
	aliases := workspaceAliases(t, t.TempDir(), "packages/feature/src", map[string]string{
		"package.json":                `{"name": "acme", "workspaces": ["packages/*"]}`,
		"packages/core/package.json":  `{"name": "@acme/core", "main": "./out/main.js"}`,
		"packages/core/tsconfig.json": `{"compilerOptions": {"outDir": "./out", "rootDir": "./code"}}`,
		"packages/core/code/main.ts":  `export const ok = 1`,
	})
	if got, _, a := resolveImportPathVia("@acme/core", "packages/feature/src", aliases); got != "packages/core/code/main" || a.via != aliasViaWorkspace {
		t.Errorf("@acme/core -> %q via %q, want packages/core/code/main via the package tsconfig", got, a.via)
	}
}

func TestSplitPackageSpecifier(t *testing.T) {
	cases := map[string][2]string{
		"lodash":         {"lodash", "."},
		"lodash/fp":      {"lodash", "./fp"},
		"@acme/core":     {"@acme/core", "."},
		"@acme/core/a/b": {"@acme/core", "./a/b"},
	}
	for spec, want := range cases {
		name, sub, ok := splitPackageSpecifier(spec)
		if !ok || name != want[0] || sub != want[1] {
			t.Errorf("%s -> (%q, %q, %v), want %v", spec, name, sub, ok, want)
		}
	}
	for _, spec := range []string{"", "./x", "/abs", "@acme", "@acme/"} {
		if _, _, ok := splitPackageSpecifier(spec); ok {
			t.Errorf("%q must not split", spec)
		}
	}
}
