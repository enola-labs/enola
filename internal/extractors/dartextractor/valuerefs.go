package dartextractor

import (
	"maps"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
	"strings"
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
		name := root.Utf8Text(w.src)
		if dartInvocationTarget(root) || dartCallMethodTarget(root, w.src) {
			// File scope has no call scan, so the invocation is recorded here.
			// `f.call(...)` on a file function invokes f itself. The target is
			// already canonical; as a bare dotted name the resolution pass would
			// read its directory as a receiver and lose it.
			if target := w.callableValue(name); target != "" && !shadows[name] {
				b.addRelation(facts.RelCalls, "@lexical:"+target)
			} else if !shadows[name] && w.unboundName(name) && dartInvocationTarget(root) {
				b.addValueName(facts.RelCalls, name)
			}
		} else {
			w.recordValueReference(root, name, shadows, b)
		}
		w.recordUnboundValue(root, b.siteOf(root), name, shadows, b, 0, 0)
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
		b.site = dartSiteAt(c, root, kids, i)
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

func (w *walker) fileGetterNames(root *sitter.Node) map[string]bool {
	getters := map[string]bool{}
	for _, n := range namedChildren(root) {
		if kindOf(n) == "getter_signature" {
			if name := signatureName(n, w.src); name != "" {
				getters[name] = true
			}
		}
	}
	return getters
}

// recordUnboundValue records an unshadowed read of a top-level getter: this
// file's, or a name nothing in this file binds, whose declaration only the
// resolution pass can find in another part of the library or an import. A
// value-position name of the second kind is also kept as a tear-off candidate.
// A same-named class member hides both inside its class.
func (w *walker) recordUnboundValue(n *sitter.Node, site dartSite, name string, shadows map[string]bool, b *bodyWalk, loop, scaling int) {
	if shadows[name] {
		return
	}
	if _, member := w.memberFunctions[name]; member {
		return
	}
	unbound := w.unboundName(name)
	if w.assignedName(n, site.parent) {
		if unbound || w.fileGetters[name] {
			b.markSetterWrite(name)
		}
	} else if (w.fileGetters[name] || unbound) && dartReadKind(kindOf(site.parent)) {
		b.markGetterRead(name, loop, scaling)
	}
	next := kindOf(site.next)
	invoked := next == "selector" && childOfKind(site.next, "argument_part") != nil
	if unbound && !invoked && dartValuePositionAt(n, site.parent, site.next) {
		b.addValueName(facts.RelNames, name)
	}
}

// dartSite is a node's parent and next named sibling.
type dartSite struct{ node, parent, next *sitter.Node }

func dartSiteAt(c, parent *sitter.Node, kids []*sitter.Node, i int) dartSite {
	site := dartSite{node: c, parent: parent}
	if i+1 < len(kids) {
		site.next = kids[i+1]
	}
	return site
}

// siteOf returns the walk's record of n's site, or asks the tree when n was
// not reached through a child loop.
func (b *bodyWalk) siteOf(n *sitter.Node) dartSite {
	if b.site.node != nil && b.site.node.Id() == n.Id() {
		return b.site
	}
	return dartSite{node: n, parent: n.Parent(), next: n.NextNamedSibling()}
}

// assignedName reports a bare name written by a plain assignment, `name = v`.
func (w *walker) assignedName(n, parent *sitter.Node) bool {
	if kindOf(parent) != "assignable_expression" || parent.EndByte() != n.EndByte() {
		return false
	}
	assignment := parent.Parent()
	return kindOf(assignment) == "assignment_expression" && dartPlainAssignment(assignment, w.src) &&
		assignment.NamedChild(0) != nil && assignment.NamedChild(0).Id() == parent.Id()
}

func (b *bodyWalk) markSetterWrite(name string) {
	if b.setterWrites == nil {
		b.setterWrites = map[string]bool{}
	}
	b.setterWrites[name] = true
}

func (b *bodyWalk) addValueName(kind, name string) {
	if b.valueNames == nil {
		b.valueNames = map[string]bool{}
	}
	b.valueNames[kind+":"+name] = true
}

// unboundName reports a name this file declares nowhere it can see: not a
// file function or getter, nor a member, field or getter of the current class.
func (w *walker) unboundName(name string) bool {
	if w.fileGetters[name] || w.getterNames[name] || w.valueFunctions[name] != "" {
		return false
	}
	if _, field := w.fieldTypes[name]; field {
		return false
	}
	_, member := w.memberFunctions[name]
	return !member
}

// dartReadKind excludes a label, a cascade target and a member selector,
// none of which reads a top-level name.
func dartReadKind(parent string) bool {
	switch parent {
	case "label", "cascade_section", "cascade_selector":
		return false
	default:
		return !strings.HasSuffix(parent, "assignable_selector")
	}
}

// dartCallMethodTarget reports `name.call(...)`: the base of a `.call`
// selector that arguments follow.
func dartCallMethodTarget(n *sitter.Node, src []byte) bool {
	sel := n.NextNamedSibling()
	if kindOf(sel) != "selector" || childOfKind(sel, "argument_part") != nil {
		return false
	}
	if identifierChild(childOfKind(sel, "unconditional_assignable_selector"), src) != "call" {
		return false
	}
	args := sel.NextNamedSibling()
	return kindOf(args) == "selector" && childOfKind(args, "argument_part") != nil
}

// dartParameterDefaults returns the default value expressions of a signature's
// own parameters. They sit beside the formal_parameter nodes, not inside them.
func dartParameterDefaults(signature *sitter.Node) []*sitter.Node {
	params := firstOfKind(signature, "formal_parameter_list")
	var out []*sitter.Node
	for _, group := range namedChildren(params) {
		if kindOf(group) != "optional_formal_parameters" {
			continue
		}
		for _, c := range namedChildren(group) {
			switch kindOf(c) {
			case "formal_parameter", "annotation", "comment", "documentation_comment":
			default:
				out = append(out, c)
			}
		}
	}
	return out
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
				} else {
					w.setterNames[name] = true
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
	return dartValuePositionAt(n, p, n.NextNamedSibling())
}

func dartValuePositionAt(n, p, next *sitter.Node) bool {
	if p == nil {
		return false
	}
	// A selector's base is a receiver, not a bare callback value. Invocations in
	// function bodies have calls edges from scanCallChain; file-scope ones are
	// recorded by valueReferences.
	if kindOf(next) == "selector" {
		return false
	}
	switch kindOf(p) {
	case "argument", "named_argument", "return_statement", "function_body", "function_expression_body",
		"list_literal", "set_or_map_literal", "switch_expression_case", "conditional_expression",
		"if_null_expression", "optional_formal_parameters":
		return true
	case "pair", "initialized_variable_definition":
		value := p.ChildByFieldName("value")
		return value != nil && value.StartByte() == n.StartByte()
	case "initialized_identifier", "static_final_declaration":
		first := childOfKind(p, "identifier")
		return first != nil && first.StartByte() != n.StartByte()
	}
	return false
}

// dartInvocationTarget reports a bare name invoked directly: `name(...)` or
// `name<T>(...)`. The grammar has no call node, only the name followed by an
// argument selector.
func dartInvocationTarget(n *sitter.Node) bool {
	next := n.NextNamedSibling()
	return kindOf(next) == "selector" && childOfKind(next, "argument_part") != nil
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
				if written := dartWrittenType(root, src); typ != "" && strings.Contains(written, "<") && dartBaseType(written) == typ {
					typ = written
				}
				if typ == "" && kind == "initialized_variable_definition" {
					if expr := dartInitializerExpression(root, src); expr != "" {
						typ = "@infer:" + expr
					}
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

// Module initializers have no enclosing function, but still use callback values
// and build classes.
func (w *walker) moduleValueReferences(root *sitter.Node) {
	b := &bodyWalk{seenRel: map[string]bool{}}
	for _, n := range namedChildren(root) {
		switch kindOf(n) {
		case "initialized_variable_definition", "static_final_declaration_list", "initialized_identifier_list":
			shadows := map[string]bool{}
			dartBoundNames(n, w.src, shadows)
			w.valueReferences(n, shadows, b)
			// `final router = Router();` builds the class at file scope, with the
			// construction rule field initializers use.
			w.constructionReferences(n, shadows, b)
		}
	}
	w.emitInitializerWalk(b)
}

// emitInitializerWalk moves an initializer region's references onto the file's
// reference-only fact. Getter reads and tear-off candidates from several
// regions are unioned, since each region feeds the one fact.
func (w *walker) emitInitializerWalk(b *bodyWalk) {
	if len(b.relations) == 0 && len(b.getterReads) == 0 && len(b.valueNames) == 0 && len(b.setterWrites) == 0 {
		return
	}
	props := map[string]any{}
	if len(b.getterReads) > 0 || len(b.valueNames) > 0 || len(b.setterWrites) > 0 {
		reads := map[string]dartGetterRead{}
		names := map[string]bool{}
		writes := map[string]bool{}
		for i := range w.out {
			f := &w.out[i]
			if f.Kind == facts.KindFileRef && f.Name == w.relFile && f.File == w.relFile && f.Line == 0 {
				if prior, ok := f.Props["_dart_getter_reads"].(map[string]dartGetterRead); ok {
					maps.Copy(reads, prior)
				}
				if prior, ok := f.Props["_dart_value_names"].([]string); ok {
					for _, name := range prior {
						names[name] = true
					}
				}
				if prior, ok := f.Props["_dart_setter_writes"].([]string); ok {
					for _, name := range prior {
						writes[name] = true
					}
				}
			}
		}
		maps.Copy(reads, b.getterReads)
		maps.Copy(names, b.valueNames)
		maps.Copy(writes, b.setterWrites)
		if len(writes) > 0 {
			props["_dart_setter_writes"] = sortedKeys(writes)
		}
		if len(reads) > 0 {
			props["_dart_getter_reads"] = reads
		}
		if len(names) > 0 {
			props["_dart_value_names"] = sortedKeys(names)
		}
	}
	w.mergeFileReference(props, b.relations)
}

// Class-body field initializers are the third initializer region, beside
// constructor initializer lists (walked with the constructor) and file-scope
// variables (moduleValueReferences). A schema generator's `late final T t =
// T(columns: [_column_0])` names its top-level column builders only here. The
// walk runs while the class's member overlay is in force, so a field's own name
// and a sibling field still shadow a same-named function, and a method tear-off
// resolves to the member. Private fields have no symbol to carry the edge, so all
// of them go to the file's reference-only fact, as file-scope initializers do.
func (w *walker) fieldInitializerReferences(list *sitter.Node) {
	if w.fieldValues == nil {
		w.fieldValues = &bodyWalk{seenRel: map[string]bool{}}
	}
	shadows := map[string]bool{}
	dartBoundNames(list, w.src, shadows)
	w.valueReferences(list, shadows, w.fieldValues)
	// `late final _c = Controller()` is the class's only construction site as often
	// as a method body is.
	w.constructionReferences(list, shadows, w.fieldValues)
}

func (w *walker) emitFieldInitializerReferences() {
	if w.fieldValues != nil {
		w.emitInitializerWalk(w.fieldValues)
	}
	w.fieldValues = nil
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
	pending := map[string]string{}
	for name, typ := range bindings.types {
		if strings.HasPrefix(typ, "@infer:") {
			expr := strings.TrimPrefix(typ, "@infer:")
			id := b.allocateValueBinding(name, expr)
			bindings.types[name] = id
			pending[id] = expr
		}
	}
	for name := range local {
		shadow, hadShadow := b.shadows[name]
		typ, hadType := b.receiverTypes[name]
		saved = append(saved, dartScopeRestore{name, shadow, hadShadow, typ, hadType})
		b.shadows[name] = true
		b.receiverTypes[name] = bindings.types[name]
	}
	for id, expr := range pending {
		binding := b.valueBindings[id]
		binding.Expression = w.expressionForBinding(expr, b)
		b.valueBindings[id] = binding
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
