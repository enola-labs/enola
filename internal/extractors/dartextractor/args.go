package dartextractor

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// Argument-list helpers. The grammar spells a positional argument `argument` and a
// named one `named_argument` with a `label` child, so reading `GoRoute(path: '/x')`
// means asking for a named argument by label while `dio.get('/x')` means asking for a
// positional one by index.

// argumentsOf returns the `arguments` node of an invocation selector (or of an
// annotation, which holds its arguments directly).
func argumentsOf(n *sitter.Node) *sitter.Node {
	if n == nil {
		return nil
	}
	if kindOf(n) == "arguments" {
		return n
	}
	return firstOfKind(n, "arguments")
}

// namedArgOf returns the value node of a named argument, reading labels from src.
func namedArgOf(args *sitter.Node, src []byte, label string) *sitter.Node {
	if args == nil {
		return nil
	}
	for _, a := range namedChildren(args) {
		if kindOf(a) != "named_argument" {
			continue
		}
		lbl := childOfKind(a, "label")
		if lbl == nil {
			continue
		}
		if strings.TrimSuffix(strings.TrimSpace(lbl.Utf8Text(src)), ":") != label {
			continue
		}
		// A named_argument is [label, value...]; the value is everything after the
		// label, and the first of those nodes is the one worth reading.
		//
		// Deliberately indexed rather than found by comparing against lbl: every call
		// to namedChildren allocates FRESH node wrappers, so two lookups of the same
		// child are different pointers and `k == lbl` is never true. That silently
		// returned nil for every named argument until it was found.
		if kids := namedChildren(a); len(kids) > 1 {
			return kids[1]
		}
	}
	return nil
}

// namedArgText returns the full source text of a named argument's value.
//
// namedArgOf returns only the FIRST value node, which is right for a literal but wrong
// for anything the grammar flattens into a selector chain: `path: Home.routeName` has
// value nodes [identifier Home][selector .routeName], so reading the first alone yields
// `Home` and the reference silently stops being one.
func namedArgText(args *sitter.Node, src []byte, label string) string {
	if args == nil {
		return ""
	}
	for _, a := range namedChildren(args) {
		if kindOf(a) != "named_argument" {
			continue
		}
		lbl := childOfKind(a, "label")
		if lbl == nil {
			continue
		}
		if strings.TrimSuffix(strings.TrimSpace(lbl.Utf8Text(src)), ":") != label {
			continue
		}
		full := strings.TrimSpace(a.Utf8Text(src))
		_, rest, ok := strings.Cut(full, ":")
		if !ok {
			return ""
		}
		return strings.TrimSpace(rest)
	}
	return ""
}

// positionalArg returns the i-th positional argument node.
//
// The WRAPPER is returned, not its first child. An argument that is a selector chain —
// `Uri.parse('/api/orders')`, the standard spelling for package:http — flattens into
// [identifier Uri][selector .parse][selector (…)], so returning the first child yields
// the bare text `Uri` and the URL disappears. Callers that want a plain literal are
// unaffected: stringLiteralValue only accepts a literal that IS the whole node, which
// is exactly the distinction between `'/api/orders'` and `Uri.parse('/api/orders')`.
func positionalArg(args *sitter.Node, i int) *sitter.Node {
	if args == nil {
		return nil
	}
	n := 0
	for _, a := range namedChildren(args) {
		if kindOf(a) != "argument" {
			continue
		}
		if n == i {
			return a
		}
		n++
	}
	return nil
}

// stringLiteralValue returns a string literal's content, or "" when the node is not a
// literal this extractor can read.
//
// An INTERPOLATED string returns "" deliberately. `'/users/$id'` is a template, not a
// path, and publishing it with the `$id` still in it would put a route in the graph
// that no server serves and no matcher can normalise — the same reason the PHP
// extractor skips interpolated URLs and the Scala one tags an unresolved prefix rather
// than emitting it.
func stringLiteralValue(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	if kindOf(n) != "string_literal" {
		lit := firstOfKind(n, "string_literal")
		if lit == nil {
			return ""
		}
		// Only accept a literal that IS the whole expression, not one buried in a
		// larger expression (a concatenation, a ternary).
		if strings.TrimSpace(lit.Utf8Text(src)) != strings.TrimSpace(n.Utf8Text(src)) {
			return ""
		}
		n = lit
	}
	if firstOfKind(n, "template_substitution", "string_interpolation") != nil {
		return ""
	}
	txt := strings.TrimSpace(n.Utf8Text(src))
	if strings.ContainsAny(txt, "$") {
		return ""
	}
	txt = strings.TrimPrefix(txt, "r") // raw string prefix
	for _, q := range []string{`"""`, "'''", `"`, "'"} {
		if strings.HasPrefix(txt, q) && strings.HasSuffix(txt, q) && len(txt) >= 2*len(q) {
			return txt[len(q) : len(txt)-len(q)]
		}
	}
	return ""
}

// stringArg reads a named-or-positional string argument in one call.
func stringArg(args *sitter.Node, src []byte, label string, pos int) string {
	if v := namedArgOf(args, src, label); v != nil {
		return stringLiteralValue(v, src)
	}
	if pos >= 0 {
		return stringLiteralValue(positionalArg(args, pos), src)
	}
	return ""
}
