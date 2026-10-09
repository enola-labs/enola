package tsextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Loops whose trip count is fixed where the code is written. Such a loop raises
// loop_depth and adds no factor of n, and a call inside it runs a fixed number of
// times.
//
// A literal collection was the only form recognised. The rest of what is fixed is
// fixed by NAME, which is how code says so:
//
//	for (const p of PACKAGES) …              an ALL_CAPS constant
//	Object.entries(FONT_FAMILY).forEach(…)   a view of one
//	for (let i = 0; i < 4; i++) …            a literal bound
//	for (let x = 0; x < GRID_COLUMNS - 8; x++) …   arithmetic over constants
//	const prefixes = ["a/", "b/"]; prefixes.some(…)   a local that is a literal and stays one
//
// Each is a syntactic proof and fails closed: a name that merely looks constant
// (mixed case, a parameter, a mutated local) keeps counting.

// tsScreaming reports whether name is SCREAMING_SNAKE_CASE with at least two
// characters. One capital letter is as often a type parameter or a loop bound
// someone called N.
func tsScreaming(name string) bool {
	if len(name) < 2 {
		return false
	}
	hasLetter := false
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return hasLetter
}

// tsConstViews are the calls that return a view of their argument, as many
// elements as it has.
var tsConstViews = map[string]bool{
	"Object.keys": true, "Object.values": true, "Object.entries": true, "Array.from": true,
}

// tsConstantIterable reports whether n is a collection with a fixed number of
// elements. consts are the local names proven to be literal collections.
func tsConstantIterable(kinds *tsutil.KindTable, n *sitter.Node, src []byte, consts map[string]bool) bool {
	if n == nil {
		return false
	}
	switch kindOf(kinds, n) {
	case "array", "object":
		return true
	case "identifier":
		name := nodeText(n, src)
		return tsScreaming(name) || consts[name]
	case "member_expression":
		// `Config.STOP_WORDS`: the constant is the trailing member.
		if prop := n.ChildByFieldName("property"); prop != nil {
			return tsScreaming(nodeText(prop, src))
		}
	case "parenthesized_expression", "as_expression", "satisfies_expression", "non_null_expression":
		if n.NamedChildCount() > 0 {
			return tsConstantIterable(kinds, n.NamedChild(0), src, consts)
		}
	case "call_expression":
		fn := n.ChildByFieldName("function")
		args := n.ChildByFieldName("arguments")
		if fn == nil || args == nil || args.NamedChildCount() == 0 || !tsConstViews[nodeText(fn, src)] {
			return false
		}
		return tsConstantIterable(kinds, args.NamedChild(0), src, consts)
	}
	return false
}

// tsConstantExpr reports whether n is built only from number literals and
// ALL_CAPS constants: `4`, `MAX_ROWS`, `GRID_COLUMNS - 8`, `Limits.MAX * 2`.
func tsConstantExpr(kinds *tsutil.KindTable, n *sitter.Node, src []byte) bool {
	if n == nil {
		return false
	}
	switch kindOf(kinds, n) {
	case "number":
		return true
	case "identifier":
		return tsScreaming(nodeText(n, src))
	case "member_expression":
		if prop := n.ChildByFieldName("property"); prop != nil {
			return tsScreaming(nodeText(prop, src))
		}
	case "parenthesized_expression":
		return n.NamedChildCount() > 0 && tsConstantExpr(kinds, n.NamedChild(0), src)
	case "binary_expression":
		op := n.ChildByFieldName("operator")
		if op == nil {
			return false
		}
		switch nodeText(op, src) {
		case "+", "-", "*", "/":
			return tsConstantExpr(kinds, n.ChildByFieldName("left"), src) &&
				tsConstantExpr(kinds, n.ChildByFieldName("right"), src)
		}
	}
	return false
}

