package tsextractor

import (
	"github.com/enola-labs/enola/internal/extractors/callsite"
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// tsFunctionKinds are the node kinds whose body is a function. Both spellings of the
// function expression are listed because the grammar renamed it.
var tsFunctionKinds = map[string]bool{
	"function_declaration":           true,
	"generator_function_declaration": true,
	"function_expression":            true,
	"function":                       true,
	"generator_function":             true,
	"arrow_function":                 true,
	"method_definition":              true,
}

// tsDeclarationWrappers are the nodes a function can sit inside while still being
// the value a declaration names (`const f = () => …`, `export const …`, a class
// field holding an arrow), so its symbol's Line is the wrapper's start.
var tsDeclarationWrappers = map[string]bool{
	"variable_declarator":      true,
	"lexical_declaration":      true,
	"variable_declaration":     true,
	"export_statement":         true,
	"public_field_definition":  true,
	"pair":                     true,
	"assignment_expression":    true,
	"expression_statement":     true,
	"parenthesized_expression": true,
	"as_expression":            true,
	"satisfies_expression":     true,
	"type_assertion":           true,
	"non_null_expression":      true,
}

// attributeClientCallers names the calling function of every hand-written client
// call site in result; see callsite.Attribute. An object-literal property holds a
// function but gets no symbol, so a call inside one names nothing.
func attributeClientCallers(kinds *tsutil.KindTable, root *sitter.Node, result []facts.Fact) {
	callsite.Attribute(callsite.Grammar{
		KindOf:    func(n *sitter.Node) string { return kindOf(kinds, n) },
		Functions: tsFunctionKinds,
		Wrappers:  tsDeclarationWrappers,
	}, root, result)
}
