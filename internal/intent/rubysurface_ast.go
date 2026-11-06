package intent

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// The small reading layer under the Ruby declaration surface. Each helper
// answers one question about a node and nothing else, because a declaration
// file must be understood exactly or refused: a helper that guesses is a
// helper that lets a mistyped law compile into a law nobody wrote.

// methodName is the method a statement calls, whether it was written with a
// receiver, with parentheses, or as a bare word.
func (r *surfaceReader) methodName(node *sitter.Node) string {
	if node == nil {
		return ""
	}
	switch node.Kind() {
	case "identifier":
		return r.text(node)
	case "call", "method_call":
		if m := node.ChildByFieldName("method"); m != nil {
			return r.text(m)
		}
	}
	return ""
}

// receiverName is the part a law's sentence is about: the receiver of a call,
// when it is a bare name rather than an expression.
func (r *surfaceReader) receiverName(node *sitter.Node) string {
	if node == nil || (node.Kind() != "call" && node.Kind() != "method_call") {
		return ""
	}
	recv := node.ChildByFieldName("receiver")
	if recv == nil || recv.Kind() != "identifier" {
		return ""
	}
	return r.text(recv)
}

// blockBody is the body of the do/end (or brace) block a call carries.
func blockBody(node *sitter.Node) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		switch child.Kind() {
		case "do_block", "block":
			for j := uint(0); j < child.NamedChildCount(); j++ {
				if inner := child.NamedChild(j); inner.Kind() == "body_statement" {
					return inner
				}
			}
			return child
		}
	}
	return nil
}

// argumentNodes are a call's arguments, with or without parentheses.
func argumentNodes(node *sitter.Node) []*sitter.Node {
	if node == nil {
		return nil
	}
	args := node.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	out := make([]*sitter.Node, 0, args.NamedChildCount())
	for i := uint(0); i < args.NamedChildCount(); i++ {
		out = append(out, args.NamedChild(i))
	}
	return out
}

type keywordPair struct{ key, value *sitter.Node }

// keywordPairs are the `key: value` arguments of a call, in written order.
func (r *surfaceReader) keywordPairs(args []*sitter.Node) []keywordPair {
	var out []keywordPair
	for _, arg := range args {
		switch arg.Kind() {
		case "pair":
			out = append(out, keywordPair{key: arg.ChildByFieldName("key"), value: arg.ChildByFieldName("value")})
		case "hash":
			for i := uint(0); i < arg.NamedChildCount(); i++ {
				if pair := arg.NamedChild(i); pair.Kind() == "pair" {
					out = append(out, keywordPair{key: pair.ChildByFieldName("key"), value: pair.ChildByFieldName("value")})
				}
			}
		}
	}
	return out
}

// symbolOrString reads a symbol, a string or a bare name as its text, which is
// how every name in this surface is written.
func (r *surfaceReader) symbolOrString(node *sitter.Node) string {
	if node == nil {
		return ""
	}
	switch node.Kind() {
	case "simple_symbol":
		return strings.TrimPrefix(r.text(node), ":")
	case "hash_key_symbol", "identifier", "constant", "integer":
		return r.text(node)
	case "string", "bare_string":
		return stringContent(r.text(node))
	case "string_content":
		return r.text(node)
	case "pair":
		return ""
	}
	// A string with interpolation, or anything else, has no single literal
	// value: refusing is what keeps a law from meaning something at load time
	// that it does not say on the page.
	return ""
}

func stringContent(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) >= 2 {
		first, last := trimmed[0], trimmed[len(trimmed)-1]
		if (first == '"' || first == '\'') && last == first {
			return trimmed[1 : len(trimmed)-1]
		}
	}
	return trimmed
}

// stringList reads one string or an array of them, so `files:` takes either.
func (r *surfaceReader) stringList(node *sitter.Node) []string {
	if node == nil {
		return nil
	}
	if node.Kind() == "array" {
		var out []string
		for i := uint(0); i < node.NamedChildCount(); i++ {
			if v := r.symbolOrString(node.NamedChild(i)); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	if v := r.symbolOrString(node); v != "" {
		return []string{v}
	}
	return nil
}

// hash reads a `where:` predicate: property names against literal values.
func (r *surfaceReader) hash(node *sitter.Node) map[string]any {
	if node == nil || node.Kind() != "hash" {
		return nil
	}
	out := map[string]any{}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		pair := node.NamedChild(i)
		if pair.Kind() != "pair" {
			continue
		}
		key := r.symbolOrString(pair.ChildByFieldName("key"))
		value := r.symbolOrString(pair.ChildByFieldName("value"))
		if key != "" && value != "" {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// partList reads the parts a law names on the other side of an edge: one bare
// name, or several.
func (r *surfaceReader) partList(args []*sitter.Node) []string {
	var out []string
	for _, arg := range args {
		if arg.Kind() == "pair" || arg.Kind() == "hash" {
			continue
		}
		if arg.Kind() == "array" {
			out = append(out, r.stringList(arg)...)
			continue
		}
		if name := r.symbolOrString(arg); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func firstOrNil(args []*sitter.Node) *sitter.Node {
	for _, arg := range args {
		if arg.Kind() != "pair" && arg.Kind() != "hash" {
			return arg
		}
	}
	return nil
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// literalList reads the string arguments of a law's sentence, which are how a
// far end or an antecedent names something the graph recorded rather than
// something this declaration selected. A bare name is a part and is not read
// here.
func (r *surfaceReader) literalList(args []*sitter.Node) []string {
	var out []string
	for _, arg := range args {
		switch arg.Kind() {
		case "string", "bare_string":
			if v := r.symbolOrString(arg); v != "" {
				out = append(out, v)
			}
		case "array":
			for i := uint(0); i < arg.NamedChildCount(); i++ {
				child := arg.NamedChild(i)
				if child.Kind() == "string" || child.Kind() == "bare_string" {
					if v := r.symbolOrString(child); v != "" {
						out = append(out, v)
					}
				}
			}
		}
	}
	return out
}
