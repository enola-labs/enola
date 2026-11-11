package dartextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

type dartLocalValue struct {
	Name       string
	Expression string
}

type dartValueDeclaration struct {
	Owner      string `json:"owner,omitempty"`
	Name       string `json:"name"`
	Type       string `json:"type,omitempty"`
	Expression string `json:"expression,omitempty"`
	Static     bool   `json:"static,omitempty"`
}

func dartWrittenType(n *sitter.Node, src []byte) string {
	var first, last *sitter.Node
	for _, c := range namedChildren(n) {
		switch kindOf(c) {
		case "type_identifier", "type_arguments", "nullable_type":
			if first == nil {
				first = c
			}
			last = c
		default:
			if first != nil {
				return strings.TrimSpace(string(src[first.StartByte():last.EndByte()]))
			}
		}
	}
	if first != nil {
		return strings.TrimSpace(string(src[first.StartByte():last.EndByte()]))
	}
	return ""
}

// Normalize only expression structure needed for type propagation. Argument
// contents, arbitrary operators, and executable bodies never enter this model.
func dartValueExpression(nodes []*sitter.Node, src []byte) string {
	if len(nodes) == 0 {
		return ""
	}
	n := nodes[0]
	base := ""
	switch kindOf(n) {
	case "identifier", "this", "super":
		base = n.Utf8Text(src)
	case "new_expression", "const_object_expression":
		typ := declaredReceiverType(n, src)
		if typ == "" {
			return ""
		}
		base = "new " + typ + "()"
	case "parenthesized_expression", "unary_expression", "await_expression":
		if kindOf(n) == "unary_expression" && childOfKind(n, "await_expression") == nil {
			return ""
		}
		base = dartValueExpression(namedChildren(n), src)
		if base == "" {
			return ""
		}
		if kindOf(n) == "await_expression" {
			base = "await " + base
		}
	case "string_literal":
		base = "@type{String}"
	case "decimal_integer_literal", "hex_integer_literal":
		base = "@type{int}"
	case "decimal_floating_point_literal":
		base = "@type{double}"
	case "true", "false":
		base = "@type{bool}"
	default:
		return ""
	}
	for _, c := range nodes[1:] {
		if kindOf(c) == "type_arguments" || kindOf(c) == "cascade_section" {
			continue
		}
		if c.EndByte()-c.StartByte() == 1 && src[c.StartByte()] == '!' {
			continue
		}
		if kindOf(c) != "selector" && kindOf(c) != "unconditional_assignable_selector" && kindOf(c) != "conditional_assignable_selector" {
			return ""
		}
		if childOfKind(c, "argument_part") != nil {
			base += "()"
			continue
		}
		sel := childOfKind(c, "unconditional_assignable_selector", "conditional_assignable_selector")
		if kindOf(c) != "selector" {
			sel = c
		}
		name := identifierChild(sel, src)
		if name == "" {
			return ""
		}
		base += "." + name
	}
	return base
}
func dartInitializerExpression(n *sitter.Node, src []byte) string {
	value := n.ChildByFieldName("value")
	if value == nil {
		return ""
	}
	var nodes []*sitter.Node
	for next := value; next != nil; next = next.NextNamedSibling() {
		nodes = append(nodes, next)
	}
	return dartValueExpression(nodes, src)
}
func (w *walker) recordValueDeclaration(n *sitter.Node, owner string, written ...string) {
	typ := dartWrittenType(n, w.src)
	if len(written) > 0 && written[0] != "" {
		typ = written[0]
	}
	var visit func(*sitter.Node)
	visit = func(node *sitter.Node) {
		switch kindOf(node) {
		case "initialized_variable_definition", "initialized_identifier", "static_final_declaration":
			id := node.ChildByFieldName("name")
			if id == nil {
				id = childOfKind(node, "identifier")
			}
			if id == nil {
				return
			}
			expression := dartInitializerExpression(node, w.src)
			if expression == "" { // Lists use an equals token rather than a value field.
				kids := namedChildren(node)
				for i, c := range kids {
					if c.StartByte() == id.StartByte() && i+1 < len(kids) {
						expression = dartValueExpression(kids[i+1:], w.src)
						break
					}
				}
			}
			w.valueDeclarations = append(w.valueDeclarations, dartValueDeclaration{owner, id.Utf8Text(w.src), typ, expression, owner == "" || hasToken(n, "static")})
			return
		}
		for _, c := range namedChildren(node) {
			if kindOf(c) != "function_body" {
				visit(c)
			}
		}
	}
	visit(n)
}
func (w *walker) expressionForBinding(expr string, b *bodyWalk) string {
	prefix := ""
	if strings.HasPrefix(expr, "await ") {
		prefix = "await "
		expr = strings.TrimPrefix(expr, "await ")
	}
	if strings.HasPrefix(expr, "new ") || strings.HasPrefix(expr, "@") {
		return prefix + expr
	}
	end := strings.IndexAny(expr, ".(")
	if end < 0 {
		end = len(expr)
	}
	root := expr[:end]
	if b.shadows[root] {
		typ := b.receiverTypes[root]
		if strings.HasPrefix(typ, "@value:") {
			return prefix + typ + expr[end:]
		}
		if typ != "" && typ != "dynamic" {
			return prefix + "@type{" + typ + "}" + expr[end:]
		}
		return "@unknown{" + root + "}"
	}
	return prefix + expr
}
func (b *bodyWalk) allocateValueBinding(name, expr string) string {
	id := "@value:" + itoa(len(b.valueBindings))
	b.valueBindings[id] = dartLocalValue{name, expr}
	return id
}

