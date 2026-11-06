package javaextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/enola-labs/enola/internal/treesitter"
	java "github.com/tree-sitter/tree-sitter-java/bindings/go"
)

// javaKinds names Java node kinds without allocating. go-tree-sitter's
// Node.Kind() builds a fresh Go string from the grammar's already-interned C string
// on every call; see tsutil.KindTable.
//
// A package-level table is correct here because this extractor parses exactly one
// grammar. Reading one grammar's symbol ids against another's table would return
// plausible, wrong kind names with no other symptom, which is why the extractors that
// parse several carry their table alongside the language instead.
var javaKinds = tsutil.KindsFor(java.Language())

// kindOf is the allocation-free spelling of node.Kind().
func kindOf(node *sitter.Node) string { return javaKinds.Of(node) }