// tsForConstBounded reports whether a C-style for runs between two constants:
// `for (let i = 0; i < 4; i++)`, `for (let x = 0; x < GRID_COLUMNS - 8; x++)`. The
// other side of the comparison has to be a plain identifier, so `i < 4 && more()`
// and `i < rows.length` keep counting.
//
// BOTH ends have to be constant. `for (let i = rows.length - 1; i >= 0; i--)`
// compares against a literal and walks every row: the bound that scales is where
// it starts.
func tsForConstBounded(kinds *tsutil.KindTable, n *sitter.Node, src []byte) bool {
	if !tsForStartsAtConstant(kinds, n, src) {
		return false
	}
	cond := n.ChildByFieldName("condition")
	for cond != nil && kindOf(kinds, cond) != "binary_expression" && cond.NamedChildCount() == 1 {
		cond = cond.NamedChild(0)
	}
	if cond == nil || kindOf(kinds, cond) != "binary_expression" || n.ChildByFieldName("increment") == nil {
		return false
	}
	op := cond.ChildByFieldName("operator")
	if op == nil {
		return false
	}
	switch nodeText(op, src) {
	case "<", "<=", ">", ">=", "!=", "!==":
	default:
		return false
	}
	left, right := cond.ChildByFieldName("left"), cond.ChildByFieldName("right")
	if left == nil || right == nil {
		return false
	}
	counter := func(x *sitter.Node) bool {
		return kindOf(kinds, x) == "identifier" && !tsScreaming(nodeText(x, src))
	}
	return (counter(left) && tsConstantExpr(kinds, right, src)) || (counter(right) && tsConstantExpr(kinds, left, src))
}

// tsMutators are the methods that change how many elements an array, Set or Map
// holds.
var tsMutators = []string{".push(", ".unshift(", ".splice(", ".add(", ".set(", ".pop(", ".shift(", ".delete(", ".length ="}

// tsConstLiteralLocals returns the names a function body binds, with `const`, to
// an array or object literal and never grows: no mutating call on the name and no
// element assignment anywhere in the body.
//
// The mutation test is textual and so errs towards refusing: a `.push(` on a
// different variable of the same name in a nested scope disqualifies both.
func tsConstLiteralLocals(kinds *tsutil.KindTable, body *sitter.Node, src []byte) map[string]bool {
	if body == nil {
		return nil
	}
	var out map[string]bool
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if kindOf(kinds, n) == "lexical_declaration" && strings.HasPrefix(nodeText(n, src), "const") {
			for i := range n.NamedChildCount() {
				d := n.NamedChild(i)
				if kindOf(kinds, d) != "variable_declarator" {
					continue
				}
				name, value := d.ChildByFieldName("name"), d.ChildByFieldName("value")
				if name == nil || value == nil || kindOf(kinds, name) != "identifier" {
					continue
				}
				if k := kindOf(kinds, value); k == "array" || k == "object" {
					if out == nil {
						out = make(map[string]bool)
					}
					out[nodeText(name, src)] = true
				}
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(body)
	if len(out) == 0 {
		return nil
	}
	text := string(src[body.StartByte():body.EndByte()])
	for name := range out {
		grows := strings.Contains(text, name+"[")
		for _, m := range tsMutators {
			grows = grows || strings.Contains(text, name+m)
		}
		if grows {
			delete(out, name)
		}
	}
	return out
}

// tsForStartsAtConstant reports whether a C-style for declares exactly one counter
// and initialises it to a constant.
func tsForStartsAtConstant(kinds *tsutil.KindTable, n *sitter.Node, src []byte) bool {
	init := n.ChildByFieldName("initializer")
	if init == nil {
		return false
	}
	decls := 0
	for i := range init.NamedChildCount() {
		d := init.NamedChild(i)
		if kindOf(kinds, d) != "variable_declarator" {
			continue
		}
		decls++
		if !tsConstantExpr(kinds, d.ChildByFieldName("value"), src) {
			return false
		}
	}
	return decls == 1
}
