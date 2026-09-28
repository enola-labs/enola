package tsextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// functionLikeKinds are the node kinds whose body is a function. Both spellings of
// the function expression are listed because the grammar renamed it.
var functionLikeKinds = map[string]bool{
	"function_declaration":           true,
	"generator_function_declaration": true,
	"function_expression":            true,
	"function":                       true,
	"generator_function":             true,
	"arrow_function":                 true,
	"method_definition":              true,
}

// declarationWrappers are the nodes a function can sit inside while still being the
// value a declaration names, so its symbol's Line is the wrapper's start.
var declarationWrappers = map[string]bool{
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

// funcSpan is one function-like node's row range and the symbol it was emitted as.
type funcSpan struct {
	start, end uint
	symbol     string
}

// attributeClientCallers sets facts.PropCaller on every hand-written client route in
// result: the symbol of the innermost function containing the call site that this
// file emitted a symbol for.
//
// A function node and its symbol are paired by line. A symbol's Line is the start of
// the node the declaration walk emitted it for, which is the function itself or a
// declaration wrapping it (`const f = () => …`, `export const …`, a class field
// holding an arrow), so a node is matched on its own start row or that of a wrapping
// declaration, and on no other ancestor's. Two symbols starting on one of those rows
// leave the node unmatched rather than guessed at.
//
// A call inside a callback with no symbol of its own (`.then(() => fetch(…))`) is
// credited to the nearest enclosing function that has one, which is how the call
// walk credits that callback's other calls. A call at module scope gets no caller.
func attributeClientCallers(kinds *tsutil.KindTable, root *sitter.Node, result []facts.Fact) {
	var calls []int
	for i := range result {
		f := &result[i]
		if f.Kind == facts.KindRoute && f.Line > 0 && f.PropAny(facts.PropRole) == facts.RoleClient &&
			facts.HandWrittenClientSources[f.PropString(facts.PropSource)] {
			calls = append(calls, i)
		}
	}
	if len(calls) == 0 {
		return
	}

	byLine := map[uint][]string{}
	for _, f := range result {
		if f.Kind == facts.KindSymbol && f.Line > 0 {
			byLine[uint(f.Line-1)] = append(byLine[uint(f.Line-1)], f.Name)
		}
	}
	symbolAt := func(n *sitter.Node) string {
		for {
			if names := byLine[n.StartPosition().Row]; len(names) > 0 {
				if len(names) == 1 {
					return names[0]
				}
				return ""
			}
			p := n.Parent()
			if p == nil || !declarationWrappers[kindOf(kinds, p)] {
				return ""
			}
			n = p
		}
	}

	var spans []funcSpan
	stack := []*sitter.Node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if functionLikeKinds[kindOf(kinds, n)] {
			spans = append(spans, funcSpan{start: n.StartPosition().Row, end: n.EndPosition().Row, symbol: symbolAt(n)})
		}
		for i := range n.ChildCount() {
			stack = append(stack, n.Child(i))
		}
	}

	for _, i := range calls {
		row := uint(result[i].Line - 1)
		best := -1
		for j, s := range spans {
			if s.symbol == "" || row < s.start || row > s.end {
				continue
			}
			if best < 0 || s.end-s.start < spans[best].end-spans[best].start {
				best = j
			}
		}
		if best >= 0 {
			result[i].SetProp(facts.PropCaller, spans[best].symbol)
		}
	}
}
