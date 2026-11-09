package tsextractor

// Workspace packages: the packages an npm, Yarn, pnpm or Bun workspace links into
// node_modules, so that one member imports another by its package name
// (`import { x } from "@acme/core"`). The package manager resolves that name through
// a symlink to the member's directory and then through its package.json exports, and
// TypeScript follows the same path. Without this map such an import read as an
// external dependency, and the repository's own cross-package edges, the ones its
// constraints are written about, were not in the graph at all.
//
// Only declared members count. A package.json that merely holds a name (a fixture,
// an example, a template) is not linked by the package manager, and treating it as
// one would let a fixture called `react` capture every real `react` import.

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/workspace"
)

// workspacePkg is one member of a workspace, as its package.json declares it.
type workspacePkg struct {
	name string
	dir  string // repository-relative, slash-separated; "" is the repository root
	// exports is the raw `exports` field: a string, a conditions object, or a
	// subpath map. Kept raw because its shape is only known once it is read.
	exports json.RawMessage
	// main, module, types and source are the legacy entry fields, tried when
	// exports is absent.
	main, module, types, source string
}

// workspaceRoot is a directory that declares workspace members, and the members
// it declares, by package name.
type workspaceRoot struct {
	dir  string
	pkgs map[string]*workspacePkg
}

// workspaceManifest is the part of a package.json this file reads.
type workspaceManifest struct {
	Name       string          `json:"name"`
	Workspaces json.RawMessage `json:"workspaces"`
	Exports    json.RawMessage `json:"exports"`
	Main       string          `json:"main"`
	Module     string          `json:"module"`
	Types      string          `json:"types"`
	Typings    string          `json:"typings"`
	Source     string          `json:"source"`
}

// collectWorkspaces finds every workspace in the repository and its members. It
// reads package.json and pnpm-workspace.yaml from disk rather than from the file
// list, as collectPackageNames does: a configuration that ignores JSON as data
// noise must not hide the workspace from import resolution.
func collectWorkspaces(repoPath string) []workspaceRoot {
	manifests := map[string]workspaceManifest{}
	globsAt := map[string][]string{}
	_ = filepath.WalkDir(repoPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if p != repoPath && (strings.HasPrefix(name, ".") || tsSkipDirs[name] || name == "testdata" || workspace.IsLinkedWorktree(p)) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repoPath, filepath.Dir(p)) //factpath:host
		if err != nil {
			return nil
		}
		dir := factpath.Slash(rel)
		if dir == "." {
			dir = ""
		}
		switch d.Name() {
		case "package.json":
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			var m workspaceManifest
			if json.Unmarshal(data, &m) != nil {
				return nil
			}
			manifests[dir] = m
			if globs := workspaceGlobs(m.Workspaces); len(globs) > 0 {
				globsAt[dir] = append(globsAt[dir], globs...)
			}
		case "pnpm-workspace.yaml", "pnpm-workspace.yml":
			if globs := pnpmWorkspaceGlobs(p); len(globs) > 0 {
				globsAt[dir] = append(globsAt[dir], globs...)
			}
		}
		return nil
	})

	dirs := make([]string, 0, len(manifests))
	for dir := range manifests {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	var roots []workspaceRoot
	for rootDir, globs := range globsAt {
		r := workspaceRoot{dir: rootDir, pkgs: map[string]*workspacePkg{}}
		for _, dir := range dirs {
			m := manifests[dir]
			if m.Name == "" || dir == rootDir {
				continue
			}
			rel, ok := relUnder(rootDir, dir)
			if !ok || !workspaceMember(rel, globs) {
				continue
			}
			if _, dup := r.pkgs[m.Name]; dup {
				continue // two members of one name: the first by path, deterministically
			}
			types := m.Types
			if types == "" {
				types = m.Typings
			}
			r.pkgs[m.Name] = &workspacePkg{name: m.Name, dir: dir, exports: m.Exports, main: m.Main, module: m.Module, types: types, source: m.Source}
		}
		if len(r.pkgs) > 0 {
			roots = append(roots, r)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].dir < roots[j].dir })
	return roots
}

// relUnder returns dir relative to root when dir is inside it.
func relUnder(root, dir string) (string, bool) {
	if root == "" {
		return dir, true
	}
	if rest, ok := strings.CutPrefix(dir, root+"/"); ok {
		return rest, true
	}
	return "", false
}

// workspaceGlobs reads package.json `workspaces`: an array of globs (npm, Yarn,
// Bun), or Yarn's `{ "packages": [...] }` object.
func workspaceGlobs(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var obj struct {
		Packages []string `json:"packages"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Packages
	}
	return nil
}

// pnpmWorkspaceGlobs reads the `packages` list of a pnpm-workspace.yaml.
func pnpmWorkspaceGlobs(file string) []string {
	data, err := os.ReadFile(file) //factpath:host
	if err != nil {
		return nil
	}
	var w struct {
		Packages []string `yaml:"packages"`
	}
	if yaml.Unmarshal(data, &w) != nil {
		return nil
	}
	return w.Packages
}

// workspaceMember reports whether a directory, relative to the workspace root,
// is a member under the globs: matched by an inclusion and by no later `!`
// exclusion, the order the package managers apply them in.
func workspaceMember(dir string, globs []string) bool {
	member := false
	for _, g := range globs {
		g = strings.TrimSpace(g)
		neg := strings.HasPrefix(g, "!")
		g = strings.TrimPrefix(g, "!")
		g = strings.TrimSuffix(strings.TrimPrefix(g, "./"), "/")
		g = strings.TrimSuffix(g, "/package.json")
		if g == "" || !globSegmentsMatch(strings.Split(g, "/"), strings.Split(dir, "/")) {
			continue
		}
		member = !neg
	}
	return member
}

// globSegmentsMatch matches a path segment by segment: `*` and the other
// path.Match forms within a segment, `**` across any number of them.
func globSegmentsMatch(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if globSegmentsMatch(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
		return false
	}
	return globSegmentsMatch(pat[1:], segs[1:])
}

// isWorkspaceManifest reports whether a file declares or describes workspace
// members, so its contents belong in the extractor's cache key (AffectsKey).
func isWorkspaceManifest(relFile string) bool {
	switch path.Base(filepath.ToSlash(relFile)) {
	case "package.json", "pnpm-workspace.yaml", "pnpm-workspace.yml":
		return true
	}
	return false
}

// AffectsKey implements plugin.KeyDependent. package.json is owned by the manifest
// extractor, so without this an edit to a member's exports, or to the workspace
// globs, would not invalidate the import edges resolved through them.
func (e *TSExtractor) AffectsKey(relFile string) bool { return isWorkspaceManifest(relFile) }
