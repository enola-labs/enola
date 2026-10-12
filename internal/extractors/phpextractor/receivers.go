package phpextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// propReceiverCalls lists, on a function or method, the instance calls whose
// receiver type the source declares, each as `Type::method`. It carries them from
// the symbol walk to bindReceiverCalls, which turns the ones that resolve into
// edges and removes it.
const propReceiverCalls = "receiver_calls"

// classPropertyTypes reads the declared type of each property of a class body: a
// typed property, a promoted constructor parameter, and a property the constructor
// assigns a typed parameter to.
func (w *phpWalker) classPropertyTypes(body *sitter.Node) map[string]string {
	props := map[string]string{}
	if body == nil {
		return props
	}
	for i := uint(0); i < body.NamedChildCount(); i++ {
		member := body.NamedChild(i)
		switch kindOf(member) {
		case "property_declaration":
			typ := w.declaredType(member.ChildByFieldName("type"))
			if typ == "" {
				continue
			}
			for j := uint(0); j < member.NamedChildCount(); j++ {
				if el := member.NamedChild(j); kindOf(el) == "property_element" {
					if name := variableName(el.ChildByFieldName("name"), w.src); name != "" {
						props[name] = typ
					}
				}
			}
		case "method_declaration":
			if phpText(member.ChildByFieldName("name"), w.src) != "__construct" {
				continue
			}
			params := w.parameterTypes(member.ChildByFieldName("parameters"), props)
			w.assignedPropertyTypes(member.ChildByFieldName("body"), params, props)
		}
	}
	return props
}

// parameterTypes reads the declared type of each parameter. A promoted constructor
// parameter is a property too, and is recorded in promoted when it is given.
func (w *phpWalker) parameterTypes(params *sitter.Node, promoted map[string]string) map[string]string {
	out := map[string]string{}
	if params == nil {
		return out
	}
	for i := uint(0); i < params.NamedChildCount(); i++ {
		p := params.NamedChild(i)
		kind := kindOf(p)
		if kind != "simple_parameter" && kind != "property_promotion_parameter" {
			continue
		}
		name := variableName(p.ChildByFieldName("name"), w.src)
		typ := w.declaredType(p.ChildByFieldName("type"))
		if name == "" || typ == "" {
			continue
		}
		out[name] = typ
		if kind == "property_promotion_parameter" && promoted != nil {
			promoted[name] = typ
		}
	}
	return out
}

// assignedPropertyTypes records `$this->prop = $param;` for each typed parameter,
// which is how a constructor written before promotion declares what a property holds.
func (w *phpWalker) assignedPropertyTypes(node *sitter.Node, params, props map[string]string) {
	if node == nil || len(params) == 0 {
		return
	}
	if kindOf(node) == "assignment_expression" {
		left, right := node.ChildByFieldName("left"), node.ChildByFieldName("right")
		if prop := thisProperty(left, w.src); prop != "" && props[prop] == "" {
			if typ := params[variableName(right, w.src)]; typ != "" {
				props[prop] = typ
			}
		}
		return
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		w.assignedPropertyTypes(node.NamedChild(i), params, props)
	}
}

// declaredType returns the class a type declaration names, fully qualified: `Foo`,
// `?Foo` and `Foo|null`. "" for a builtin type and for a union of classes, which
// names no single one.
func (w *phpWalker) declaredType(node *sitter.Node) string {
	if node == nil {
		return ""
	}
	named := node
	if kindOf(node) != "named_type" {
		named = nil
		for i := uint(0); i < node.NamedChildCount(); i++ {
			if c := node.NamedChild(i); kindOf(c) == "named_type" {
				if named != nil {
					return ""
				}
				named = c
			}
		}
		if named == nil {
			return ""
		}
	}
	return w.className(phpText(named, w.src))
}

