package dartextractor

import (
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// Tear-offs read a callable as a value without invoking it. Keep them as names
// edges: they establish usage without claiming a call or propagating I/O.
func (w *walker) valueReferences(root *sitter.Node, shadows map[string]bool, b *bodyWalk) {
	if root == nil {
		return
	}
	if kindOf(root) == "identifier" {
		name := root.Utf8Text(w.src)
		if target := w.valueFunctions[name]; target != "" && !shadows[name] && dartValuePosition(root) {
			b.addRelation(facts.RelNames, target)
		}
	}
	for _, c := range namedChildren(root) {
		w.valueReferences(c, shadows, b)
	}
}

// Resolve references from declarations in the current file, rather than from a
// globally unique short name. Imported and inherited callable values need their
// library/type scope established first and are deliberately left unclaimed.
func (w *walker) fileCallableValues(root *sitter.Node) map[string]string {
	values := map[string]string{}
	for _, n := range namedChildren(root) {
		if kindOf(n) == "function_signature" {
			if name := signatureName(n, w.src); name != "" {
				values[name] = w.qualify(name)
			}
		}
	}
	return values
}

func (w *walker) memberCallableValues(body *sitter.Node, typeName string) map[string]string {
	values := map[string]string{}
	for name, target := range w.valueFunctions {
		values[name] = target
	}
	for _, n := range namedChildren(body) {
		if kindOf(n) != "method_signature" && kindOf(n) != "declaration" {
			continue
		}
		if sig := firstOfKind(n, "function_signature"); sig != nil {
			if name := signatureName(sig, w.src); name != "" {
				values[name] = w.qualify(typeName + "." + name)
			}
		}
		// A field or getter shadows a same-named top-level function. It is data,
		// even when an unrelated class elsewhere declares a callable with its name.
		for _, c := range namedChildren(n) {
			switch kindOf(c) {
			case "initialized_identifier_list", "static_final_declaration_list":
				for _, name := range identifierNames(c, w.src) {
					delete(values, name)
				}
			case "getter_signature", "setter_signature":
				delete(values, signatureName(c, w.src))
			}
		}
	}
	return values
}

func dartValuePosition(n *sitter.Node) bool {
	p := n.Parent()
	if p == nil {
		return false
	}
	// A selector's base is a receiver, not a bare callback value. Invocations
	// already have calls edges from scanCallChain.
	if next := n.NextNamedSibling(); kindOf(next) == "selector" {
		return false
	}
	switch kindOf(p) {
	case "argument", "named_argument", "return_statement", "function_body", "function_expression_body",
		"list_literal", "set_or_map_literal", "switch_expression_case", "conditional_expression":
		return true
	case "pair", "initialized_variable_definition":
		value := p.ChildByFieldName("value")
		return value != nil && value.StartByte() == n.StartByte()
	case "initialized_identifier":
		first := childOfKind(p, "identifier")
		return first != nil && first.StartByte() != n.StartByte()
	}
	return false
}

// As in the TypeScript walker, a name bound anywhere in the function shadows
// the module's name. This deliberately declines ambiguous references.
func dartBoundNames(root *sitter.Node, src []byte, out map[string]bool) {
	if root == nil {
		return
	}
	switch kindOf(root) {
	case "formal_parameter", "initialized_variable_definition", "function_signature", "for_loop_parts", "variable_pattern":
		name := root.ChildByFieldName("name")
		if name == nil && kindOf(root) == "formal_parameter" {
			name = childOfKind(root, "identifier")
		}
		if name != nil {
			out[name.Utf8Text(src)] = true
		}
	case "initialized_identifier", "static_final_declaration":
		if name := childOfKind(root, "identifier"); name != nil {
			out[name.Utf8Text(src)] = true
		}
	case "catch_parameters":
		for _, n := range namedChildren(root) {
			if kindOf(n) == "identifier" {
				out[n.Utf8Text(src)] = true
			}
		}
	}
	for _, c := range namedChildren(root) {
		dartBoundNames(c, src, out)
	}
}

// Module initializers have no enclosing function, but still use callback values.
func (w *walker) moduleValueReferences(root *sitter.Node) {
	b := &bodyWalk{seenRel: map[string]bool{}}
	for _, n := range namedChildren(root) {
		switch kindOf(n) {
		case "initialized_variable_definition", "static_final_declaration_list", "initialized_identifier_list":
			shadows := map[string]bool{}
			dartBoundNames(n, w.src, shadows)
			w.valueReferences(n, shadows, b)
		}
	}
	if len(b.relations) > 0 {
		w.out = append(w.out, facts.Fact{Kind: facts.KindFileRef, Name: w.relFile, File: w.relFile,
			Props: map[string]any{"language": "dart"}, Relations: b.relations})
	}
}
