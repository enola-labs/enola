package swiftextractor

import (
	"strings"

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

// These bodies are mostly Codable plumbing on framework values
// (`try decoder.container(keyedBy:)`, `values.decode(...)`), so a member call on a
// receiver whose type the source does not state as a repository type binds
// nothing: the bare-name fallback would bind `values.decode` to the one repository
// method named decode.

// pushExtensionInit pushes, when no type owns node, the init method fact it is,
// with its parameters bound as typed locals, and returns what undoes that; nil
// when a type owns node.
func (w *astWalker) pushExtensionInit(node *sitter.Node) func() {
	enclosing := w.enclosingType()
	if w.currentOwner() >= 0 || enclosing == "" {
		return nil
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
			"exported":    w.exportedFor(modifiers),
			"language":    "swift",
			"receiver":    enclosing,
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	})
	w.pushOwner(len(w.out) - 1)
	w.typedReceiversOnly = true
	savedLocals := w.localTypes
	w.bindParameters(node)
	return func() {
		w.localTypes = savedLocals
		w.typedReceiversOnly = false
		w.popOwner()
	}
}

// handleMemberBody walks the body of a subscript or a deinit, in a type that owns
// its calls.
func (w *astWalker) handleMemberBody(node *sitter.Node) {
	if w.currentOwner() < 0 {
		return
	}
	w.noteTypeUsesIn(node)
	saved := w.typedReceiversOnly
	w.typedReceiversOnly = true
	for _, kind := range []string{"computed_property", "function_body"} {
		if body := findChildByKind(node, kind); body != nil {
			w.walkForCalls(body)
		}
	}
	w.typedReceiversOnly = saved
}

// isQualifiedType reports a type written through its outer type or module,
// `Query.Data.Node.Badge`: its last component alone would name whatever type of
// that name the caller's module declares, so a newly walked init names none.
func isQualifiedType(node *sitter.Node, src []byte) bool {
	t := nodeText(node, src)
	if i := strings.IndexByte(t, '<'); i >= 0 {
		t = t[:i]
	}
	return strings.Contains(t, ".")
}