// className returns the fully-qualified class a name written in this file refers to,
// as PHP resolves it: through the imports, else relative to the current namespace.
// "" for self or static outside a class.
func (w *phpWalker) className(raw string) string {
	raw = strings.TrimSpace(raw)
	switch raw {
	case "":
		return ""
	case "self", "static":
		if len(w.typeStack) == 0 {
			return ""
		}
		return w.typeStack[len(w.typeStack)-1]
	}
	if strings.HasPrefix(raw, `\`) {
		return strings.TrimLeft(raw, `\`)
	}
	first := raw
	if i := strings.IndexByte(raw, '\\'); i >= 0 {
		first = raw[:i]
	}
	if _, imported := w.importMap[first]; imported {
		return w.resolveRef(raw)
	}
	return w.qualify(raw)
}

// recordReceiverCall notes an instance call whose receiver type is declared: `$this`,
// a typed property of `$this`, a typed parameter, or an object created in place
// (`(new Action)->execute()`). The call keeps its bare-name
// edge; bindReceiverCalls adds the resolved one once every file is read.
func (w *phpWalker) recordReceiverCall(call *sitter.Node, ownerIdx int, method string) {
	if ownerIdx < 0 {
		return
	}
	obj := call.ChildByFieldName("object")
	typ := ""
	switch kindOf(obj) {
	case "variable_name":
		if name := variableName(obj, w.src); name != "this" {
			typ = w.paramTypes[name]
		} else if len(w.typeStack) > 0 {
			typ = w.typeStack[len(w.typeStack)-1]
		}
	case "member_access_expression":
		typ = w.propTypes[thisProperty(obj, w.src)]
	case "parenthesized_expression", "object_creation_expression":
		created := obj
		if kindOf(obj) == "parenthesized_expression" && obj.NamedChildCount() == 1 {
			created = obj.NamedChild(0)
		}
		if kindOf(created) == "object_creation_expression" {
			typ = w.className(w.creationClass(created))
		}
	}
	if typ == "" {
		return
	}
	target := typ + "::" + method
	calls, _ := w.out[ownerIdx].Props[propReceiverCalls].([]string)
	for _, c := range calls {
		if c == target {
			return
		}
	}
	w.out[ownerIdx].Props[propReceiverCalls] = append(calls, target)
}

// variableName returns a `$name` node's name without the sigil.
func variableName(node *sitter.Node, src []byte) string {
	if node == nil || kindOf(node) != "variable_name" {
		return ""
	}
	return strings.TrimPrefix(phpText(node, src), "$")
}

// thisProperty returns prop for a `$this->prop` node, "" for anything else.
func thisProperty(node *sitter.Node, src []byte) string {
	if node == nil || kindOf(node) != "member_access_expression" {
		return ""
	}
	if variableName(node.ChildByFieldName("object"), src) != "this" {
		return ""
	}
	if name := node.ChildByFieldName("name"); name != nil && kindOf(name) == "name" {
		return phpText(name, src)
	}
	return ""
}

// bindReceiverCalls adds a calls edge for each instance call whose receiver type is
// declared, to the method that call runs, in place.
//
// The symbol walk can only name such a call by its bare method name, which matches
// no symbol, so a walk from a controller stopped at the first injected service. The
// edge is added beside the bare name, never in its place, and only to a method this
// repository declares: on the receiver's class, or on the parent class or trait it
// inherits it from. A receiver typed by an interface reaches the interface's method,
// and the implementing method when exactly one class implements the interface.
//
// The one call named without a declaration is a framework method on an Eloquent model
// (`$order->delete()` on a route-bound Order): the model is this repository's, and the
// call is how the code touches it.
func bindReceiverCalls(allFacts []facts.Fact) {
	ix := newClassIndex(allFacts)
	models := map[string]bool{}
	isModel := func(class string) bool {
		known, seen := models[class]
		if !seen {
			known = ix.hasClass(class) && ix.extendsEloquent(class, map[string]bool{})
			models[class] = known
		}
		return known
	}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol || f.Props == nil {
			continue
		}
		calls, _ := f.Props[propReceiverCalls].([]string)
		delete(f.Props, propReceiverCalls)
		for _, call := range calls {
			sep := strings.LastIndex(call, "::")
			if sep <= 0 {
				continue
			}
			targets := ix.callTargets(call[:sep], call[sep+2:])
			// A model calling a framework method on itself says nothing new.
			if len(targets) == 0 && !strings.HasPrefix(f.Name, call[:sep]+"::") && isModel(call[:sep]) {
				targets = []string{call}
			}
			for _, target := range targets {
				if target != f.Name && !f.HasRelation(facts.RelCalls, target) {
					f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
				}
			}
		}
	}
}

// callTargets returns the declared methods a call of method on a receiver of the
// given type runs.
func (ix *classIndex) callTargets(typ, method string) []string {
	if ix.hasClass(typ) {
		if target := ix.declaringMethod(typ, method); target != "" {
			return []string{target}
		}
		return nil
	}
	if !ix.interfaces[typ] {
		return nil
	}
	var out []string
	if name := typ + "::" + method; ix.methods[name] {
		out = append(out, name)
	}
	if impls := ix.implementers[typ]; len(impls) == 1 {
		if target := ix.declaringMethod(impls[0], method); target != "" {
			out = append(out, target)
		}
	}
	return out
}
