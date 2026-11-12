package dartextractor

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// Receiver types are taken from declarations, never from a globally distinctive
// method name. Unknown and multiply bound locals deliberately hide field types.
type dartReceiverBindings struct {
	types        map[string]string
	seen         map[string]bool
	fieldFormals map[string]bool
}

func (b *dartReceiverBindings) record(name, typ string) {
	delete(b.fieldFormals, name)
	if b.seen[name] {
		typ = ""
	}
	b.seen[name] = true
	b.types[name] = typ
}

func (w *walker) bodyReceiverTypes(signature *sitter.Node, shadows map[string]bool, bindings *dartReceiverBindings) (map[string]string, map[string]bool) {
	out := map[string]string{}
	for name := range shadows {
		out[name] = ""
	}
	for name, typ := range bindings.types {
		if typ == "" && bindings.fieldFormals[name] {
			typ = w.fieldTypes[name]
		}
		out[name] = typ
	}

	// A type parameter is not a repository-wide class with the same spelling.
	generic := map[string]bool{}
	for node := signature; node != nil; node = node.Parent() {
		// Program and class-body children can contain thousands of declarations.
		// Only declaration headers can introduce type parameters for this scope.
		switch kindOf(node) {
		case "function_signature", "class_definition", "mixin_declaration", "extension_declaration", "extension_type_declaration", "enum_declaration":
		default:
			continue
		}
		if params := childOfKind(node, "type_parameters"); params != nil {
			for _, param := range namedChildren(params) {
				name := param.ChildByFieldName("name")
				if name == nil {
					name = childOfKind(param, "type_identifier", "identifier")
				}
				if name != nil {
					generic[name.Utf8Text(w.src)] = true
				}
			}
		}
	}
	for name, typ := range out {
		if generic[typ] {
			out[name] = ""
		}
	}
	return out, generic
}

func (w *walker) memberReceiverTypes(body *sitter.Node) map[string]string {
	out := map[string]string{}
	for _, n := range namedChildren(body) {
		if kindOf(n) != "declaration" {
			continue
		}
		typ := declaredReceiverType(n, w.src)
		for _, c := range namedChildren(n) {
			if kindOf(c) == "initialized_identifier_list" || kindOf(c) == "static_final_declaration_list" {
				for _, name := range identifierNames(c, w.src) {
					out[name] = typ
				}
			}
		}
	}
	return out
}

// callReceiver retains the selector path, including empty argument markers for
// intermediate calls. A return value's type cannot be assumed to equal its base.
// Argument contents never become part of a target name.
func (w *walker) callReceiver(kids []*sitter.Node, i int) (string, bool) {
	memberIndex := i - 1
	if memberIndex >= 0 && kindOf(kids[memberIndex]) == "type_arguments" {
		memberIndex--
	}
	if memberIndex < 1 || (kindOf(kids[memberIndex]) != "selector" && kindOf(kids[memberIndex]) != "unconditional_assignable_selector" && kindOf(kids[memberIndex]) != "conditional_assignable_selector") {
		return "", false
	}
	start := memberIndex - 1
	for start >= 0 && (kindOf(kids[start]) == "selector" || kindOf(kids[start]) == "type_arguments" || kindOf(kids[start]) == "unconditional_assignable_selector" || kindOf(kids[start]) == "conditional_assignable_selector") {
		start--
	}
	if start < 0 {
		return "<expression>", false
	}
	base := strings.TrimSpace(kids[start].Utf8Text(w.src))
	switch kindOf(kids[start]) {
	case "parenthesized_expression", "unary_expression", "await_expression":
		base = w.expressionReceiver(kids[start])
	case "new_expression", "const_object_expression":
		if typ := declaredReceiverType(kids[start], w.src); typ != "" {
			prefix := "new "
			if kindOf(kids[start]) == "const_object_expression" {
				prefix = "const "
			}
			base = prefix + typ + "()"
		} else {
			base = "<expression>"
		}
	default:
		if !isPlainIdentifier(base) {
			base = "<expression>"
		}
	}
	direct := true
	for j := start + 1; j < memberIndex; j++ {
		if kindOf(kids[j]) == "type_arguments" || (kids[j].EndByte()-kids[j].StartByte() == 1 && w.src[kids[j].StartByte()] == '!') {
			continue
		}
		if childOfKind(kids[j], "argument_part") != nil {
			base += "()"
			direct = false
			continue
		}
		sel := childOfKind(kids[j], "unconditional_assignable_selector", "conditional_assignable_selector")
		if kindOf(kids[j]) == "unconditional_assignable_selector" || kindOf(kids[j]) == "conditional_assignable_selector" {
			sel = kids[j]
		}
		if sel == nil {
			base += "[]"
			direct = false
			continue
		}
		name := identifierChild(sel, w.src)
		if base == "this" {
			base += "." + name
		} else {
			base += "." + name
			direct = false
		}
	}
	return base, direct
}

