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
	"sync"

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
	// root is the repository on disk, for reading the package's tsconfig.
	root string
	// outDir and rootDir are the package tsconfig's, read once (tsconfigDirs).
	tsconfigOnce    sync.Once
	outDir, rootDir string
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
			r.pkgs[m.Name] = &workspacePkg{name: m.Name, dir: dir, exports: m.Exports, main: m.Main, module: m.Module, types: types, source: m.Source, root: repoPath}
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

// How a workspace import resolved, recorded as resolved_via. A package entry is
// the module its exports or entry fields name, or the source a build output was
// compiled from. A package directory is the last resort: the package is known
// but none of its declared entries names an indexed module (its exports point at
// a build that is not checked in, and no source mapping finds the file), so the
// import lands on the package as a whole. The edge still belongs to the package,
// which is what a constraint between packages is about; it binds no symbol.
const (
	aliasViaWorkspace    = "workspace"
	aliasViaWorkspaceDir = "workspace-package-dir"
)

// workspaceAliasKey is where a workspace package sits in an alias map. The NUL
// prefix keeps it out of the exact and prefix passes, which match by key: a
// workspace package is tried only after every configured alias has had its turn.
func workspaceAliasKey(name string) string { return "\x00workspace\x00" + name }

// splitPackageSpecifier splits a bare specifier into the package name and the
// subpath exports are keyed by: `@acme/core/extra` is `@acme/core` and `./extra`,
// `lodash` is `lodash` and `.`.
func splitPackageSpecifier(spec string) (name, sub string, ok bool) {
	if spec == "" || strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") {
		return "", "", false
	}
	parts := strings.SplitN(spec, "/", 3)
	n := 1
	if strings.HasPrefix(spec, "@") {
		if len(parts) < 2 || parts[1] == "" {
			return "", "", false
		}
		n = 2
	}
	name = strings.Join(parts[:n], "/")
	rest := strings.TrimPrefix(spec[len(name):], "/")
	if rest == "" {
		return name, ".", true
	}
	return name, "./" + rest, true
}

// withWorkspacePackages adds every workspace's members to the alias maps of the
// files inside that workspace. A workspace whose directory has no alias root of
// its own gets one, seeded with the aliases its files already compiled under, so
// adding the packages takes nothing away. Workspaces are applied outermost first,
// so a nested workspace's member wins over an outer one of the same name.
func withWorkspacePackages(repoPath string, roots []tsAliasRoot) []tsAliasRoot {
	for _, w := range collectWorkspaces(repoPath) {
		found := false
		for _, r := range roots {
			if r.dir == w.dir {
				found = true
				break
			}
		}
		if !found {
			seeded := map[string]tsAlias{}
			for k, a := range aliasesForDir(roots, w.dir) {
				seeded[k] = a
			}
			roots = append(roots, tsAliasRoot{dir: w.dir, aliases: seeded})
		}
		for i := range roots {
			if _, under := relUnder(w.dir, roots[i].dir); !under && roots[i].dir != w.dir {
				continue
			}
			if roots[i].aliases == nil {
				roots[i].aliases = map[string]tsAlias{}
			}
			for name, p := range w.pkgs {
				roots[i].aliases[workspaceAliasKey(name)] = tsAlias{ws: p, via: aliasViaWorkspace}
			}
		}
	}
	return roots
}

// resolveWorkspaceImport resolves a bare specifier through the workspace package
// it names, when it names one. ok is false for a specifier that names no member.
func resolveWorkspaceImport(importPath string, aliases map[string]tsAlias) (string, tsAlias, bool) {
	name, sub, ok := splitPackageSpecifier(importPath)
	if !ok {
		return "", tsAlias{}, false
	}
	a, ok := aliases[workspaceAliasKey(name)]
	if !ok || a.ws == nil {
		return "", tsAlias{}, false
	}
	resolved, entry := a.ws.resolve(sub, a.known)
	if !entry {
		a.via = aliasViaWorkspaceDir
	}
	return factpath.Clean(resolved), a, true
}

// resolve maps a subpath of the package to the module it names. entry is false
// when nothing more precise than the package directory could be found.
//
// In order: a target the package declares (exports conditions, or the legacy
// entry fields) that is an indexed module; then the source such a target was
// built from; then the package directory. Without the indexed set (the test-ref
// pass) the first declared target is taken, as an alias takes its first target.
func (p *workspacePkg) resolve(sub string, known map[string]bool) (string, bool) {
	candidates := p.entryCandidates(sub)
	if known == nil {
		if len(candidates) > 0 {
			return candidates[0], true
		}
		return p.dir, false
	}
	for _, c := range candidates {
		if m, ok := indexedModule(c, known); ok {
			return m, true
		}
	}
	for _, c := range candidates {
		for _, s := range p.sourcesFor(c) {
			if m, ok := indexedModule(s, known); ok {
				return m, true
			}
		}
	}
	return p.dir, false
}

