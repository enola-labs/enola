package tsextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// A value reference is not a call, and was not an edge: a function that hands a
// module-scope value on (`query: listBankAccountQuery.loc?.source.body`), lists one in
// an object (`{ required, isValidPassword }`, a Vuelidate rule set) or passes one as an
// argument reached nothing, so impact on what the value stands for stopped there, and
// a helper used only that way read as test-only or dead. Such a use is recorded as
// `names`, which impact follows and coupling and fan-in leave out.
//
// Only a name the file binds at module scope counts: a declaration of its own, or a
// named or default import from this repository. A name the function binds itself (a
// parameter, a local, a destructured or caught binding) shadows it.

// moduleValues maps the names a file binds at module scope to the symbols they stand
// for. A namespace import names a module, not a symbol, and is left out.
func moduleValues(kinds *tsutil.KindTable, root *sitter.Node, src []byte, dir string, importMap, internal map[string]string) map[string]string {
	out := map[string]string{}
	for local, target := range internal {
		if importMap[local] == target {
			out[local] = target
		}
	}
	for i := range root.NamedChildCount() {
		stmt := root.NamedChild(i)
		if kindOf(kinds, stmt) == "export_statement" {
			if d := stmt.ChildByFieldName("declaration"); d != nil {
				stmt = d
			}
		}
		switch kindOf(kinds, stmt) {
		case "lexical_declaration", "variable_declaration":
			for j := range stmt.NamedChildCount() {
				d := stmt.NamedChild(j)
				if name := d.ChildByFieldName("name"); kindOf(kinds, d) == "variable_declarator" && name != nil && kindOf(kinds, name) == "identifier" {
					out[nodeText(name, src)] = dir + "." + nodeText(name, src)
				}
			}
		case "function_declaration", "generator_function_declaration", "class_declaration":
			if name := stmt.ChildByFieldName("name"); name != nil {
				out[nodeText(name, src)] = dir + "." + nodeText(name, src)
			}
		}
	}
	return out
}

// noteValueRef records `names` for an identifier in value position that stands for a
// module-scope value.
func (w *tsBodyWalker) noteValueRef(n *sitter.Node) {
	if len(w.values) == 0 {
		return
	}
	name := nodeText(n, w.src)
	target, ok := w.values[name]
	if !ok || target == w.selfName || !w.valuePosition(n) {
		return
	}
	if w.locals == nil {
		w.locals = boundNames(w.kinds, w.scopeRoot, w.src)
	}
	if w.locals[name] || w.seen["names:"+target] {
		return
	}
	w.seen["names:"+target] = true
	w.rels = append(w.rels, facts.Relation{Kind: facts.RelNames, Target: target})
}

// valuePosition reports an identifier that is read as a value: not the callee of a
// call or a construction (those are calls), not a name being bound, not a JSX tag.
func (w *tsBodyWalker) valuePosition(n *sitter.Node) bool {
	p := n.Parent()
	if p == nil {
		return false
	}
	is := func(field string) bool {
		c := p.ChildByFieldName(field)
		return c != nil && c.StartByte() == n.StartByte() && c.EndByte() == n.EndByte()
	}
	switch kindOf(w.kinds, p) {
	case "call_expression":
		return !is("function")
	case "new_expression":
		return !is("constructor")
	case "variable_declarator":
		return !is("name")
	case "assignment_expression", "augmented_assignment_expression":
		return !is("left")
	case "jsx_opening_element", "jsx_closing_element", "jsx_self_closing_element",
		"import_specifier", "export_specifier", "namespace_import", "import_clause",
		"labeled_statement", "break_statement", "continue_statement",
		"required_parameter", "optional_parameter", "catch_clause", "arrow_function",
		"function_declaration", "class_declaration", "function_expression", "method_definition":
		return false
	}
	return true
}

// boundNames returns every name a function binds within it: its parameters, its
// locals, destructured and caught bindings, and nested declarations. Over-approximate
// on purpose: a name bound anywhere inside shadows the module's, so no edge is drawn
// to a module value the code may not mean.
func boundNames(kinds *tsutil.KindTable, root *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	var pattern func(*sitter.Node)
	pattern = func(p *sitter.Node) {
		if p == nil {
			return
		}
		switch kindOf(kinds, p) {
		case "identifier", "shorthand_property_identifier_pattern":
			out[nodeText(p, src)] = true
		case "pair_pattern":
			pattern(p.ChildByFieldName("value"))
		case "assignment_pattern", "object_assignment_pattern":
			pattern(p.ChildByFieldName("left"))
		default:
			for i := range p.NamedChildCount() {
				pattern(p.NamedChild(i))
			}
		}
	}
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch kindOf(kinds, n) {
		case "variable_declarator", "function_declaration", "generator_function_declaration", "class_declaration":
			pattern(n.ChildByFieldName("name"))
		case "required_parameter", "optional_parameter":
			pattern(n.ChildByFieldName("pattern"))
		case "arrow_function":
			pattern(n.ChildByFieldName("parameter"))
		case "catch_clause":
			pattern(n.ChildByFieldName("parameter"))
		case "for_in_statement":
			pattern(n.ChildByFieldName("left"))
		}
		for i := range n.NamedChildCount() {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
	return out
}

// dataValueRefs returns the `names` a variable's non-function value draws: the
// module values its object, array or expression lists (`const rules = { password: {
// required, isValidPassword } }`). Its calls are not recorded, as before; a function
// inside it is an object member with its own symbol and walk, and is skipped.
func dataValueRefs(value *sitter.Node, scope callScope, selfName string) []facts.Relation {
	if len(scope.values) == 0 {
		return nil
	}
	w := &tsBodyWalker{callScope: scope, selfName: selfName, seen: map[string]bool{}, scopeRoot: value}
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil || tsIsFunctionLike(kindOf(w.kinds, n)) {
			return
		}
		switch kindOf(w.kinds, n) {
		case "identifier", "shorthand_property_identifier":
			w.noteValueRef(n)
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(value)
	return w.rels
}

// defaultExportValueRefs returns the `names` of a default-exported object, or of the
// object handed to defineComponent: a Vue Options API component's options.
func defaultExportValueRefs(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx) []facts.Relation {
	for i := range root.NamedChildCount() {
		stmt := root.NamedChild(i)
		if kindOf(kinds, stmt) != "export_statement" || !strings.Contains(nodeText(stmt, ctx.src), "default") {
			continue
		}
		value := stmt.ChildByFieldName("value")
		if value != nil && kindOf(kinds, value) == "call_expression" {
			if args := value.ChildByFieldName("arguments"); args != nil && args.NamedChildCount() > 0 {
				value = args.NamedChild(0)
			}
		}
		if value == nil || kindOf(kinds, value) != "object" {
			return nil
		}
		return dataValueRefs(value, ctx.callScope(kinds, "", nil), "")
	}
	return nil
}
