package tsextractor

import (
	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
)

// superclassProp is the base class a class extends, exactly as the source writes
// it and one level only — the same meaning rubyextractor gives the prop, so an
// architecture rule about a base class reads the same in both languages.
//
// superclassModuleProp is the module that identifier was imported from. It is
// what the Ruby prop does not need: a Ruby superclass token is a globally
// resolvable constant, while a JavaScript one is bound by an import and means
// whatever that file imported. `Controller` is @hotwired/stimulus' base class in
// one file and @ember/controller's in the next — measured over one production
// Ember frontend's 4,417 class facts, 150 against 259 — and a prop carrying
// only the identifier fuses them. The module is read from the file's own import
// table and from nowhere else: never from the identifier's spelling, never from
// the file's path.
const (
	superclassProp       = "superclass"
	superclassModuleProp = "superclass_module"
)

// tsExtendsValue returns the node a class's extends clause names, or nil when the
// class extends nothing. A class_heritage also carries the implements clause,
// whose type names are a different relation and never a base class, and the
// clause admits a leading comment before the value.
func tsExtendsValue(kinds *tsutil.KindTable, classNode *sitter.Node) *sitter.Node {
	for i := range classNode.ChildCount() {
		heritage := classNode.Child(i)
		if kindOf(kinds, heritage) != "class_heritage" {
			continue
		}
		for j := range heritage.ChildCount() {
			clause := heritage.Child(j)
			if kindOf(kinds, clause) != "extends_clause" {
				continue
			}
			for k := range clause.ChildCount() {
				value := clause.Child(k)
				if value.IsNamed() && kindOf(kinds, value) != "comment" {
					return value
				}
			}
		}
	}
	return nil
}

// tsSuperclassName returns the base class a class extends as the source writes
// it, or "" when the source names none.
//
// Only a bare identifier is a name. `extends Base<T>` is one too: the type
// arguments are types applied to the base, not the base. Every other form —
// `extends mixin(A, B)`, `extends foo.Bar`, `extends (cond ? A : B)`,
// `extends new Factory()` — reaches its base through a value the source does not
// state, and a class built by a mixin factory has no static base class to name.
// Answering those from the nearest identifier would name the factory, the
// namespace, or the condition, so they name nothing.
func tsSuperclassName(kinds *tsutil.KindTable, classNode *sitter.Node, src []byte) string {
	value := tsExtendsValue(kinds, classNode)
	if value == nil || kindOf(kinds, value) != "identifier" {
		return ""
	}
	return nodeText(value, src)
}
