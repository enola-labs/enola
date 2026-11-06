package cppextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/enola-labs/enola/internal/treesitter"
	c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
)

// This extractor parses TWO grammars, so unlike the single-grammar extractors it
// cannot keep one package-level kind table: symbol ids are assigned per grammar and
// only 17% of the ids below tree-sitter-c's symbol count mean the same thing in
// tree-sitter-cpp. Reading a C node's id out of the C++ table returns a real kind
// name belonging to a different node type — plausible, wrong, and with no symptom
// beyond the extractor quietly failing to recognise a construct.
//
// So the table travels with the parse: kindsFor picks it from the same `lang` value
// that selected the grammar, the walker carries it, and free helpers take it as a
// parameter. TestKindTable_MatchesNodeKind checks both grammars.
func kindsFor(lang string) *tsutil.KindTable {
	if lang == langC {
		return tsutil.KindsFor(c.Language())
	}
	return tsutil.KindsFor(cpp.Language())
}

// kindOf is the allocation-free spelling of node.Kind(), given the table for the
// grammar that produced the node.
func kindOf(t *tsutil.KindTable, node *sitter.Node) string { return t.Of(node) }