// Use decimal IDs without adding source-specific strings to the graph.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func (w *walker) emitValueDeclarations() {
	if len(w.valueDeclarations) > 0 {
		values := make([]any, 0, len(w.valueDeclarations))
		for _, decl := range w.valueDeclarations {
			item := map[string]any{"name": decl.Name}
			if decl.Owner != "" {
				item["owner"] = decl.Owner
			}
			if decl.Type != "" {
				item["type"] = decl.Type
			}
			if decl.Expression != "" {
				item["expression"] = decl.Expression
			}
			if decl.Static {
				item["static"] = true
			}
			values = append(values, item)
		}
		w.mergeFileReference(map[string]any{"dart_values": values}, nil)
	}
}
func (w *walker) mergeFileReference(props map[string]any, relations []facts.Relation) {
	for i := range w.out {
		f := &w.out[i]
		if f.Kind == facts.KindFileRef && f.Name == w.relFile && f.File == w.relFile && f.Line == 0 {
			if f.Props == nil {
				f.Props = map[string]any{"language": "dart"}
			}
			for k, v := range props {
				f.Props[k] = v
			}
			f.Relations = append(f.Relations, relations...)
			return
		}
	}
	if props == nil {
		props = map[string]any{}
	}
	props["language"] = "dart"
	w.out = append(w.out, facts.Fact{Kind: facts.KindFileRef, Name: w.relFile, File: w.relFile, Props: props, Relations: relations})
}

func dartTypeParameterNames(n *sitter.Node, src []byte) []string {
	var out []string
	if params := childOfKind(n, "type_parameters"); params != nil {
		for _, param := range namedChildren(params) {
			id := param.ChildByFieldName("name")
			if id == nil {
				id = childOfKind(param, "type_identifier", "identifier")
			}
			if id != nil {
				out = append(out, id.Utf8Text(src))
			}
		}
	}
	return out
}

func dartGlobalWrittenType(kids []*sitter.Node, index int, src []byte) string {
	end := index - 1
	start := end
	for start >= 0 {
		switch kindOf(kids[start]) {
		case "type_identifier", "type_arguments", "nullable_type":
			start--
		default:
			goto done
		}
	}
done:
	start++
	if start > end {
		return ""
	}
	return strings.TrimSpace(string(src[kids[start].StartByte():kids[end].EndByte()]))
}
func (w *walker) returnExpression(signature, body *sitter.Node) string {
	if childOfKind(signature, "void_type") != nil {
		return ""
	}
	expr := dartValueExpression(namedChildren(body), w.src)
	if expr == "" {
		return ""
	}
	shadows := map[string]bool{}
	bindings := &dartReceiverBindings{types: map[string]string{}, seen: map[string]bool{}}
	if params := firstOfKind(signature, "formal_parameter_list"); params != nil {
		dartBoundNames(params, w.src, shadows, bindings)
	}
	return w.expressionForBinding(expr, &bodyWalk{shadows: shadows, receiverTypes: bindings.types})
}
