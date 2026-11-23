package kotlinextractor

import (
	"slices"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Operator conventions.
//
//	operator fun LocalDateTime.minus(other: LocalDateTime): Duration = …
//	fun delay(at: LocalDateTime) = at - now()      // calls minus
//
// An operator is a call of the function its convention names: `a - b` calls
// a.minus(b), `a[i]` a.get(i), `x in a` a.contains(x), `a()` a.invoke(). The
// source never writes the name, so the call recorded nothing and an operator
// function read as unreferenced.
//
// A use binds only where the operand's type is declared (a parameter, a property
// or a local with a written type or a constructor), and only to an `operator fun`
// that type declares, a supertype of it declares, or an extension declares on
// it. The type is the repository's canonical name, or for an imported library
// type its qualified name, so an extension on kotlinx.datetime.LocalDateTime is
// not taken for one on another LocalDateTime. An extension binds only where the
// using file sees it: the same package, an import of it, or a wildcard import of
// its package. An operand of unknown type binds nothing.

// The walker's notes to resolveOperatorUses. Neither leaves the extractor.
const (
	propOperatorDecl = "operator_decl"
	propOperatorUses = "operator_uses"
)

// extTypePrefix marks a type key naming a library type by its qualified name.
const extTypePrefix = "ext:"

// operatorDecl is an `operator fun`: the type it is declared in or on, and the
// package declaring it.
type operatorDecl struct {
	key       string
	pkg       string
	extension bool
}

// fileScope is what decides whether a file sees an extension.
type fileScope struct {
	pkg       string
	imports   map[string]string // simple name -> canonical name of an internal import
	wildcards []string          // packages imported with .*
}

// operatorUse is one operator applied to an operand of declared type. methods
// are tried in order: `a += b` is plusAssign, else plus.
type operatorUse struct {
	key     string
	methods []string
	scope   *fileScope
}

// binaryOperators names the function each binary operator calls.
var binaryOperators = map[string]string{
	"+": "plus", "-": "minus", "*": "times", "/": "div", "%": "rem",
	"<": "compareTo", ">": "compareTo", "<=": "compareTo", ">=": "compareTo",
	"..": "rangeTo", "..<": "rangeUntil",
}

// compoundAssignments names the functions a compound assignment calls.
var compoundAssignments = map[string][]string{
	"+=": {"plusAssign", "plus"}, "-=": {"minusAssign", "minus"}, "*=": {"timesAssign", "times"},
	"/=": {"divAssign", "div"}, "%=": {"remAssign", "rem"},
}

// unaryOperators names the function a prefix or postfix operator calls.
var unaryOperators = map[string]string{"-": "unaryMinus", "+": "unaryPlus", "!": "not", "++": "inc", "--": "dec"}

// typeKey returns the key a written simple type name has: the canonical name of a
// repository type, extTypePrefix plus the qualified name of an imported library
// type, or "" (a builtin, or nothing known).
func (w *astWalker) typeKey(simple string) string {
	if fqn, ok := w.extImports[simple]; ok {
		return extTypePrefix + fqn
	}
	return w.typeCandidate(simple)
}

// extDeclaredType returns the key of a declaration's written type when it is an
// imported library type, "" otherwise (the repository's types are in the
// localTypes and memberTypes scopes).
func (w *astWalker) extDeclaredType(decl *sitter.Node) string {
	t := lastTypeIdentifier(firstTypeChild(decl), w.src)
	if fqn, ok := w.extImports[t]; ok && t != "" {
		return extTypePrefix + fqn
	}
	return ""
}

// bindExt records (or, untyped, forgets) a name's library type in a scope.
func (w *astWalker) bindExt(into map[string]string, name string, decl *sitter.Node) {
	if into == nil {
		return
	}
	if k := w.extDeclaredType(decl); k != "" {
		into[name] = k
	} else {
		delete(into, name)
	}
}

// operandKey returns the type key of an operand: a name of declared type.
func (w *astWalker) operandKey(n *sitter.Node) string {
	for n != nil && kindOf(n) == "parenthesized_expression" {
		n = firstNamedChild(n)
	}
	if n == nil || !kotlinIsIdentifier(n) {
		return ""
	}
	name := nodeText(n, w.src)
	if t, ok := w.localTypes[name]; ok {
		return t
	}
	if t, ok := w.localExt[name]; ok {
		return t
	}
	for i := len(w.memberTypes) - 1; i >= 0; i-- {
		if t, ok := w.memberTypes[i][name]; ok {
			return t
		}
		if i < len(w.memberExt) {
			if t, ok := w.memberExt[i][name]; ok {
				return t
			}
		}
	}
	return ""
}

// operatorToken returns the text of a node's first anonymous child.
func operatorToken(n *sitter.Node) string {
	for i := uint(0); i < n.ChildCount(); i++ {
		if c := n.Child(i); !c.IsNamed() {
			return kindOf(c)
		}
	}
	return ""
}

// noteOperatorUse records an operator applied to an operand of declared type.
func (w *astWalker) noteOperatorUse(node *sitter.Node, kind string) {
	var operand *sitter.Node
	var methods []string
	switch kind {
	case "binary_expression", "range_expression":
		if m, ok := binaryOperators[operatorToken(node)]; ok {
			operand, methods = firstNamedChild(node), []string{m}
		}
	case "index_expression":
		m := "get"
		if p := node.Parent(); p != nil && kindOf(p) == "assignment" && firstNamedChild(p) != nil && firstNamedChild(p).Id() == node.Id() && operatorToken(p) == "=" {
			m = "set"
		}
		operand, methods = firstNamedChild(node), []string{m}
	case "in_expression":
		operand, methods = lastNamedChild(node), []string{"contains"}
	case "unary_expression":
		if m, ok := unaryOperators[operatorToken(node)]; ok {
			operand, methods = firstNamedChild(node), []string{m}
		}
	case "assignment":
		if ms, ok := compoundAssignments[operatorToken(node)]; ok {
			operand, methods = firstNamedChild(node), ms
		}
	}
	if operand == nil {
		return
	}
	key := w.operandKey(operand)
	owner := w.currentOwner()
	if key == "" || owner == noOwner {
		return
	}
	if w.scope == nil {
		w.scope = &fileScope{pkg: w.pkg, imports: w.importMap, wildcards: w.wildcards}
	}
	uses, _ := w.out[owner].PropAny(propOperatorUses).([]operatorUse)
	w.out[owner].SetProp(propOperatorUses, append(uses, operatorUse{key, methods, w.scope}))
}

// noteOperatorDecl records an `operator fun` on its fact: an extension on the type
// it extends, a member on its enclosing type.
func (w *astWalker) noteOperatorDecl(f *facts.Fact, fn *sitter.Node, modifierText string) {
	if !strings.Contains(modifierText, "operator") || !hasOperatorModifier(fn, w.src) {
		return
	}
	if recv, ok := w.extensionReceiverKey(fn); ok {
		if recv != "" {
			f.SetProp(propOperatorDecl, operatorDecl{recv, w.pkg, true})
		}
		return
	}
	if len(w.typeStack) > 0 {
		f.SetProp(propOperatorDecl, operatorDecl{w.dir + "." + w.enclosingType(), w.pkg, false})
	}
}

// hasOperatorModifier reports whether a function declares the operator modifier.
func hasOperatorModifier(fn *sitter.Node, src []byte) bool {
	mods := findChildByKind(fn, "modifiers")
	if mods == nil {
		return false
	}
	for i := uint(0); i < mods.NamedChildCount(); i++ {
		if c := mods.NamedChild(i); kindOf(c) == "function_modifier" && strings.TrimSpace(nodeText(c, src)) == "operator" {
			return true
		}
	}
	return false
}

// extensionReceiverKey returns the type key of an extension function's receiver,
// and whether the function is one.
func (w *astWalker) extensionReceiverKey(fn *sitter.Node) (string, bool) {
	recv, ok := w.extensionReceiver(fn)
	if !ok || recv != "" {
		return recv, ok
	}
	name := fn.ChildByFieldName("name")
	for i := uint(0); i < fn.NamedChildCount(); i++ {
		c := fn.NamedChild(i)
		if name != nil && c.Id() == name.Id() {
			break
		}
		switch kindOf(c) {
		case "user_type", "nullable_type":
			return w.typeKey(lastTypeIdentifier(c, w.src)), true
		}
	}
	return "", true
}

// resolveOperatorUses turns the walker's operator notes into calls edges.
func resolveOperatorUses(all []facts.Fact) {
	type declared struct {
		name      string
		pkg       string
		extension bool
	}
	decls := map[string][]declared{} // key + "\x00" + method
	for i := range all {
		f := &all[i]
		d, ok := f.PropAny(propOperatorDecl).(operatorDecl)
		if !ok {
			continue
		}
		f.DelProp(propOperatorDecl)
		method := f.Name[strings.LastIndexByte(f.Name, '.')+1:]
		k := d.key + "\x00" + method
		if !slices.ContainsFunc(decls[k], func(x declared) bool { return x.name == f.Name }) {
			decls[k] = append(decls[k], declared{f.Name, d.pkg, d.extension})
		}
	}
	// A member operator a supertype declares: the implements edges are canonical
	// once resolveTypedCalls has run.
	supers := map[string][]string{}
	for i := range all {
		if all[i].Kind != facts.KindSymbol {
			continue
		}
		for _, r := range all[i].Relations {
			if r.Kind == facts.RelImplements && r.Target != all[i].Name {
				supers[all[i].Name] = append(supers[all[i].Name], r.Target)
			}
		}
	}
	visible := func(d declared, s *fileScope, method string) bool {
		if !d.extension || d.pkg == s.pkg {
			return true
		}
		if s.imports[method] == d.name {
			return true
		}
		return slices.Contains(s.wildcards, d.pkg)
	}
	for i := range all {
		f := &all[i]
		uses, ok := f.PropAny(propOperatorUses).([]operatorUse)
		if !ok {
			continue
		}
		f.DelProp(propOperatorUses)
		for _, u := range uses {
			target := ""
			for _, m := range u.methods {
				found := map[string]bool{}
				seen := map[string]bool{}
				for queue := []string{u.key}; len(queue) > 0 && len(found) == 0; {
					t := queue[0]
					queue = queue[1:]
					if seen[t] {
						continue
					}
					seen[t] = true
					for _, d := range decls[t+"\x00"+m] {
						if visible(d, u.scope, m) {
							found[d.name] = true
						}
					}
					queue = append(queue, supers[t]...)
				}
				if len(found) == 1 {
					for name := range found {
						target = name
					}
				}
				if len(found) > 0 {
					break // the first convention that resolves decides, ambiguous or not
				}
			}
			if target != "" && target != f.Name && !f.HasRelation(facts.RelCalls, target) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
			}
		}
	}
}
