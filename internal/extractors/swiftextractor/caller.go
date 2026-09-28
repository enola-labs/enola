package swiftextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/callsite"
	"github.com/enola-labs/enola/internal/facts"
)

// swiftCallerGrammar names Swift's functions for callsite.Attribute. A closure or a
// computed property held by a property is paired through the property_declaration;
// a closure passed to a call (`session.dataTask { … }`) is not a declaration and
// credits its enclosing function.
var swiftCallerGrammar = callsite.Grammar{
	KindOf: kindOf,
	Functions: map[string]bool{
		"function_declaration":          true,
		"protocol_function_declaration": true,
		"init_declaration":              true,
		"deinit_declaration":            true,
		"subscript_declaration":         true,
		"lambda_literal":                true,
		"computed_property":             true,
		"computed_getter":               true,
		"computed_setter":               true,
	},
	Wrappers: map[string]bool{
		"property_declaration": true,
	},
}

// extractFileWithClients is extractFileASTWithDir for the extractor's own path: it
// walks the file, appends the client routes the regex passes found in it, and names
// each call's calling function while the tree is still open.
func extractFileWithClients(src []byte, relFile string, isiOS bool, dir string, clients []facts.Fact) []facts.Fact {
	out := clients // what the file yields when the grammar cannot be loaded, as before
	withTree(src, relFile, isiOS, dir, func(root *sitter.Node, walked []facts.Fact) {
		out = append(walked, clients...)
		callsite.Attribute(swiftCallerGrammar, root, out)
	})
	return out
}
