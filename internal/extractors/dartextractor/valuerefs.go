package dartextractor

import (
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// Module initializers need a value-only traversal. Function bodies collect the
// same tear-off references directly in walkNode.
func (w *walker) valueReferences(root *sitter.Node, shadows map[string]bool, b *bodyWalk) {
	if root == nil {
		return
	}
	if dartLexicalScope(root) {
		local := map[string]bool{}
		for name, bound := range shadows {
			local[name] = bound
		}
		dartBoundNames(root, w.src, local)
		if kindOf(root) == "local_function_declaration" {
			if params := firstOfKind(root, "formal_parameter_list"); params != nil {
				dartBoundNames(params, w.src, local)
			}
		}
		shadows = local
	}
	if kindOf(root) == "identifier" {
		w.recordValueReference(root, root.Utf8Text(w.src), shadows, b)
	}
	kids := namedChildren(root)
	for i, c := range kids {
		scope := shadows
		if kindOf(root) == "try_statement" && kindOf(c) == "block" && i > 0 && kindOf(kids[i-1]) == "catch_clause" {
			scope = map[string]bool{}
			for name, bound := range shadows {
				scope[name] = bound
			}
			dartBoundNames(kids[i-1], w.src, scope)
		}
		w.valueReferences(c, scope, b)
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
	for _, n := range namedChildren(body) {
		if kindOf(n) != "method_signature" && kindOf(n) != "declaration" {
			continue
		}
		if sig := firstOfKind(n, "function_signature"); sig != nil {
			if name := signatureName(sig, w.src); name != "" {
				values[name] = w.qualify(typeName + "." + name)
			}
		}
		if sig := childOfKind(n, "constructor_signature"); sig != nil && !dartConstructorSignature(sig, typeName, w.src) {
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
					values[name] = ""
				}
			case "getter_signature", "setter_signature":
				name := signatureName(c, w.src)
				if kindOf(c) == "getter_signature" {
					w.getterNames[name] = true
				}
				values[name] = ""
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

// Collect bindings belonging to one lexical scope. Nested scopes are entered by
// the body/value walkers, so their parameters and locals cannot hide outer names.
func dartBoundNames(root *sitter.Node, src []byte, out map[string]bool, receivers ...*dartReceiverBindings) {
	if root == nil {
		return
	}
	kind := kindOf(root)

	switch kind {
	case "formal_parameter", "initialized_variable_definition", "function_signature", "for_loop_parts", "variable_pattern":
		if kind == "formal_parameter" {
			if cp := childOfKind(root, "constructor_param"); cp != nil && childOfKind(cp, "this") != nil {
				if name := childOfKind(cp, "identifier"); name != nil {
					key := name.Utf8Text(src)
					out[key] = true
					if len(receivers) > 0 {
						b := receivers[0]
						first := !b.seen[key]
						b.record(key, declaredReceiverType(root, src))
						if first {
							if b.fieldFormals == nil {
								b.fieldFormals = map[string]bool{}
							}
							b.fieldFormals[key] = true
						}
					}
					break
				}
			}
		}
		name := root.ChildByFieldName("name")
		explicitName := name != nil
		if name == nil && kind == "formal_parameter" {
			name = childOfKind(root, "identifier")
		}
		if name != nil {
			key := name.Utf8Text(src)
			out[key] = true
			if explicitName && len(receivers) > 0 && (kind == "formal_parameter" || kind == "initialized_variable_definition" || kind == "for_loop_parts") {
				typ := declaredReceiverType(root, src)
				if typ == "" && kind == "initialized_variable_definition" {
					typ = dartConstructedBinding(root, src)
				}
				receivers[0].record(key, typ)
			}
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
		if dartLexicalScope(c) || kind == "function_signature" {
			if kindOf(c) == "local_function_declaration" {
				if sig := firstOfKind(c, "function_signature"); sig != nil {
					if name := signatureName(sig, src); name != "" {
						out[name] = true
					}
				}
			}
			continue
		}
		dartBoundNames(c, src, out, receivers...)
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

// Blocks, closures and loop/catch bindings have their own visibility region.
func dartLexicalScope(n *sitter.Node) bool {
	switch kindOf(n) {
	case "block", "function_expression", "for_statement", "catch_clause", "local_function_declaration":
		return true
	}
	return false
}

type dartScopeRestore struct {
	name              string
	shadow, hadShadow bool
	typ               string
	hadType           bool
}

func (w *walker) enterDartScope(n *sitter.Node, b *bodyWalk) func() {
	local := map[string]bool{}
	bindings := &dartReceiverBindings{types: map[string]string{}, seen: map[string]bool{}}
	dartBoundNames(n, w.src, local, bindings)
	if kindOf(n) == "local_function_declaration" {
		if params := firstOfKind(n, "formal_parameter_list"); params != nil {
			dartBoundNames(params, w.src, local, bindings)
		}
	}
	// Only bindings introduced by this scope need saving. The outer maps stay
	// in place, including blank type entries that deliberately hide field types.
	saved := make([]dartScopeRestore, 0, len(local))
	for name := range local {
		shadow, hadShadow := b.shadows[name]
		typ, hadType := b.receiverTypes[name]
		saved = append(saved, dartScopeRestore{name, shadow, hadShadow, typ, hadType})
		b.shadows[name] = true
		b.receiverTypes[name] = bindings.types[name]
	}
	return func() {
		for _, old := range saved {
			if old.hadShadow {
				b.shadows[old.name] = old.shadow
			} else {
				delete(b.shadows, old.name)
			}
			if old.hadType {
				b.receiverTypes[old.name] = old.typ
			} else {
				delete(b.receiverTypes, old.name)
			}
		}
	}
}

func (w *walker) recordValueReference(n *sitter.Node, name string, shadows map[string]bool, b *bodyWalk) {
	if target := w.callableValue(name); target != "" && !shadows[name] && dartValuePosition(n) {
		b.addRelation(facts.RelNames, target)
	}
}

// Class members overlay the file's immutable callable index. Empty entries hide
// a file function when a field/getter has the same spelling.
func (w *walker) callableValue(name string) string {
	if value, ok := w.memberFunctions[name]; ok {
		return value
	}
	return w.valueFunctions[name]
}

// Retain a constructor-shaped initializer as an expression receiver. The
// resolver only supplies its type after verifying the actual constructor call.
func dartConstructedBinding(n *sitter.Node, src []byte) string {
	value := n.ChildByFieldName("value")
	if value == nil {
		return ""
	}
	if kindOf(value) == "new_expression" || kindOf(value) == "const_object_expression" {
		typ := declaredReceiverType(value, src)
		if typ == "" {
			return ""
		}
		prefix := "new "
		if kindOf(value) == "const_object_expression" {
			prefix = "const "
		}
		return prefix + typ + "()"
	}
	if kindOf(value) != "identifier" {
		return ""
	}
	name := value.Utf8Text(src)
	if !namesAType(name) {
		return ""
	}
	for next := value.NextNamedSibling(); next != nil; next = next.NextNamedSibling() {
		if kindOf(next) == "type_arguments" {
			continue
		}
		if kindOf(next) != "selector" {
			return ""
		}
		if childOfKind(next, "argument_part") != nil {
			if next.NextNamedSibling() != nil {
				return ""
			}
			return name + "()"
		}
		sel := childOfKind(next, "unconditional_assignable_selector")
		member := identifierChild(sel, src)
		if member == "" {
			return ""
		}
		name += "." + member
	}
	return ""
}
