package tsextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
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

// wrapperBody returns the body of the function a statement calls where it is written,
// which is where a file that is not a module keeps its declarations:
//
//	(function (global) { function roll() {} ... })(window);
//
// A script loaded by a <script> tag shares one global scope with every other script,
// so the convention is to put the file's contents in a function and call it at once.
// To the language those declarations are local to a function; to anyone reading the
// file they are its top level, and reading them as locals leaves the file with no
// symbols at all.
//
// A function PASSED to the wrapper is not read, though the "universal module" form
// keeps its declarations in one. That form is what a library is distributed in, and
// across the benchmark corpus it was found almost only in checked-in builds of other
// people's code: 3,304 of the symbols it yielded in one application were from five
// such files, against 3 symbols in the two repositories whose own code is wrapped.
func wrapperBody(kinds *tsutil.KindTable, stmt *sitter.Node) *sitter.Node {
	_, callee := wrapperCall(kinds, stmt)
	if callee == nil {
		return nil
	}
	if body := callee.ChildByFieldName("body"); kindOf(kinds, body) == "statement_block" {
		return body
	}
	return nil
}

// wrapperCall returns the call a statement makes to a function written where it is
// called, and that function. Both are nil for any other statement.
func wrapperCall(kinds *tsutil.KindTable, stmt *sitter.Node) (call, callee *sitter.Node) {
	if kindOf(kinds, stmt) != "expression_statement" || stmt.NamedChildCount() == 0 {
		return nil, nil
	}
	call = unwrapExpression(kinds, stmt.NamedChild(0))
	if kindOf(kinds, call) != "call_expression" {
		return nil, nil
	}
	callee = unwrapExpression(kinds, call.ChildByFieldName("function"))
	if !isFunctionValue(kinds, callee) {
		return nil, nil
	}
	return call, callee
}

// functionValue returns the function a variable is declared as, when its value is one
// written out as an expression.
func functionValue(kinds *tsutil.KindTable, declarator *sitter.Node) *sitter.Node {
	for _, kind := range []string{"arrow_function", "function_expression", "generator_function"} {
		if v := findChildByKind(kinds, declarator, kind); v != nil {
			return v
		}
	}
	return nil
}

// withoutPlainVariables drops the variables a wrapper declares that are neither
// functions nor objects with members of their own, and that it does not hand out
// (see wrapperExposes): one it hands out can be used from outside after all.
//
// A module's top-level variable can be imported by another file, which is why it is a
// symbol. A wrapper's cannot: the wrapper exists to keep them private, and most are
// names it gives to globals for its own use (`var $ = jQuery`, `var __ = wp.i18n.__`).
// Every file that does so declares the same short name, and they would all be one
// symbol. In one corpus application they were 320 of the 440 symbols wrappers added.
func withoutPlainVariables(ff []facts.Fact) []facts.Fact {
	hasMembers := make(map[string]bool)
	for _, f := range ff {
		if i := strings.LastIndex(f.Name, "."); i > 0 {
			hasMembers[f.Name[:i]] = true
		}
	}
	kept := ff[:0]
	for _, f := range ff {
		// A class's field is a variable too, and is the class's, not the wrapper's.
		_, member := f.Props["receiver"]
		plain := f.Kind == facts.KindSymbol && f.Props["symbol_kind"] == facts.SymbolVariable &&
			!member && !hasMembers[f.Name] && f.Props["exported"] != true
		if !plain {
			kept = append(kept, f)
		}
	}
	return kept
}
