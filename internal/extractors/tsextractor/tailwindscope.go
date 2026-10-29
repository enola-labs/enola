package tsextractor

import (
	"encoding/json"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
)

// A token name two themes declare is two tokens: an app's `primary` is not the
// docs site's. Each theme source belongs to a project, the nearest directory above
// it with a tailwind.config.* or a package.json, and a project sees its own themes
// first, then those it imports, nearest first: a config spreading another by its
// workspace package name (`import base from "docs-tailwind"`), a preset
// (`presets: [require("@medusajs/ui-preset")]`), a stylesheet's
// `@import 'config/theme.css'`. A package a theme styles from outside it (v4's
// `@source '../../packages/ui/src'`, a v3 content glob `"../../packages/ui/**"`)
// sees that theme too. Two themes a stylesheet @imports that both declare a name
// are told apart by order, as Tailwind does: the later import wins.
var (
	tailwindJSSpecRe      = regexp.MustCompile(`(?m)(?:\bfrom\s*|\brequire\(\s*|\bimport\(\s*|^\s*import\s+)["']([^"'\n]+)["']`)
	tailwindCSSSpecRe     = regexp.MustCompile(`@(?:import|config|plugin|reference)\s+(?:url\()?["']([^"'\n]+)["']`)
	tailwindCSSSourceRe   = regexp.MustCompile(`@source\s+["']([^"'\n]+)["']`)
	tailwindContentGlobRe = regexp.MustCompile(`["'](\.\.?/[^"'\n]*\*[^"'\n]*)["']`)
)

type tailwindScope struct {
	boundaries map[string]bool   // directories holding a tailwind.config.* or a package.json
	packages   map[string]string // workspace package name -> its directory
	imports    map[string]map[string]bool
	order      map[string]map[string]importPos // where a stylesheet @imports a project
	styledBy   map[string]map[string]bool      // a package -> the projects whose themes style it
	themes     map[string]bool                 // projects whose themes declare a token
}

// newTailwindScope reads which projects import which from the theme sources; the
// stylesheets the token users are read from add theirs with addImports.
func newTailwindScope(repoPath string, files []string, tokens []tailwindToken) *tailwindScope {
	s := &tailwindScope{boundaries: map[string]bool{}, packages: map[string]string{}, imports: map[string]map[string]bool{}, order: map[string]map[string]importPos{}, styledBy: map[string]map[string]bool{}}
	for _, f := range files {
		f = filepath.ToSlash(f)
		switch {
		case tailwindConfigRe.MatchString(f):
			s.boundaries[factpath.Dir(f)] = true
		case path.Base(f) == "package.json":
			dir := factpath.Dir(f)
			s.boundaries[dir] = true
			if raw, err := os.ReadFile(filepath.Join(repoPath, f)); err == nil { //factpath:host
				var pkg struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(raw, &pkg) == nil && pkg.Name != "" {
					s.packages[pkg.Name] = dir
				}
			}
		}
	}
	// Every config is read, a preset-only one too: what it lists is its theme.
	sources := map[string]bool{}
	for _, f := range files {
		if f = filepath.ToSlash(f); tailwindConfigRe.MatchString(f) {
			sources[f] = true
		}
	}
	s.themes = map[string]bool{}
	for _, t := range tokens {
		if t.source != "" {
			sources[t.source] = true
			s.themes[s.project(t.source)] = true
		}
	}
	for _, f := range slices.Sorted(maps.Keys(sources)) {
		if src, err := os.ReadFile(filepath.Join(repoPath, f)); err == nil { //factpath:host
			s.addImports(f, string(src))
		}
	}
	return s
}

func (s *tailwindScope) addImports(file, src string) {
	re, styled := tailwindJSSpecRe, tailwindContentGlobRe
	if isStylesheet(file) {
		re, styled = tailwindCSSSpecRe, tailwindCSSSourceRe
	} else if !tailwindConfigRe.MatchString(file) {
		styled = nil
	}
	from := s.project(file)
	for i, m := range re.FindAllStringSubmatch(src, -1) {
		if to, ok := s.target(file, m[1]); ok && from != to {
			link(s.imports, from, to)
			pos := importPos{file: file, lo: i, hi: i}
			if re != tailwindCSSSpecRe || !strings.HasPrefix(m[0], "@import") {
				pos = importPos{}
			}
			s.place(from, to, pos)
		}
	}
	if styled == nil {
		return
	}
	for _, m := range styled.FindAllStringSubmatch(src, -1) {
		glob := m[1]
		if i := strings.IndexAny(glob, "*{["); i >= 0 {
			glob = glob[:i]
		}
		if !strings.HasPrefix(glob, ".") {
			continue
		}
		if to, ok := s.target(file, strings.TrimSuffix(glob, "/")+"/x"); ok {
			link(s.styledBy, to, from)
		}
	}
}

func link(edges map[string]map[string]bool, from, to string) {
	if from == to {
		return
	}
	if edges[from] == nil {
		edges[from] = map[string]bool{}
	}
	edges[from][to] = true
}

// importPos is the span of a project's @imports in the one stylesheet that orders
// it; a project imported from two files, or by a config, has no order (file "").
type importPos struct {
	file   string
	lo, hi int
}

