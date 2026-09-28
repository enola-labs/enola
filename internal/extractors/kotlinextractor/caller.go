package kotlinextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/callsite"
	"github.com/enola-labs/enola/internal/facts"
)

// kotlinCallerGrammar names Kotlin's functions for callsite.Attribute. A
// function_declaration starts at its first annotation, the row its symbol and a
// Retrofit route on it both carry, so the route's caller is the interface method it
// declares. A lambda or anonymous function held by a property is paired through the
// property_declaration; a trailing lambda passed to a call is not a declaration and
// credits its enclosing function.
var kotlinCallerGrammar = callsite.Grammar{
	KindOf: kindOf,
	Functions: map[string]bool{
		"function_declaration":  true,
		"secondary_constructor": true,
		"anonymous_function":    true,
		"lambda_literal":        true,
		"getter":                true,
		"setter":                true,
		"anonymous_initializer": true,
	},
	Wrappers: map[string]bool{
		"property_declaration": true,
	},
}

// extractFileWithClients is extractFileAST for the extractor's own path: it walks the
// file, appends the client routes the regex passes found in it, and names each
// call's calling function while the tree is still open.
func extractFileWithClients(src []byte, relFile string, isAndroid bool, sourceRoot, basePackage string,
	packageIndex map[string]string, clients []facts.Fact) []facts.Fact {
	out := clients // what the file yields when the grammar cannot be loaded, as before
	withTree(src, relFile, isAndroid, sourceRoot, basePackage, packageIndex, func(root *sitter.Node, walked []facts.Fact) {
		out = append(walked, clients...)
		callsite.Attribute(kotlinCallerGrammar, root, out)
	})
	return out
}
