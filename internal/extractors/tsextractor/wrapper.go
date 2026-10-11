package tsextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
)

// wrapperDepth bounds how far a wrapper inside a wrapper is followed. One level is
// the pattern; a second covers a bundle that wraps each file it concatenates.
const wrapperDepth = 2

// isFunctionValue reports whether a node is a function written as an expression.
func isFunctionValue(kinds *tsutil.KindTable, n *sitter.Node) bool {
	switch kindOf(kinds, n) {
	case "function_expression", "arrow_function", "generator_function":
		return true
	}
	return false
}

// unwrapExpression strips what can stand around an expression without changing which
// expression it is: parentheses, and the `!` or `void` a wrapper is sometimes prefixed
// with so that the parser reads `function` as an expression.
func unwrapExpression(kinds *tsutil.KindTable, n *sitter.Node) *sitter.Node {
	for n != nil {
		switch kindOf(kinds, n) {
		case "parenthesized_expression":
			if n.NamedChildCount() == 0 {
				return nil
			}
			n = n.NamedChild(0)
		case "unary_expression":
			n = n.ChildByFieldName("argument")
		default:
			return n
		}
	}
	return nil
}

// wrapperBodies returns the bodies of the functions a statement runs as the file
// loads, which are where a file that is not a module keeps its declarations:
//
//	(function (global) { function roll() {} ... })(window);
//
// A script loaded by a <script> tag shares one global scope with every other script,
// so the convention is to put the file's contents in a function and call it at once.
// To the language those declarations are local to a function; to anyone reading the
// file they are its top level, and reading them as locals leaves the file with no
// symbols at all.
//
// The function called is one body. A function passed to it is another: in the
// "universal module" form the wrapper only decides how to export, and the
// declarations are in the factory handed to it.
func wrapperBodies(kinds *tsutil.KindTable, stmt *sitter.Node) []*sitter.Node {
	if kindOf(kinds, stmt) != "expression_statement" || stmt.NamedChildCount() == 0 {
		return nil
	}
	call := unwrapExpression(kinds, stmt.NamedChild(0))
	if kindOf(kinds, call) != "call_expression" {
		return nil
	}
	callee := unwrapExpression(kinds, call.ChildByFieldName("function"))
	if !isFunctionValue(kinds, callee) {
		return nil
	}
	fns := []*sitter.Node{callee}
	if args := call.ChildByFieldName("arguments"); args != nil {
		for i := range args.NamedChildCount() {
			if arg := unwrapExpression(kinds, args.NamedChild(i)); isFunctionValue(kinds, arg) {
				fns = append(fns, arg)
			}
		}
	}
	var bodies []*sitter.Node
	for _, fn := range fns {
		if body := fn.ChildByFieldName("body"); kindOf(kinds, body) == "statement_block" {
			bodies = append(bodies, body)
		}
	}
	return bodies
}
