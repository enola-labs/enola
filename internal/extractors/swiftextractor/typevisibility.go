package swiftextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// A type name written in a file means the type of that name the file can see: its
// own module's first; else the one top-level type of that name in a module the file
// imports. A nested type is in scope by its bare name only inside its outer type,
// an extension of it in another module included, or from a folder of the same
// Swift module. A type of the name in a target the file's module does not reach through its
// imports is a different type that happens to share the name (a GraphQL fragment's
// nested `Project` against the app's `Project`), so it is never the one meant. A
// module that is no import unit (a loose folder, a package of a subdivided app
// target) is visible as before.
//
// Imports are followed transitively: a module that re-exports another
// (`@_exported import`) makes its types visible to its importers, and the import
// facts do not record which imports are re-exports. A module the file imports
// itself comes first: only when none of those declares the name is a module
// reached through them consulted.

// swiftTypeDeclSite is one module's declaration of a type name.
type swiftTypeDeclSite struct {
	dir      string
	topLevel bool
	outer    string // the simple name of the type a nested type is declared in
}

// swiftTypeVisibility answers which module a bare type name in a file means.
type swiftTypeVisibility struct {
	moduleOf    func(file string) string
	importUnits map[string]bool            // module identities a file must import to see
	direct      map[string]map[string]bool // module -> module identities its files import
	imports     map[string]map[string]bool // module -> module identities it reaches
}

// swiftModuleName is the name `import` writes for a target: `Kickstarter-Framework`
// is imported as Kickstarter_Framework.
func swiftModuleName(target string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, target)
}

// newSwiftTypeVisibility reads the import units (SwiftPM targets among allFacts,
// and units, the XcodeGen target names to their identities) and what each module
// reaches through its imports. Imports are read per module, not per file: one
// module's import facts are recorded once, at the first file that writes them.
func newSwiftTypeVisibility(allFacts []facts.Fact, units map[string]string, moduleOf func(string) string) *swiftTypeVisibility {
	byName := map[string]string{}
	for name, id := range units {
		byName[swiftModuleName(name)] = id
	}
	for _, f := range allFacts {
		if f.Kind != facts.KindModule {
			continue
		}
		if name, ok := f.PropAny("spm_target").(string); ok && name != "" {
			byName[swiftModuleName(name)] = f.Name
		}
	}
	v := &swiftTypeVisibility{moduleOf: moduleOf, importUnits: map[string]bool{}, imports: map[string]map[string]bool{}}
	for _, id := range byName {
		v.importUnits[id] = true
	}
	direct := map[string]map[string]bool{} // module -> modules its files import
	for _, f := range allFacts {
		if f.Kind != facts.KindDependency {
			continue
		}
		for _, r := range f.Relations {
			id, ok := byName[r.Target]
			if !ok || r.Kind != facts.RelImports {
				continue
			}
			addTo(direct, moduleOf(f.File), id)
		}
	}
	for mod, mods := range direct {
		reach := map[string]bool{}
		queue := make([]string, 0, len(mods))
		for m := range mods {
			queue = append(queue, m)
		}
		for len(queue) > 0 {
			m := queue[0]
			queue = queue[1:]
			if reach[m] {
				continue
			}
			reach[m] = true
			for d := range direct[m] {
				queue = append(queue, d)
			}
		}
		v.imports[mod] = reach
	}
	v.direct = direct
	return v
}

func addTo(m map[string]map[string]bool, key, val string) {
	if m[key] == nil {
		m[key] = map[string]bool{}
	}
	m[key][val] = true
}

// resolve returns the module the type name means where from is declared, among the
// declarations of it, or "" when none is visible or several are.
func (v *swiftTypeVisibility) resolve(from *facts.Fact, decls []swiftTypeDeclSite) string {
	own := v.moduleOf(from.File)
	for _, d := range decls {
		if d.dir == own {
			return own
		}
	}
	if dir, found := v.pick(from, decls, v.direct[own]); found {
		return dir
	}
	dir, _ := v.pick(from, decls, v.imports[own])
	return dir
}

// pick resolves among the declarations visible through the modules seen: the one
// top-level type, else the one nested type in scope. found reports whether any
// declaration was visible, so a name ambiguous among the direct imports is not
// settled by a module reached only through them.
func (v *swiftTypeVisibility) pick(from *facts.Fact, decls []swiftTypeDeclSite, seen map[string]bool) (dir string, found bool) {
	var top, nested []string
	for _, d := range decls {
		if v.importUnits[d.dir] && !seen[d.dir] {
			continue
		}
		switch {
		case d.topLevel:
			top = appendNew(top, d.dir)
		case !v.importUnits[d.dir] || strings.Contains("."+from.Name+".", "."+d.outer+"."):
			nested = appendNew(nested, d.dir)
		}
	}
	switch {
	case len(top) == 1:
		return top[0], true
	case len(top) == 0 && len(nested) == 1:
		return nested[0], true
	}
	return "", len(top)+len(nested) > 0
}

func appendNew(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}
