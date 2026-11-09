package tsextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
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
		"packages/core/types.d.ts":       `export type T = 1`,
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
		{"declaration-only module", `{"name": "@acme/core"}`,
			"@acme/core/types", "packages/core/types.d.ts", aliasViaWorkspace},
		{"build rooted above the package", `{"name": "@acme/core", "exports": {"./*": {"types": "./dist/types/feature/src/*.d.ts"}}}`,
			"@acme/core/index", "packages/feature/src/index", aliasViaWorkspace},
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

// extractWorkspaceRepo runs the extractor over a temporary workspace and returns
// the imports edges and the calls targets of one file.
func extractWorkspaceRepo(t *testing.T, files map[string]string, file string) (imports map[string]string, calls map[string]bool) {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, files)
	var rel []string
	for f := range files {
		rel = append(rel, f)
	}
	sort.Strings(rel)
	ff, err := New().Extract(context.Background(), root, rel)
	if err != nil {
		t.Fatal(err)
	}
	imports, calls = map[string]string{}, map[string]bool{}
	for _, f := range ff {
		if f.File != file {
			continue
		}
		for _, r := range f.Relations {
			switch r.Kind {
			case facts.RelImports:
				imports[r.Target] = f.PropString("resolved_via")
			case facts.RelCalls:
				calls[r.Target] = true
			}
		}
	}
	return imports, calls
}

var issueWorkspace = map[string]string{
	"package.json":                  `{"name": "acme", "private": true, "workspaces": ["packages/*"]}`,
	"tsconfig.json":                 `{"compilerOptions": {"module": "NodeNext", "moduleResolution": "NodeNext", "strict": true}}`,
	"packages/core/package.json":    `{"name": "@acme/core", "type": "module", "exports": {".": "./src/index.ts", "./extra": "./src/extra.ts"}}`,
	"packages/core/src/index.ts":    "export const ok = 1\n",
	"packages/core/src/extra.ts":    "export const extra = 2\n",
	"packages/feature/package.json": `{"name": "@acme/feature", "type": "module", "dependencies": {"@acme/core": "*"}}`,
	"packages/feature/src/index.ts": "import { ok } from \"@acme/core\"\nimport { extra } from \"@acme/core/extra\"\nexport const value = ok + extra\nexport function f() { return ok }\n",
}

func TestExtractWorkspaceImportsResolveToSource(t *testing.T) {
	imports, _ := extractWorkspaceRepo(t, issueWorkspace, "packages/feature/src/index.ts")
	for _, want := range []string{"packages/core/src/index.ts", "packages/core/src/extra.ts"} {
		if imports[want] != aliasViaWorkspace {
			t.Errorf("import %s via %q, want via %q; imports = %v", want, imports[want], aliasViaWorkspace, imports)
		}
	}
	for target := range imports {
		if strings.HasPrefix(target, "@acme/") {
			t.Errorf("workspace import left external: %s", target)
		}
	}
}

// The issue's related case: a wildcard paths entry that cannot spell a subpath.
// TypeScript falls back to the package, so the subpath resolves through exports.
func TestExtractTSConfigAliasMissingTargetFallsBackToWorkspace(t *testing.T) {
	files := map[string]string{}
	for k, v := range issueWorkspace {
		files[k] = v
	}
	files["tsconfig.json"] = `{"compilerOptions": {"paths": {"@acme/*": ["./packages/*/src/index.ts", "./libs/*/src/index.ts"]}}}`
	imports, _ := extractWorkspaceRepo(t, files, "packages/feature/src/index.ts")
	if imports["packages/core/src/index.ts"] != aliasViaTSConfig {
		t.Errorf("@acme/core must still resolve through tsconfig paths: %v", imports)
	}
	if imports["packages/core/src/extra.ts"] != aliasViaWorkspace {
		t.Errorf("@acme/core/extra must fall back to the workspace package: %v", imports)
	}
	if _, phantom := imports["packages/core/extra/src/index.ts"]; phantom {
		t.Errorf("edge to a file that does not exist: %v", imports)
	}
}

// A package reached only as a whole binds its names to nothing, not to whatever
// the parent directory declares.
func TestExtractWorkspacePackageDirBindsNothing(t *testing.T) {
	files := map[string]string{}
	for k, v := range issueWorkspace {
		files[k] = v
	}
	files["packages/core/package.json"] = `{"name": "@acme/core", "exports": {".": "./generated/bundle.js"}}`
	files["packages/index.ts"] = "export const ok = 99\n" // a same-named symbol one level up
	imports, calls := extractWorkspaceRepo(t, files, "packages/feature/src/index.ts")
	if imports["packages/core"] != aliasViaWorkspaceDir {
		t.Errorf("want the package directory via %q: %v", aliasViaWorkspaceDir, imports)
	}
	if calls["packages.ok"] {
		t.Errorf("call bound to the parent directory's symbol: %v", calls)
	}
}
