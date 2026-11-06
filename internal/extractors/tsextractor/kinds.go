package tsextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/enola-labs/enola/internal/treesitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// Like cppextractor, this extractor parses more than one grammar — TypeScript and
// TSX, including for the script blocks it lifts out of Vue and Svelte files — so it
// cannot keep one package-level kind table. Symbol ids are per grammar and only 12%
// of them agree between the two, so the wrong table yields a real kind name for the
// wrong node type: no error, just a construct silently unrecognised.
//
// The table therefore travels with the parse, chosen from the same grammar handed to
// the parser. TestKindTable_MatchesNodeKind checks both.
// tsKindsFor returns the table for the grammar a file is parsed with, resolved once
// per grammar and shared thereafter.
func tsKindsFor(tsx bool) *tsutil.KindTable {
	if tsx {
		return tsutil.KindsFor(typescript.LanguageTSX())
	}
	return tsutil.KindsFor(typescript.LanguageTypescript())
}

// kindOf is the allocation-free spelling of node.Kind(), given the table for the
// grammar that produced the node.
func kindOf(t *tsutil.KindTable, node *sitter.Node) string { return t.Of(node) }