// indexedModule reports whether a candidate names an indexed module, and the
// spelling to keep: a declaration file stands for the module beside it.
func indexedModule(c string, known map[string]bool) (string, bool) {
	for _, ext := range []string{".d.ts", ".d.mts", ".d.cts"} {
		if stem, ok := strings.CutSuffix(c, ext); ok {
			c = stem
			break
		}
	}
	if moduleIndexed(c, known) {
		return c, true
	}
	return "", false
}

// joinPkg joins a target the manifest names (`./src/index.ts`) to the package
// directory, repository-relative.
func (p *workspacePkg) joinPkg(target string) string {
	return factpath.Clean(factpath.Join(p.dir, strings.TrimPrefix(target, "./")))
}

// entryCandidates lists the files the package declares for a subpath, most
// preferred first. A package with exports exposes nothing else: a subpath it
// does not export has no candidates, as Node refuses it.
func (p *workspacePkg) entryCandidates(sub string) []string {
	var targets []string
	if len(p.exports) > 0 && string(p.exports) != "null" {
		node, err := decodeOrderedJSON(p.exports)
		if err != nil {
			return nil
		}
		targets = exportTargets(node, sub)
	} else if sub == "." {
		for _, t := range []string{p.source, p.types, p.module, p.main} {
			if t != "" {
				targets = append(targets, t)
			}
		}
		targets = append(targets, "index", "src/index")
	} else {
		targets = []string{sub}
	}
	out := make([]string, 0, len(targets))
	seen := map[string]bool{}
	for _, t := range targets {
		if c := p.joinPkg(t); !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// outputDirs are the directories a package commonly builds into, and the module
// format subdirectories some builds add beneath them.
var (
	outputDirs    = map[string]bool{"dist": true, "build": true, "lib": true, "out": true, "esm": true, "cjs": true, "es": true, "types": true, "typings": true}
	outputFormats = map[string]bool{"esm": true, "cjs": true, "es": true, "mjs": true, "types": true, "src": true}
)

// sourcesFor lists the source files a build output in the package may have been
// compiled from: through the package tsconfig's outDir and rootDir when it
// declares them, then by the common layout of a build directory mirroring src/.
// Each is only a candidate; resolve keeps one only if it is an indexed module.
func (p *workspacePkg) sourcesFor(c string) []string {
	rel, ok := relUnder(p.dir, c)
	if !ok {
		return nil
	}
	for _, ext := range []string{".d.ts", ".d.mts", ".d.cts", ".js", ".mjs", ".cjs", ".jsx"} {
		if stem, ok := strings.CutSuffix(rel, ext); ok {
			rel = stem
			break
		}
	}
	var out []string
	add := func(s string) { out = append(out, p.joinPkg(s)) }
	if outDir, rootDir := p.tsconfigDirs(); outDir != "" {
		if rest, ok := strings.CutPrefix(rel, outDir+"/"); ok {
			if rootDir != "" {
				add(path.Join(rootDir, rest))
			} else {
				add(path.Join("src", rest))
				add(rest)
			}
		}
	}
	segs := strings.Split(rel, "/")
	if len(segs) > 1 && outputDirs[segs[0]] {
		segs = segs[1:]
		if len(segs) > 1 && outputFormats[segs[0]] {
			segs = segs[1:]
		}
		rest := strings.Join(segs, "/")
		add(path.Join("src", rest))
		add(rest)
	}
	return out
}

// tsconfigDirs reads compilerOptions.outDir and rootDir from the package's own
// tsconfig, relative to the package directory. Read once per package.
func (p *workspacePkg) tsconfigDirs() (outDir, rootDir string) {
	p.tsconfigOnce.Do(func() {
		for _, name := range []string{"tsconfig.build.json", "tsconfig.lib.json", "tsconfig.json"} {
			data, err := os.ReadFile(filepath.Join(p.root, filepath.FromSlash(p.dir), name)) //factpath:host
			if err != nil {
				continue
			}
			var cfg struct {
				CompilerOptions struct {
					OutDir  string `json:"outDir"`
					RootDir string `json:"rootDir"`
				} `json:"compilerOptions"`
			}
			if json.Unmarshal(stripJSONC(data), &cfg) != nil || cfg.CompilerOptions.OutDir == "" {
				continue
			}
			clean := func(s string) string {
				s = path.Clean(strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(s)), "./"))
				if s == "." {
					return ""
				}
				return s
			}
			p.outDir, p.rootDir = clean(cfg.CompilerOptions.OutDir), clean(cfg.CompilerOptions.RootDir)
			return
		}
	})
	return p.outDir, p.rootDir
}

