package swiftextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Calls on a receiver whose type the source states.
//
//	func didCreateURLRequest(_ urlRequest: URLRequest, for request: Request) {
//	    request.didCreateURLRequest(urlRequest)   // Request's, not Session's
//	    let task = request.task(for: urlRequest, using: session)
//	}
//	let initialRequest: URLRequest = …
//	try initialRequest.validate()                 // an extension of URLRequest
//
// A member call is bound in resolveMethodCalls by its name alone, which works while
// one type in the project declares a method of that name and leaves the call bare
// once two do, which is when it matters most. Where the receiver's type is written
// down (self, a parameter, a let or var with a type annotation or a constructor
// call) the call is bound to the method that type, one of its extensions or the
// nearest of its supertypes declares. A receiver of any other kind is left to
// resolveMethodCalls as before.

// propTypedCalls holds the walker's notes to resolveTypedCalls on a function fact.
const propTypedCalls = "swift_typed_calls"

// swiftTypedCall is one member call whose receiver's type is stated.
type swiftTypedCall struct {
	typ    string // simple type name
	method string
}

// statedType reduces a written type to the simple name of the type a value of it
// has: `Request?` and `Request!` are a Request, `[Request]` and a tuple none.
func statedType(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	t := strings.TrimSpace(nodeText(node, src))
	t = strings.TrimRight(t, "?!")
	if t == "" || strings.ContainsAny(t[:1], "[(") {
		return ""
	}
	if name := extractTypeName(t); isTypeName(name) {
		return name
	}
	return ""
}

// bindParameters records a function's typed parameters by their internal name.
func (w *astWalker) bindParameters(fn *sitter.Node) {
	w.localTypes = make(map[string]string)
	for i := uint(0); i < fn.ChildCount(); i++ {
		p := fn.Child(i)
		if kindOf(p) != "parameter" {
			continue
		}
		name := p.ChildByFieldName("name")
		if name == nil {
			continue
		}
		if t := statedType(parameterTypeNode(p), w.src); t != "" {
			w.localTypes[nodeText(name, w.src)] = t
		}
	}
}

// bindLocal records the type a local let or var states: its annotation, or the
// type it is constructed from (`let cache = Cache()`). A local of no stated type
// shadows a typed name.
func (w *astWalker) bindLocal(prop *sitter.Node) {
	nameNode := propertyNameNode(prop, w.src)
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	t := ""
	if ta := findChildByKind(prop, "type_annotation"); ta != nil {
		t = statedType(firstTypeNode(ta), w.src)
	} else if v := prop.ChildByFieldName("value"); v != nil && kindOf(v) == "call_expression" {
		if callee := firstNamedChild(v); callee != nil && kindOf(callee) == "simple_identifier" {
			if c := nodeText(callee, w.src); isTypeName(c) {
				t = c
			}
		}
	}
	if t != "" {
		w.localTypes[name] = t
	} else {
		delete(w.localTypes, name)
	}
}

// receiverType is the simple type name of a navigation callee's direct receiver
// when the source states it: a typed parameter or local.
func (w *astWalker) receiverType(callee *sitter.Node) string {
	if w.localTypes == nil || callee == nil || kindOf(callee) != "navigation_expression" {
		return ""
	}
	target := callee.ChildByFieldName("target")
	if target == nil {
		target = firstNamedChild(callee)
	}
	if target == nil || kindOf(target) != "simple_identifier" {
		return ""
	}
	return w.localTypes[nodeText(target, w.src)]
}

// noteTypedCall records a member call on a receiver of stated type against the
// function being walked.
func (w *astWalker) noteTypedCall(typ, method string) {
	if typ == "" || method == "" || isCapitalized(method) {
		return
	}
	idx := w.currentOwner()
	if idx < 0 {
		return
	}
	notes, _ := w.out[idx].PropAny(propTypedCalls).([]swiftTypedCall)
	w.out[idx].SetProp(propTypedCalls, append(notes, swiftTypedCall{typ, method}))
}

// resolveTypedCalls binds each noted call to the method its receiver's type
// declares, replacing the bare edge the walker wrote for it, or adding the edge
// where the walker skipped the name.
func resolveTypedCalls(allFacts []facts.Fact) {
	var h *swiftHierarchy
	for i := range allFacts {
		f := &allFacts[i]
		notes, ok := f.PropAny(propTypedCalls).([]swiftTypedCall)
		if !ok {
			continue
		}
		f.DelProp(propTypedCalls)
		if h == nil {
			h = newSwiftHierarchy(allFacts)
		}
		for _, n := range notes {
			target := h.resolve(n.typ, n.method)
			if target == "" || target == f.Name {
				continue
			}
			for j := range f.Relations {
				if r := &f.Relations[j]; r.Kind == facts.RelCalls && r.Target == n.method {
					f.Relations = append(f.Relations[:j], f.Relations[j+1:]...)
					break
				}
			}
			if !f.HasRelation(facts.RelCalls, target) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
			}
		}
	}
}
