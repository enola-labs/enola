package rubyextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/enola-labs/enola/internal/treesitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

// rubyKinds names Ruby node kinds without allocating. go-tree-sitter's
// Node.Kind() builds a fresh Go string from the grammar's already-interned C string
// on every call; see tsutil.KindTable.
//
// A package-level table is correct here because this extractor parses exactly one
// grammar. Reading one grammar's symbol ids against another's table would return
// plausible, wrong kind names with no other symptom, which is why the extractors that
// parse several carry their table alongside the language instead.
var rubyKinds = tsutil.KindsFor(ruby.Language())

// kindOf is the allocation-free spelling of node.Kind().
func kindOf(node *sitter.Node) string { return rubyKinds.Of(node) }
