package swiftextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// An init, a subscript and a deinit call in the name of the type that declares
// them: the type's fact owns their edges. An extension pushes no owner
// (handleExtension), so an init declared in one, the usual home of a Codable
// `init(from:)`, dropped every call and type it named. It is a member of its own,
// Base.init, a method the dead-code detector treats as an entry point by its name.
// No resolver indexes it (scope.go, swiftHierarchies): `self.init(...)`,
// `super.init(...)` and `Type.init(...)` bind as they did before it was a fact.
// A subscript in an extension still names nothing: a member fact called
// `subscript` would read as dead, since `x[i]` names no member.

// pushExtensionInit pushes, when no type owns node, the init method fact it is,
// and reports whether it did.
func (w *astWalker) pushExtensionInit(node *sitter.Node) bool {
	enclosing := w.enclosingType()
	if w.currentOwner() >= 0 || enclosing == "" {
		return false
	}
	modifiers := findChildByKind(node, "modifiers")
	w.out = append(w.out, facts.Fact{
		Kind:    facts.KindSymbol,
		Name:    w.dir + "." + w.qualify("init"),
		File:    w.relFile,
		Line:    int(node.StartPosition().Row) + 1,
		EndLine: tsutil.EndLine(node),
		Props: map[string]any{
			"symbol_kind": facts.SymbolMethod,
			"exported":    !isPrivateAccess(nodeText(modifiers, w.src)),
			"language":    "swift",
			"receiver":    enclosing,
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	})
	w.pushOwner(len(w.out) - 1)
	return true
}

// handleMemberBody walks the body of a subscript or a deinit, in a type that owns
// its calls.
func (w *astWalker) handleMemberBody(node *sitter.Node) {
	if w.currentOwner() < 0 {
		return
	}
	for _, kind := range []string{"computed_property", "function_body"} {
		if body := findChildByKind(node, kind); body != nil {
			w.walkForCalls(body)
		}
	}
}
