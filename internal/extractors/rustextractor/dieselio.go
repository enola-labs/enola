package rustextractor

import (
	"strings"
	"unicode"

	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// Diesel runs a query when one of its RunQueryDsl methods is handed a connection:
//
//	use diesel::prelude::*;
//	let krate = crates::table.filter(crates::name.eq(name)).first::<Crate>(conn)?;
//
// The names are common words (`first` on a slice, `load` on an atomic), so the
// name alone is not evidence. A call counts when all of these hold:
//
//   - the method is a RunQueryDsl one in rustDieselQueryMethods;
//   - the file brings diesel into scope: a `use` of diesel or diesel_async, of an
//     in-repo crate whose manifest depends on one of them (a crate re-exporting
//     diesel), or of anything named RunQueryDsl. The trait must be in scope for the
//     method to resolve, so a file without such a `use` cannot be calling it;
//   - the call has exactly one argument, the connection, and it is not a path
//     (`Ordering::SeqCst`), a capitalized name (`SeqCst`) or a literal.
//
// Missed: a file that gets the trait only through a glob of its own crate or a
// parent module (`use crate::prelude::*`, `use super::*`) re-exporting diesel, and
// the connection methods (`conn.batch_execute(sql)`). `execute` is in
// rustIOMethods already.

// rustDieselQueryMethods are diesel's RunQueryDsl methods that run a query, less
// `execute`, which rustIOMethods seeds on any receiver.
var rustDieselQueryMethods = map[string]bool{
	"load": true, "load_iter": true, "load_stream": true,
	"first": true, "get_result": true, "get_results": true,
}

// rustDieselCrates are the crates whose RunQueryDsl runs a diesel query.
var rustDieselCrates = map[string]bool{"diesel": true, "diesel_async": true}

// rustDieselInScope reports whether a file's `use` declarations bring diesel's
// query traits into scope.
func rustDieselInScope(root *sitter.Node, src []byte, crates []crateInfo) bool {
	found := false
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		if found || n == nil {
			return
		}
		if kindOf(n) == "use_declaration" {
			if arg := n.ChildByFieldName("argument"); arg != nil && rustUseBringsDiesel(nodeText(arg, src), crates) {
				found = true
			}
			return
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			visit(n.NamedChild(i))
		}
	}
	visit(root)
	return found
}

// rustUseBringsDiesel reports whether one `use` argument names diesel, an in-repo
// crate depending on diesel, or RunQueryDsl.
func rustUseBringsDiesel(arg string, crates []crateInfo) bool {
	arg = strings.TrimPrefix(strings.TrimSpace(arg), "::")
	first := arg
	if i := strings.IndexAny(first, ":{ ;"); i >= 0 {
		first = first[:i]
	}
	if rustDieselCrates[first] {
		return true
	}
	for _, c := range crates {
		if c.name != first {
			continue
		}
		for dep := range rustDieselCrates {
			if c.deps[dep] {
				return true
			}
		}
	}
	return strings.Contains(arg, "RunQueryDsl")
}

// isDieselQueryCall reports whether a call_expression runs a diesel query, by the
// rule above.
func (w *astWalker) isDieselQueryCall(call *sitter.Node) bool {
	if !w.dieselInScope {
		return false
	}
	fn := call.ChildByFieldName("function")
	if fn != nil && kindOf(fn) == "generic_function" {
		fn = fn.ChildByFieldName("function")
	}
	if fn == nil || kindOf(fn) != "field_expression" {
		return false
	}
	field := fn.ChildByFieldName("field")
	if field == nil || !rustDieselQueryMethods[nodeText(field, w.src)] {
		return false
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return false
	}
	var only *sitter.Node
	for i := uint(0); i < args.NamedChildCount(); i++ {
		a := args.NamedChild(i)
		switch kindOf(a) {
		case "line_comment", "block_comment", "attribute_item":
			continue
		}
		if only != nil {
			return false
		}
		only = a
	}
	return only != nil && w.isConnectionArg(only)
}

// isConnectionArg reports whether an argument can be a connection value: not a
// path, a capitalized name (an enum variant or a constant) or a literal, seen
// through `&`, `&mut`, `*` and parentheses.
func (w *astWalker) isConnectionArg(a *sitter.Node) bool {
	for a != nil {
		switch kindOf(a) {
		case "reference_expression", "unary_expression", "parenthesized_expression":
			if a.NamedChildCount() == 0 {
				return false
			}
			a = a.NamedChild(a.NamedChildCount() - 1)
		case "identifier":
			name := nodeText(a, w.src)
			return name != "" && !unicode.IsUpper(rune(name[0]))
		case "self", "field_expression", "call_expression", "try_expression", "index_expression", "await_expression":
			return true
		default:
			return false
		}
	}
	return false
}
