package swiftextractor

import (
	"slices"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// The grammar gives a call a lower precedence than `+ - * /` and the prefix
// operators, so `a + f(x)` parses as a call whose callee is `a + f`, and `!f(x)` as
// a call whose callee is `!f`. The call is the right operand's, or the operator's
// target's. `.member(x)`, an implicit member call, is the same prefix shape with `.`
// as its operator: a static member of a type the context infers.

// operatorCallee returns the callee a call names when the grammar wrapped it in an
// arithmetic or prefix operator: the right operand, or the prefix target, unwrapped
// again. An implicit member (`.member`) and any other callee are returned as is.
func operatorCallee(callee *sitter.Node, src []byte) *sitter.Node {
	for callee != nil {
		var next *sitter.Node
		switch kindOf(callee) {
		case "additive_expression", "multiplicative_expression":
			next = callee.ChildByFieldName("rhs")
		case "prefix_expression":
			if implicitMember(callee, src) != "" {
				return callee
			}
			if op := callee.ChildByFieldName("operation"); op == nil || nodeText(op, src) == "." {
				return callee // `.a.b(x)`: an implicit member chain, not followed
			}
			next = callee.ChildByFieldName("target")
		}
		if next == nil {
			return callee
		}
		callee = next
	}
	return callee
}

// An implicit member call, `.member(...)`, calls a static member of the type the
// context infers, or makes an enum case. Names alone cannot tell them apart: a type
// that wraps a backing enum declares `static func ipv4(host:port:)` beside the
// case `ipv4(host:port:)`, and a framework type can have the same factory as the
// repository's. So the call binds only where the source states the type the
// context infers:
//
//	static var tls: Environment { .custom(name: "tls") }  // the declared type
//	static func make() -> Self { return .build(a: 1) }    // Self, the enclosing type
//	let c: Config = .defaults()                           // the annotation
//	self = .ipv4(host: h, port: p)                        // the enclosing type
//	server.address = .unix(path: p)                       // the property's type
//	respond(to: r, advertising: .basic(realm: r))         // the parameter's type
//
// A property is typed by its annotation in its own type, or, written through
// another receiver, by the one type every repository property of that name states.
// A parameter is typed by the one type every repository function of the called name
// states for that label. It binds to the static or class method of that type whose
// parameters take the labels the call writes, in order (one with a default may be
// left out); not when several types of that name declare one, and never to the
// method the call is written in. Anything else is not recorded.

// propImplicitCalls holds the walker's notes to resolveImplicitCalls on a fact.
const propImplicitCalls = "swift_implicit_calls"

// propStaticLabels holds, on a static or class method, its parameters' argument
// labels; propStatedType, on a property, the type it states; propParamTypes, on a
// function, the type each argument label states. They are for resolveImplicitCalls
// alone and do not leave the extractor.
const (
	propStaticLabels = "swift_static_labels"
	propStatedType   = "swift_stated_type"
	propParamTypes   = "swift_param_types"
)

// swiftImplicitCall is one `.member(...)` call, the argument labels it writes, and
// where its type comes from: typ when the walk read it, else the property it is
// assigned to, else the function and label it is passed to.
type swiftImplicitCall struct {
	member string
	labels []string
	typ    string
	prop   string
	fn     string
	label  string
}

// noteDeclaredTypes records, on a function, its static-ness and its parameters'
// stated types.
func noteDeclaredTypes(f *facts.Fact, modifierText string, fn *sitter.Node, src []byte) {
	labels := parameterLabels(fn, src)
	for _, m := range strings.Fields(modifierText) {
		if m == "static" || m == "class" {
			f.SetProp(propStaticLabels, append([]string{}, labels...))
			break
		}
	}
	types := map[string]string{}
	k := 0
	for i := uint(0); i < fn.ChildCount(); i++ {
		p := fn.Child(i)
		if kindOf(p) != "parameter" {
			continue
		}
		if t := statedType(parameterTypeNode(p), src); t != "" && k < len(labels) && labels[k] != "_" {
			types[labels[k]] = t
		}
		k++
	}
	if len(types) > 0 {
		f.SetProp(propParamTypes, types)
	}
}

// notePropertyType records the type a property states.
func (w *astWalker) notePropertyType(f *facts.Fact, prop *sitter.Node) {
	if ta := findChildByKind(prop, "type_annotation"); ta != nil {
		if t := statedType(firstTypeNode(ta), w.src); t != "" {
			f.SetProp(propStatedType, t)
		}
	}
}

// noteImplicitCall records the implicit member call `.member(...)` at call.
// `.init(...)` constructs the inferred type, which needs no method.
func (w *astWalker) noteImplicitCall(member string, call *sitter.Node) {
	if member == "" || member == "init" || isCapitalized(member) {
		return
	}
	idx := w.currentOwner()
	if idx < 0 {
		return
	}
	n := swiftImplicitCall{member: member, labels: callArgumentLabels(call, w.src)}
	w.implicitContext(call, &n)
	if n.typ == "" && n.prop == "" && n.fn == "" {
		return
	}
	notes, _ := w.out[idx].PropAny(propImplicitCalls).([]swiftImplicitCall)
	w.out[idx].SetProp(propImplicitCalls, append(notes, n))
}

// implicitContext fills in where the type the context of call infers is stated.
func (w *astWalker) implicitContext(call *sitter.Node, n *swiftImplicitCall) {
	// `x ?? .member()`: the right operand has the type of the whole expression.
	p := call.Parent()
	for p != nil && kindOf(p) == "nil_coalescing_expression" {
		if r := p.ChildByFieldName("if_nil"); r == nil || r.StartByte() != call.StartByte() {
			return
		}
		call, p = p, p.Parent()
	}
	if p == nil {
		return
	}
	enclosing := lastDotComponent(w.enclosingType())
	selfType := func(t string) string {
		if t == "Self" {
			return enclosing
		}
		return t
	}
	switch kindOf(p) {
	case "control_transfer_statement":
		if strings.HasPrefix(nodeText(p, w.src), "return") {
			n.typ = selfType(w.declaredResultType(p))
		}
	case "statements":
		if p.NamedChildCount() == 1 {
			switch kindOf(p.Parent()) {
			case "function_body", "computed_property", "computed_getter":
				n.typ = selfType(w.declaredResultType(p))
			}
		}
	case "property_declaration":
		if v := p.ChildByFieldName("value"); v != nil && v.StartByte() == call.StartByte() {
			if ta := findChildByKind(p, "type_annotation"); ta != nil {
				n.typ = selfType(statedType(firstTypeNode(ta), w.src))
			}
		}
	case "assignment", "tuple_expression":
		r := p.ChildByFieldName("result")
		t := firstNamedChild(p.ChildByFieldName("target"))
		if kindOf(p) == "tuple_expression" {
			// `(a, b) = (x, .member())`: the target of the same position.
			r, t = call, tupleTarget(p, call)
		}
		if r == nil || r.StartByte() != call.StartByte() || t == nil {
			return
		}
		switch kindOf(t) {
		case "self_expression":
			n.typ = enclosing
		case "simple_identifier":
			name := nodeText(t, w.src)
			if lt, ok := w.localTypes[name]; ok {
				n.typ = lt
			} else if mt := w.memberTypes[name]; mt != "" {
				n.typ = mt
			} else {
				n.prop = name
			}
		case "navigation_expression":
			if recv := t.ChildByFieldName("target"); recv != nil && kindOf(recv) == "self_expression" {
				if mt := w.memberTypes[swiftNavigationMember(t, w.src)]; mt != "" {
					n.typ = mt
					return
				}
			}
			n.prop = swiftNavigationMember(t, w.src)
		}
	case "value_argument":
		args := p.Parent()
		if args == nil || kindOf(args) != "value_arguments" || args.Parent() == nil {
			return
		}
		outer := args.Parent().Parent()
		if outer == nil || kindOf(outer) != "call_expression" {
			return
		}
		label := "_"
		if l := p.ChildByFieldName("name"); l != nil {
			label = strings.TrimSuffix(nodeText(l, w.src), ":")
		}
		if label == "_" {
			return
		}
		switch callee := operatorCallee(firstNamedChild(outer), w.src); kindOf(callee) {
		case "simple_identifier":
			n.fn = nodeText(callee, w.src)
		case "navigation_expression":
			n.fn = swiftNavigationMember(callee, w.src)
		}
		if isCapitalized(n.fn) {
			n.fn = "" // a constructor: its parameters are an init's
		}
		n.label = label
	}
}

// tupleTarget returns the target an element of a tuple assigned to a tuple of
// targets is assigned to, (a, b) = (x, elem), or nil.
func tupleTarget(tuple, elem *sitter.Node) *sitter.Node {
	asg := tuple.Parent()
	if asg == nil || kindOf(asg) != "assignment" {
		return nil
	}
	if r := asg.ChildByFieldName("result"); r == nil || r.StartByte() != tuple.StartByte() {
		return nil
	}
	targets := firstNamedChild(asg.ChildByFieldName("target"))
	if targets == nil || kindOf(targets) != "tuple_expression" {
		return nil
	}
	values := tupleValues(tuple)
	dests := tupleValues(targets)
	if len(values) != len(dests) {
		return nil
	}
	for i, v := range values {
		if v.StartByte() == elem.StartByte() && v.EndByte() == elem.EndByte() {
			return dests[i]
		}
	}
	return nil
}

// tupleValues returns the elements of a tuple_expression, labelled or not.
func tupleValues(tuple *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	for i := uint(0); i < tuple.ChildCount(); i++ {
		if tuple.FieldNameForChild(uint32(i)) == "value" {
			out = append(out, tuple.Child(i))
		}
	}
	return out
}

// declaredResultType returns the type the function or computed property enclosing
// node states it returns; "" inside a closure, whose result type is not stated.
func (w *astWalker) declaredResultType(node *sitter.Node) string {
	for p := node.Parent(); p != nil; p = p.Parent() {
		switch kindOf(p) {
		case "lambda_literal", "init_declaration", "subscript_declaration", "deinit_declaration":
			return ""
		case "property_declaration":
			if ta := findChildByKind(p, "type_annotation"); ta != nil {
				return statedType(firstTypeNode(ta), w.src)
			}
			return ""
		case "function_declaration":
			arrow := false
			for i := uint(0); i < p.ChildCount(); i++ {
				c := p.Child(i)
				if arrow && c.IsNamed() {
					return statedType(c, w.src)
				}
				if kindOf(c) == "->" {
					arrow = true
				}
			}
			return ""
		}
	}
	return ""
}

// labelsAccept reports whether a call writing args can call a function whose
// parameters take params: args is params with some left out, in order.
func labelsAccept(params, args []string) bool {
	j := 0
	for _, a := range args {
		for j < len(params) && params[j] != a {
			j++
		}
		if j == len(params) {
			return false
		}
		j++
	}
	return true
}

// resolveImplicitCalls writes the edge for each noted implicit member call.
func resolveImplicitCalls(allFacts []facts.Fact, index *swiftIndexes) {
	static := map[string][][]string{} // overloads share a name
	propTypes := map[string]map[string]bool{}
	paramTypes := map[string]map[string]bool{} // "fn label"
	for i := range allFacts {
		f := &allFacts[i]
		short := lastDotComponent(f.Name)
		if labels, ok := f.PropAny(propStaticLabels).([]string); ok {
			static[f.Name] = append(static[f.Name], labels)
			f.DelProp(propStaticLabels)
		}
		if t, ok := f.PropAny(propStatedType).(string); ok {
			addType(propTypes, short, t)
			f.DelProp(propStatedType)
		}
		if types, ok := f.PropAny(propParamTypes).(map[string]string); ok {
			for label, t := range types {
				addType(paramTypes, short+" "+label, t)
			}
			f.DelProp(propParamTypes)
		}
	}
	for i := range allFacts {
		f := &allFacts[i]
		notes, ok := f.PropAny(propImplicitCalls).([]swiftImplicitCall)
		if !ok {
			continue
		}
		f.DelProp(propImplicitCalls)
		methods := index.forFile(f.File).methods
		for _, n := range notes {
			typ := n.typ
			switch {
			case typ != "":
			case n.prop != "":
				typ = onlyType(propTypes[n.prop])
			case n.fn != "":
				typ = onlyType(paramTypes[n.fn+" "+n.label])
			}
			if typ == "" {
				continue
			}
			var cands []string
			for _, m := range methods[n.member] {
				if lastDotComponent(ownerOf(m)) != typ || slices.Contains(cands, m) {
					continue
				}
				for _, params := range static[m] {
					if labelsAccept(params, n.labels) {
						cands = append(cands, m)
						break
					}
				}
			}
			if len(cands) != 1 || cands[0] == f.Name || f.HasRelation(facts.RelCalls, cands[0]) {
				continue
			}
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: cands[0]})
		}
	}
}

func addType(m map[string]map[string]bool, key, t string) {
	if m[key] == nil {
		m[key] = map[string]bool{}
	}
	m[key][t] = true
}

// onlyType returns the one type of a set, "" when it holds none or several.
func onlyType(types map[string]bool) string {
	if len(types) != 1 {
		return ""
	}
	for t := range types {
		return t
	}
	return ""
}
