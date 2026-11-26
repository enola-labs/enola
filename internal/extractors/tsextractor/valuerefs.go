package tsextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
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
// for. A namespace import names a module, not a symbol, and is left out. The body of a
// function the file calls where it writes it (wrapperBody) is the top level of a
// script that is not a module, and its declarations are symbols too; of its
// variables only those that stay symbols count (see withoutPlainVariables).
func moduleValues(kinds *tsutil.KindTable, root *sitter.Node, src []byte, dir string, importMap, internal map[string]string) map[string]string {
	out := map[string]string{}
	for local, target := range internal {
		if importMap[local] == target {
			out[local] = target
		}
	}
	addScopeValues(kinds, root, src, dir, out, wrapperDepth, false)
	return out
}

// addScopeValues adds the declarations of one scope, the module's or a wrapper's, and
// of the wrappers it calls, at most wrappers deep.
func addScopeValues(kinds *tsutil.KindTable, scope *sitter.Node, src []byte, dir string, out map[string]string, wrappers int, inWrapper bool) {
	for i := range scope.NamedChildCount() {
		stmt := scope.NamedChild(i)
		if kindOf(kinds, stmt) == "export_statement" {
			if d := stmt.ChildByFieldName("declaration"); d != nil {
				stmt = d
			}
		}
		switch kindOf(kinds, stmt) {
		case "lexical_declaration", "variable_declaration":
			for j := range stmt.NamedChildCount() {
				d := stmt.NamedChild(j)
				name := d.ChildByFieldName("name")
				if kindOf(kinds, d) != "variable_declarator" || name == nil || kindOf(kinds, name) != "identifier" {
					continue
				}
				if inWrapper && functionValue(kinds, d) == nil && !hasObjectMembers(kinds, d.ChildByFieldName("value"), src) {
					continue
				}
				out[nodeText(name, src)] = dir + "." + nodeText(name, src)
			}
		case "function_declaration", "generator_function_declaration", "class_declaration":
			if name := stmt.ChildByFieldName("name"); name != nil {
				out[nodeText(name, src)] = dir + "." + nodeText(name, src)
			}
		case "enum_declaration":
			// An enum is read as `Kind.Member`, a value read of the enum. A wrapper's
			// enum is not a symbol, so only the module's counts.
			if name := stmt.ChildByFieldName("name"); name != nil && !inWrapper {
				out[nodeText(name, src)] = dir + "." + nodeText(name, src)
			}
		case "expression_statement":
			if body := wrapperBody(kinds, stmt); body != nil && wrappers > 0 {
				addScopeValues(kinds, body, src, dir, out, wrappers-1, true)
			}
		}
	}
}

// fileScopeValueRefs returns the `names` drawn by code that runs at file scope, in
// the module's top level and in a wrapper's body (wrapperBody): a function handed on
// by value there (`global.Blob = BlobConstructor`, a ternary branch) has no
// enclosing symbol to carry the use. A declaration that is a symbol carries its own,
// and a function written inside has its own walk or none, so neither is read here.
// A call argument is left to the file's call references (fileRefs.callRefs), and a
// name whose property is assigned (`Card.propTypes = ...`) is not a use of it.
func fileScopeValueRefs(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx) []facts.Relation {
	if len(ctx.values) == 0 {
		return nil
	}
	// An imported name is already the file's reference wherever it is written
	// (fileRefs.walk), so only the file's own declarations are read here.
	own := map[string]string{}
	for name, target := range ctx.values {
		if _, imported := ctx.importMap[name]; !imported {
			own[name] = target
		}
	}
	scopeOf := ctx.callScope(kinds, "", nil)
	scopeOf.values = own
	w := &tsBodyWalker{callScope: scopeOf, seen: map[string]bool{}, locals: map[string]bool{}}
	var expr func(n *sitter.Node)
	expr = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch k := kindOf(kinds, n); {
		case tsIsFunctionLike(k), k == "class", k == "class_declaration":
			return
		case k == "identifier" || k == "shorthand_property_identifier":
			if !isCallArgument(kinds, n) && !inAssignedTarget(kinds, n) {
				w.noteValueRef(n)
			}
			return
		}
		for i := range n.ChildCount() {
			expr(n.Child(i))
		}
	}
	var scope func(block *sitter.Node, wrappers int, inWrapper bool)
	scope = func(block *sitter.Node, wrappers int, inWrapper bool) {
		for i := range block.NamedChildCount() {
			stmt := block.NamedChild(i)
			switch kindOf(kinds, stmt) {
			case "import_statement", "export_statement", "type_alias_declaration", "interface_declaration",
				"enum_declaration", "internal_module", "module", "ambient_declaration":
				// Bindings, and declarations that are symbols or only types.
				continue
			case "lexical_declaration", "variable_declaration":
				// A module's variable is a symbol carrying what its value names; a
				// wrapper's plain one is not (withoutPlainVariables), so the file does.
				if !inWrapper {
					continue
				}
				for j := range stmt.NamedChildCount() {
					d := stmt.NamedChild(j)
					if kindOf(kinds, d) == "variable_declarator" && functionValue(kinds, d) == nil && !hasObjectMembers(kinds, d.ChildByFieldName("value"), ctx.src) {
						expr(d.ChildByFieldName("value"))
					}
				}
				continue
			}
			if body := wrapperBody(kinds, stmt); body != nil {
				if wrappers > 0 {
					scope(body, wrappers-1, true)
				}
				continue
			}
			expr(stmt)
		}
	}
	scope(root, wrapperDepth, false)
	return w.rels
}

// isCallArgument reports an identifier passed straight to a call.
func isCallArgument(kinds *tsutil.KindTable, n *sitter.Node) bool {
	p := n.Parent()
	return p != nil && kindOf(kinds, p) == "arguments"
}

// inAssignedTarget reports an identifier that is the object an assignment writes a
// property of: `Card` in `Card.propTypes = ...` or `Card.a.b = ...`.
func inAssignedTarget(kinds *tsutil.KindTable, n *sitter.Node) bool {
	for cur := n; ; {
		p := cur.Parent()
		if p == nil {
			return false
		}
		switch kindOf(kinds, p) {
		case "member_expression", "subscript_expression":
			if obj := p.ChildByFieldName("object"); obj == nil || !sameTSNode(obj, cur) {
				return false
			}
			cur = p
		case "assignment_expression", "augmented_assignment_expression":
			left := p.ChildByFieldName("left")
			return cur != n && left != nil && sameTSNode(left, cur)
		default:
			return false
		}
	}
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
