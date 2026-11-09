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