func (s *tailwindScope) place(from, to string, pos importPos) {
	if s.order[from] == nil {
		s.order[from] = map[string]importPos{}
	}
	old, ok := s.order[from][to]
	switch {
	case !ok:
		s.order[from][to] = pos
	case old.file == "" || old.file != pos.file:
		s.order[from][to] = importPos{}
	default:
		s.order[from][to] = importPos{file: old.file, lo: min(old.lo, pos.lo), hi: max(old.hi, pos.hi)}
	}
}

// themed reports whether a project reaches any theme: one that does names only the
// tokens it reaches, a class matching none being Tailwind's own (`text-sm`).
func (s *tailwindScope) themed(reach map[string]reached) bool {
	for p := range reach {
		if s.themes[p] {
			return true
		}
	}
	return false
}

// target is the project a specifier names: a relative path's, or a workspace
// package's by its name or a subpath of it.
func (s *tailwindScope) target(file, spec string) (string, bool) {
	if strings.HasPrefix(spec, ".") {
		p := factpath.Clean(path.Join(factpath.Dir(file), spec))
		if strings.HasPrefix(p, "..") {
			return "", false
		}
		if s.boundaries[p] {
			return p, true
		}
		return s.project(p), true
	}
	for name := spec; name != "" && name != "."; name = path.Dir(name) {
		if dir, ok := s.packages[name]; ok {
			return dir, true
		}
		if !strings.Contains(name, "/") {
			break
		}
	}
	return "", false
}

// project is the nearest boundary directory above a file, or the repository root.
func (s *tailwindScope) project(file string) string {
	for dir := factpath.Dir(file); ; dir = factpath.Dir(dir) {
		if s.boundaries[dir] || dir == "." || dir == "" || dir == "/" {
			if dir == "" || dir == "/" {
				return "."
			}
			return dir
		}
	}
}

// reach returns the projects a project sees, each with its distance: the projects
// whose themes style it, and from there and from itself what is imported. Being
// styled is a first step only: a package imported is not styled by its styler's
// themes on the importer's behalf.
func (s *tailwindScope) reach(from string) map[string]reached {
	out := map[string]reached{from: {}}
	queue := []string{from}
	for _, p := range slices.Sorted(maps.Keys(s.styledBy[from])) {
		if _, ok := out[p]; !ok {
			out[p] = reached{dist: 1, path: []reachStep{{to: p}}}
			queue = append(queue, p)
		}
	}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, q := range slices.Sorted(maps.Keys(s.imports[p])) {
			if _, ok := out[q]; !ok {
				r := out[p]
				out[q] = reached{dist: r.dist + 1, path: append(slices.Clip(r.path), reachStep{to: q, pos: s.order[p][q]})}
				queue = append(queue, q)
			}
		}
	}
	return out
}

// reached is how a project is reached: how far, and through which imports.
type reached struct {
	dist int
	path []reachStep
}

type reachStep struct {
	to  string
	pos importPos
}

// later reports whether a was imported after b, and whether that is known: where
// their paths part, one stylesheet must @import both, a's span wholly after b's.
func later(a, b []reachStep) (after, known bool) {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i].to == b[i].to {
			continue
		}
		pa, pb := a[i].pos, b[i].pos
		if pa.file == "" || pa.file != pb.file {
			return false, false
		}
		switch {
		case pa.lo > pb.hi:
			return true, true
		case pb.lo > pa.hi:
			return false, true
		}
		return false, false
	}
	return false, false
}

// tokenDecl is one declaration of a token name: its node and the projects whose
// themes read it.
type tokenDecl struct {
	node   string
	owners []string
}

// resolveDecls picks the declarations a project sees nearest, and of those the one
// imported last when that is known. A project that reaches no theme at all (code no
// config's content or @source names) keeps every declaration of the name, as before
// themes were told apart.
func resolveDecls(ds []*tokenDecl, reach map[string]reached, themed bool) []string {
	best := -1
	var out []string
	var paths [][]reachStep // the nearest owner's path per declaration in out; nil if two tie
	for _, d := range ds {
		near, n := -1, 0
		var path []reachStep
		for _, o := range d.owners {
			r, ok := reach[o]
			switch {
			case !ok:
			case near < 0 || r.dist < near:
				near, n, path = r.dist, 1, r.path
			case r.dist == near:
				n++
			}
		}
		if n > 1 {
			path = nil
		}
		switch {
		case near < 0:
		case best < 0 || near < best:
			best, out, paths = near, []string{d.node}, [][]reachStep{path}
		case near == best:
			out, paths = append(out, d.node), append(paths, path)
		}
	}
	if best < 0 && !themed {
		for _, d := range ds {
			out = append(out, d.node)
		}
	}
	if len(paths) > 1 {
		if w := lastImported(paths); w >= 0 {
			return out[w : w+1]
		}
	}
	return out
}

// lastImported is the path imported after every other, or -1 if none is known to be.
func lastImported(paths [][]reachStep) int {
	w := 0
	for i := 1; i < len(paths); i++ {
		if after, _ := later(paths[i], paths[w]); after {
			w = i
		}
	}
	for i, p := range paths {
		if i == w {
			continue
		}
		if after, known := later(paths[w], p); !known || !after || paths[w] == nil || p == nil {
			return -1
		}
	}
	return w
}
