package tsextractor

import (
	"path"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

var (
	barrelExportFromRe = regexp.MustCompile(`export\s*\{([^}]*)\}\s*from\s*['"]([^'"]+)['"]`)
	barrelExportRe     = regexp.MustCompile(`export\s*\{([^}]*)\}\s*;?\s*(?:\n|$)`)
	barrelDefaultImpRe = regexp.MustCompile(`import\s+([A-Za-z_$][\w$]*)\s+from\s*['"]([^'"]+)['"]`)
	barrelNamedImpRe   = regexp.MustCompile(`import\s*\{([^}]*)\}\s*from\s*['"]([^'"]+)['"]`)
	barrelStarRe       = regexp.MustCompile(`export\s*\*\s*from\s*['"]([^'"]+)['"]`)
	// barrelDeclRe reads a top-level declaration: group 1 is its export keyword,
	// group 2 its default keyword, group 3 its name.
	barrelDeclRe = regexp.MustCompile(`(?m)^(export\s+)?(?:declare\s+)?(default\s+)?(?:async\s+)?(?:abstract\s+)?(?:function\s*\*?|class|const\s+enum|enum|interface|type|const|let|var|namespace)\s+([A-Za-z_$][\w$]*)`)
)

// barrelExports maps "<folder>.<Name>" to the symbol a folder's index file
// re-exports under that name: `export { A } from './a'`, or a binding imported and
// then exported (`import V from './v.vue'; export { V }`). An import of the folder
// binds to "<folder>.<Name>", which no fact declares, so without this a barrel
// leaves every import through it dangling.
//
// A name the index passes on with `export * from './x'` binds to where x declares it,
// followed through nested stars (see starExports). A name any file of the folder
// declares stays the folder's, so only a name that would otherwise name nothing moves.
func barrelExports(tsFiles []string, read func(string) []byte, knownFiles map[string]bool) map[string]string {
	out := map[string]string{}
	stars := newStarExports(tsFiles, read, knownFiles)
	for _, file := range tsFiles {
		if strings.TrimSuffix(path.Base(file), path.Ext(file)) != "index" || facts.IsTestPath(file) {
			continue
		}
		src := string(read(file))
		if !strings.Contains(src, "export") {
			continue
		}
		folder := factpath.Dir(file)
		target := func(spec, name string) string {
			if !strings.HasPrefix(spec, ".") {
				return ""
			}
			idx, dir, ok := resolveModuleFile(factpath.Clean(path.Join(folder, spec)), knownFiles)
			if !ok {
				return ""
			}
			if name == "default" {
				return dir + "." + fileSymbolName(idx)
			}
			return dir + "." + name
		}
		for _, m := range barrelExportFromRe.FindAllStringSubmatch(src, -1) {
			for _, spec := range splitSpecifiers(m[1]) {
				if t := target(m[2], spec[0]); t != "" {
					out[folder+"."+spec[1]] = t
				}
			}
		}
		imported := map[string]string{}
		for _, m := range barrelDefaultImpRe.FindAllStringSubmatch(src, -1) {
			if t := target(m[2], "default"); t != "" {
				imported[m[1]] = t
			}
		}
		for _, m := range barrelNamedImpRe.FindAllStringSubmatch(src, -1) {
			for _, spec := range splitSpecifiers(m[1]) {
				if t := target(m[2], spec[0]); t != "" {
					imported[spec[1]] = t
				}
			}
		}
		for _, m := range barrelExportRe.FindAllStringSubmatch(src, -1) {
			for _, spec := range splitSpecifiers(m[1]) {
				if t := imported[spec[0]]; t != "" {
					out[folder+"."+spec[1]] = t
				}
			}
		}
		if !barrelStarRe.MatchString(src) {
			continue
		}
		for name, e := range stars.exportsOf(file) {
			key := folder + "." + name
			if !e.viaStar || e.target == key || out[key] != "" || stars.folderDeclares(folder, name) {
				continue
			}
			out[key] = e.target
		}
	}
	return out
}

// starExport is where a name a module exports is declared: the symbol it names and
// the file that declares it. viaStar marks a name the module passes on with
// `export *` rather than exporting it by name.
type starExport struct {
	target, file string
	viaStar      bool
}

// starExports reads what modules export, following `export * from`, for the index
// files a folder import goes through. It reads sources, not facts, so the test-ref
// pass, which has no symbols, binds the same names the production pass does.
type starExports struct {
	read    func(string) []byte
	known   map[string]bool
	byDir   map[string][]string
	memo    map[string]map[string]starExport
	decls   map[string]map[string]bool
	busy    map[string]bool
	cutting int // counts the cycles cut; a module read across one is not memoised
	steps   int // modules read; past maxStarSteps every result is memoised
}

// maxStarSteps bounds the rereading of modules inside a cycle of stars, whose
// results are not memoised because a cut cycle leaves them partial.
const maxStarSteps = 100000

func newStarExports(tsFiles []string, read func(string) []byte, known map[string]bool) *starExports {
	s := &starExports{read: read, known: known, byDir: map[string][]string{},
		memo: map[string]map[string]starExport{}, decls: map[string]map[string]bool{}, busy: map[string]bool{}}
	for _, f := range tsFiles {
		if !facts.IsTestPath(f) {
			s.byDir[factpath.Dir(f)] = append(s.byDir[factpath.Dir(f)], f)
		}
	}
	return s
}

// folderDeclares reports whether a file of folder declares name at its top level,
// which names the symbol "<folder>.<name>" that a reference to the folder already
// reaches.
func (s *starExports) folderDeclares(folder, name string) bool {
	d, ok := s.decls[folder]
	if !ok {
		d = map[string]bool{}
		for _, f := range s.byDir[folder] {
			for _, m := range barrelDeclRe.FindAllStringSubmatch(string(s.read(f)), -1) {
				d[m[3]] = true
			}
		}
		s.decls[folder] = d
	}
	return d[name]
}

// resolve returns the file a relative specifier written in from names.
func (s *starExports) resolve(from, spec string) string {
	if !strings.HasPrefix(spec, ".") {
		return ""
	}
	idx, _, ok := resolveModuleFile(factpath.Clean(path.Join(factpath.Dir(from), spec)), s.known)
	if !ok {
		return ""
	}
	return idx
}

// exportsOf maps each name file exports to where it is declared. A name the file
// exports itself, by declaration or by name, shadows the same name from a star; a
// name two star sources declare differently is exported by neither, as TypeScript
// leaves it out. A module already being read (an import cycle) adds nothing.
func (s *starExports) exportsOf(file string) map[string]starExport {
	if m, ok := s.memo[file]; ok {
		return m
	}
	if s.busy[file] {
		s.cutting++
		return nil
	}
	s.busy[file] = true
	s.steps++
	cutBefore := s.cutting
	src := string(s.read(file))
	dir := factpath.Dir(file)
	out := map[string]starExport{}
	for _, m := range barrelDeclRe.FindAllStringSubmatch(src, -1) {
		if m[1] != "" && m[2] == "" {
			out[m[3]] = starExport{target: dir + "." + m[3], file: file}
		}
	}
	named := func(source, name string) starExport {
		if name == "default" {
			return starExport{target: factpath.Dir(source) + "." + fileSymbolName(source), file: source}
		}
		if e, ok := s.exportsOf(source)[name]; ok {
			return starExport{target: e.target, file: e.file}
		}
		return starExport{target: factpath.Dir(source) + "." + name, file: source}
	}
	for _, m := range barrelExportFromRe.FindAllStringSubmatch(src, -1) {
		source := s.resolve(file, m[2])
		if source == "" {
			continue
		}
		for _, spec := range splitSpecifiers(m[1]) {
			out[spec[1]] = named(source, spec[0])
		}
	}
	fromStars := map[string]starExport{}
	ambiguous := map[string]bool{}
	for _, m := range barrelStarRe.FindAllStringSubmatch(src, -1) {
		source := s.resolve(file, m[1])
		if source == "" {
			continue
		}
		for name, e := range s.exportsOf(source) {
			if _, own := out[name]; own || name == "default" {
				continue
			}
			if prev, seen := fromStars[name]; seen && (prev.file != e.file || prev.target != e.target) {
				ambiguous[name] = true
			}
			fromStars[name] = starExport{target: e.target, file: e.file, viaStar: true}
		}
	}
	for name, e := range fromStars {
		if !ambiguous[name] {
			out[name] = e
		}
	}
	delete(s.busy, file)
	if s.cutting == cutBefore || s.steps > maxStarSteps {
		s.memo[file] = out
	}
	return out
}

// splitSpecifiers reads `A, B as C, type D` into {name, alias} pairs.
func splitSpecifiers(list string) [][2]string {
	var out [][2]string
	for _, part := range strings.Split(list, ",") {
		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(part), "type "))
		switch {
		case len(fields) == 1:
			out = append(out, [2]string{fields[0], fields[0]})
		case len(fields) == 3 && fields[1] == "as":
			out = append(out, [2]string{fields[0], fields[2]})
		}
	}
	return out
}

// resolveBarrelCalls rewrites a dangling call target to the symbol a barrel
// re-exports under that name, when that symbol exists.
func resolveBarrelCalls(all []facts.Fact, barrels map[string]string) {
	if len(barrels) == 0 {
		return
	}
	exists := make(map[string]bool, len(all))
	for _, f := range all {
		if f.Kind == facts.KindSymbol {
			exists[f.Name] = true
		}
	}
	for i := range all {
		for j := range all[i].Relations {
			r := &all[i].Relations[j]
			if r.Kind != facts.RelCalls || exists[r.Target] {
				continue
			}
			if t := barrels[r.Target]; t != "" && exists[t] {
				r.Target = t
			}
		}
	}
}