// jsonNode is a JSON value with object keys kept in source order, which the
// exports field depends on: conditions are matched in the order they are written.
type jsonNode struct {
	str    *string
	arr    []jsonNode
	keys   []string
	vals   []jsonNode
	isObj  bool
	isNull bool
}

func decodeOrderedJSON(raw []byte) (jsonNode, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	return decodeNode(dec)
}

func decodeNode(dec *json.Decoder) (jsonNode, error) {
	tok, err := dec.Token()
	if err != nil {
		return jsonNode{}, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := jsonNode{isObj: true}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return jsonNode{}, err
				}
				key, _ := kt.(string)
				v, err := decodeNode(dec)
				if err != nil {
					return jsonNode{}, err
				}
				n.keys = append(n.keys, key)
				n.vals = append(n.vals, v)
			}
			_, err := dec.Token()
			return n, err
		case '[':
			var n jsonNode
			for dec.More() {
				v, err := decodeNode(dec)
				if err != nil {
					return jsonNode{}, err
				}
				n.arr = append(n.arr, v)
			}
			_, err := dec.Token()
			return n, err
		}
	case string:
		return jsonNode{str: &t}, nil
	case nil:
		return jsonNode{isNull: true}, nil
	}
	return jsonNode{}, nil // a number or a boolean names no file
}

// preferredConditions are tried before the rest, in this order: a condition that
// names the source outright, then the ones a TypeScript import resolves under.
var preferredConditions = []string{"source", "development", "types", "import", "module", "default", "require", "node"}

// exportTargets returns the targets an exports field gives a subpath, most
// preferred first, `*` already substituted.
func exportTargets(n jsonNode, sub string) []string {
	if !n.isObj || len(n.keys) == 0 || !strings.HasPrefix(n.keys[0], ".") {
		// A string, an array, or a conditions object: the package's "." alone.
		if sub != "." {
			return nil
		}
		return conditionLeaves(n, "")
	}
	for i, k := range n.keys {
		if k == sub {
			return conditionLeaves(n.vals[i], "")
		}
	}
	// A pattern: the key with the longest prefix before its `*` wins, as in Node.
	best, bestLen, star := -1, -1, ""
	for i, k := range n.keys {
		head, tail, ok := strings.Cut(k, "*")
		if !ok {
			if strings.HasSuffix(k, "/") && strings.HasPrefix(sub, k) && len(k) > bestLen {
				best, bestLen, star = i, len(k), "\x00folder\x00"+strings.TrimPrefix(sub, k)
			}
			continue
		}
		if len(sub) < len(head)+len(tail) || !strings.HasPrefix(sub, head) || !strings.HasSuffix(sub, tail) {
			continue
		}
		if len(head) > bestLen {
			best, bestLen, star = i, len(head), sub[len(head):len(sub)-len(tail)]
		}
	}
	if best < 0 {
		return nil
	}
	if folder, ok := strings.CutPrefix(star, "\x00folder\x00"); ok {
		var out []string
		for _, t := range conditionLeaves(n.vals[best], "") {
			out = append(out, t+folder)
		}
		return out
	}
	return conditionLeaves(n.vals[best], star)
}

// conditionLeaves flattens a target (string, fallback array, or conditions
// object) to the files it may name, preferred conditions first. A null target
// excludes the subpath and names nothing.
func conditionLeaves(n jsonNode, star string) []string {
	switch {
	case n.str != nil:
		return []string{strings.ReplaceAll(*n.str, "*", star)}
	case n.isNull:
		return nil
	case n.isObj:
		var out []string
		used := map[int]bool{}
		for _, c := range preferredConditions {
			for i, k := range n.keys {
				if k == c && !used[i] {
					used[i] = true
					out = append(out, conditionLeaves(n.vals[i], star)...)
				}
			}
		}
		for i := range n.keys {
			if !used[i] {
				out = append(out, conditionLeaves(n.vals[i], star)...)
			}
		}
		return out
	}
	var out []string
	for _, v := range n.arr {
		out = append(out, conditionLeaves(v, star)...)
	}
	return out
}