func (w *walker) scopedCallTarget(receiver string, direct bool, name string, b *bodyWalk) string {
	if receiver == "" {
		if b.shadows[name] {
			return "(" + name + ").call"
		}
		if _, field := w.fieldTypes[name]; field {
			return "(" + name + ").call"
		}
		if w.callOwner != "" {
			target := "this." + name
			b.implicitCalls[target] = true
			b.instanceCalls[target] = true
			return target
		}
		if target := w.callableValue(name); target != "" {
			return "@lexical:" + target
		}
		return name
	}

	// A value-bound prefix is not an import/type namespace, even in chains.
	root := receiver
	if cut := strings.IndexAny(root, ".("); cut >= 0 {
		root = root[:cut]
	}
	_, field := w.fieldTypes[root]
	knownFunction := w.callableValue(root) != ""
	if typ := b.receiverType(w, root); strings.HasPrefix(typ, "@value:") {
		target := typ + receiver[len(root):] + "." + name
		b.instanceCalls[target] = true
		return target
	}
	if !direct && b.shadows[root] {
		if typ := b.writtenReceiverType(w, root); typ != "" && typ != "dynamic" {
			target := "@type{" + typ + "}" + receiver[len(root):] + "." + name
			b.instanceCalls[target] = true
			return target
		}
	}
	if direct && name == "call" && knownFunction && !b.shadows[root] && !field && receiver == root {
		target := w.callableValue(root)
		return "@lexical:" + target
	}
	if !b.shadows[root] && (field || knownFunction || w.getterNames[root]) && (!direct || b.receiverType(w, root) == "") {
		target := "@expr:" + receiver + "." + name
		b.instanceCalls[target] = true
		return target
	}
	if !direct && (b.shadows[root] || field || knownFunction || w.getterNames[root]) {
		return "(" + root + ")" + receiver[len(root):] + "." + name
	}
	if direct {
		if receiver == "this" {
			if w.callOwner != "" {
				target := "this." + name
				b.instanceCalls[target] = true
				return target
			}
		} else {
			typ := b.receiverType(w, receiver)
			if typ != "" && typ != "dynamic" {
				target := typ + "." + name
				b.instanceCalls[target] = true
				return target
			}
			// A known value binding must not be reinterpreted as a same-named type.
			// Parentheses retain a Dart expression receiver without inventing a type.
			if b.shadows[receiver] {
				return "(" + receiver + ")." + name
			}
			if _, field := w.fieldTypes[receiver]; field {
				return "(" + receiver + ")." + name
			}
			if knownFunction || w.getterNames[root] {
				return "(" + receiver + ")." + name
			}

		}
	}
	return receiver + "." + name
}

// The grammar represents an import prefix and its type as separate identifiers.
func declaredReceiverType(n *sitter.Node, src []byte) string {
	typ := ""
	for _, c := range namedChildren(n) {
		if kindOf(c) == "type_identifier" {
			if typ != "" {
				typ += "."
			}
			typ += c.Utf8Text(src)
		}
	}
	return typ
}

type dartGetterRead struct{ Loop, Scaling bool }

func (w *walker) recordGetterRead(target string, node *sitter.Node, b *bodyWalk, loop, scaling int) {
	// The final member of a simple assignment is written, not read. Earlier
	// receiver getters still execute (e.g. object.value.field = next).
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if kindOf(parent) == "assignment_expression" {
			if op := childOfKind(parent, "assignment_operator"); op != nil && strings.TrimSpace(op.Utf8Text(w.src)) == "=" {
				if left := parent.NamedChild(0); left != nil && node.EndByte() == left.EndByte() {
					return
				}
			}
		}
		if kindOf(parent) == "function_body" {
			break
		}
	}
	if b.getterReads == nil {
		b.getterReads = map[string]dartGetterRead{}
	}
	read := b.getterReads[target]
	read.Loop = read.Loop || loop > 0
	read.Scaling = read.Scaling || scaling > 0
	b.getterReads[target] = read
}

// Retain the object path preceding the first invocation, rather than treating
// a builder/query result as having no I/O provenance. This preserves this._db
// and FirebaseFirestore.instance without reading argument contents.
func dartIORoot(receiver string) string {
	if invoke := strings.Index(receiver, "()"); invoke >= 0 {
		receiver = receiver[:invoke]
		if dot := strings.LastIndexByte(receiver, '.'); dot >= 0 {
			receiver = receiver[:dot]
		}
	}
	return receiver
}

// Recover receiver provenance from grouping/await without reading argument text
// or assuming the result has the type of its input.
func (w *walker) expressionReceiver(n *sitter.Node) string {
	kids := namedChildren(n)
	if len(kids) == 1 {
		return w.expressionReceiver(kids[0])
	}
	if kindOf(n) == "identifier" || kindOf(n) == "this" || kindOf(n) == "super" {
		return n.Utf8Text(w.src)
	}
	if len(kids) == 0 {
		return "<expression>"
	}
	base := w.expressionReceiver(kids[0])
	for _, c := range kids[1:] {
		if kindOf(c) == "type_arguments" || kindOf(c) == "cascade_section" {
			continue
		}
		if childOfKind(c, "argument_part") != nil {
			base += "()"
			continue
		}
		sel := childOfKind(c, "unconditional_assignable_selector", "conditional_assignable_selector")
		if sel == nil {
			return "<expression>"
		}
		name := identifierChild(sel, w.src)
		if name == "" {
			return "<expression>"
		}
		base += "." + name
	}
	return base
}

// Locals are an overlay; the class field index is shared by every method and
// nested scope. A blank local deliberately hides a typed field.
func (b *bodyWalk) receiverType(w *walker, receiver string) string {
	typ := b.writtenReceiverType(w, receiver)
	if strings.HasPrefix(typ, "@") {
		return typ
	}
	return dartBaseType(typ)
}

func (b *bodyWalk) writtenReceiverType(w *walker, receiver string) string {
	typ, local := b.receiverTypes[receiver]
	if !local {
		field := strings.TrimPrefix(receiver, "this.")
		typ = w.fieldTypes[field]
	}
	if !local && b.receiverGenerics[typ] {
		return ""
	}
	return typ
}
