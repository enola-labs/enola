package cppextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// carryOverride gives an out-of-line member definition the virtual specifiers of
// its in-class declaration when the two sit in different directories. dedupeSymbols
// merges a declaration into its definition only under one fact name, which means one
// directory; a class declared under include/ and defined under src/ leaves the
// definition a fact of its own, without the `override` the class body states, so a
// member reached only through virtual dispatch read as unused.
//
// The definition takes the props only from a declaration of the same qualified name
// (namespace, class and member) that its directory's includes reach: the header it
// must include to define the member. A same-named class in a directory it cannot
// include is another program's.
func carryOverride(allFacts []facts.Fact, visibility *includeVisibility) {
	type decl struct {
		dir   string
		final bool
	}
	overrides := map[string][]decl{}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol || f.PropBool("has_body") || !f.PropBool("override") {
			continue
		}
		dir := sourceDir(f)
		if qualified, ok := strings.CutPrefix(f.Name, dir+"."); ok && strings.Contains(qualified, "::") {
			overrides[qualified] = append(overrides[qualified], decl{dir, f.PropBool("final")})
		}
	}
	if len(overrides) == 0 {
		return
	}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol || !f.PropBool("has_body") || f.PropBool("override") ||
			f.PropString("symbol_kind") != facts.SymbolMethod {
			continue
		}
		dir := sourceDir(f)
		qualified, ok := strings.CutPrefix(f.Name, dir+".")
		if !ok {
			continue
		}
		for _, d := range overrides[qualified] {
			if d.dir == dir || !visibility.reaches(dir, d.dir) {
				continue
			}
			f.SetProp("virtual", true)
			f.SetProp("override", true)
			if d.final {
				f.SetProp("final", true)
			}
			break
		}
	}
}
