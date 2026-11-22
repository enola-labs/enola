package swiftextractor

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Calls on a receiver whose type the source states.
//
//	func didPrepare(_ urlRequest: URLRequest, for request: Request) {
//	    request.didPrepare(urlRequest)            // Request's, not Session's
//	    let task = request.task(for: urlRequest, using: session)
//	}
//	let prepared: URLRequest = …
//	try prepared.validate()                       // an extension of URLRequest
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

// propertyType returns a let or var's name and the type it states: its annotation, or
// the type it is constructed from (`let cache = Cache()`); "" for none.
func (w *astWalker) propertyType(prop *sitter.Node) (string, string) {
	nameNode := propertyNameNode(prop, w.src)
	if nameNode == nil {
		return "", ""
	}
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
	return nodeText(nameNode, w.src), t
}

// bindLocal records the type a local let or var states. A local of no stated type
// shadows a typed name, the enclosing type's stored property among them.
func (w *astWalker) bindLocal(prop *sitter.Node) {
	if name, t := w.propertyType(prop); name != "" {
		w.localTypes[name] = t
	}
}

// memberTypesOf maps the stored properties a type body declares to the types they
// state, for calls the type's methods make on them: `useCase.execute()`, or
// `self.useCase.execute()`, inside a closure as anywhere else.
func (w *astWalker) memberTypesOf(body *sitter.Node) map[string]string {
	out := map[string]string{}
	for i := uint(0); i < body.ChildCount(); i++ {
		if c := body.Child(i); kindOf(c) == "property_declaration" {
			if name, t := w.propertyType(c); name != "" && t != "" {
				out[name] = t
			}
		}
	}
	return out
}

// receiverType is the simple type name of a navigation callee's direct receiver
// when the source states it: a typed parameter or local, or a stored property of the
// enclosing type, named bare or through self.
func (w *astWalker) receiverType(callee *sitter.Node) string {
	if w.localTypes == nil || callee == nil || kindOf(callee) != "navigation_expression" {
		return ""
	}
	target := callee.ChildByFieldName("target")
	if target == nil {
		target = firstNamedChild(callee)
	}
	if target == nil {
		return ""
	}
	switch kindOf(target) {
	case "simple_identifier":
		name := nodeText(target, w.src)
		if t, ok := w.localTypes[name]; ok {
			return t
		}
		return w.memberTypes[name]
	case "navigation_expression":
		// self.useCase.execute(): the receiver is self's stored property.
		inner := target.ChildByFieldName("target")
		if inner == nil {
			inner = firstNamedChild(target)
		}
		if inner == nil || kindOf(inner) != "self_expression" && nodeText(inner, w.src) != "self" {
			return ""
		}
		text := nodeText(target, w.src)
		return w.memberTypes[text[strings.LastIndexByte(text, '.')+1:]]
	}
	return ""
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
// where the walker skipped the name. A production caller sees production
// declarations only (scope.go).
func resolveTypedCalls(allFacts []facts.Fact, hier *swiftHierarchies) {
	for i := range allFacts {
		f := &allFacts[i]
		notes, ok := f.PropAny(propTypedCalls).([]swiftTypedCall)
		if !ok {
			continue
		}
		f.DelProp(propTypedCalls)
		h := hier.forFile(f.File)
		for _, n := range notes {
			target := h.resolve(n.typ, n.method, f.Name)
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
			if spelled := h.inheritedSpelling(n.typ, n.method, f.Name); spelled != "" {
				// Inherited: keep the type it was called on (PropInheritedCalls).
				f.AddInheritedCalls(spelled)
			}
			if d := h.extendedDeclaration(target, f.Name); d != "" && d != f.Name && !f.HasRelation(facts.RelCalls, d) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: d})
			}
		}
	}
}

// Calls rooted at a type name.
//
//	Bundle.module.path(forResource: "Words", ofType: "db")
//	SHA256.hash(data: payload)
//	Text("Title").adaptiveFont(.matter, size: 16)
//	Repo.shared.fetch()
//
// The method a member call names is bound in resolveMethodCalls by its name alone
// when one declaration in the repository has it. Rooted at a type the repository
// does not declare, the call is on a framework value, and the one in-repo `path` or
// `hash` a class or struct of the repository declares is somebody else's: binding it
// added a call and a module edge to an unrelated package. Such a value can still
// reach a method the repository adds to a framework type or protocol
// (`extension View { func adaptiveFont(...) }`, `extension Bundle`). So a call rooted
// at an undeclared type binds only to a method whose owner is not a class or struct
// of the repository. A chain that reaches the repository's code (scope.go,
// reachesRepo) binds by name as before, and so does a call on a value of unknown
// type (`coordinator.show()`), which is not rooted at a type.

// propRootedCalls holds the walker's notes to resolveRootedCalls on a fact.
const propRootedCalls = "swift_rooted_calls"

// swiftRootedCall is one member call whose receiver chain starts at a type name.
type swiftRootedCall struct {
	root   string
	via    []string // the members between root and method: `module` in Bundle.module.path()
	method string
}

// noteRootedCall records a member call whose receiver chain starts at the type name
// root. Self is the enclosing type, always the repository's.
func (w *astWalker) noteRootedCall(callee *sitter.Node, root, method string) {
	if root == "Self" {
		w.addTentativeMethodCall(method)
		return
	}
	if method == "" || isCapitalized(method) || swiftBuiltins[method] || swiftIterators[method] || swiftCheapMethods[method] {
		return
	}
	idx := w.currentOwner()
	if idx < 0 {
		return
	}
	notes, _ := w.out[idx].PropAny(propRootedCalls).([]swiftRootedCall)
	call := swiftRootedCall{root: root, via: chainMembers(callee.ChildByFieldName("target"), w.src), method: method}
	w.out[idx].SetProp(propRootedCalls, append(notes, call))
}

// chainMembers returns the member names a receiver chain passes through, from the
// outermost in: `af` for SecPolicy.af, `make` and `shared` for Repo.shared.make().
func chainMembers(node *sitter.Node, src []byte) []string {
	var out []string
	for node != nil {
		switch kindOf(node) {
		case "navigation_expression":
			suffix := node.ChildByFieldName("suffix")
			if suffix == nil {
				suffix = findChildByKind(node, "navigation_suffix")
			}
			if suffix != nil {
				if id := findFirstIdentifier(suffix, src); id != nil {
					out = append(out, nodeText(id, src))
				}
			}
			target := node.ChildByFieldName("target")
			if target == nil {
				target = firstNamedChild(node)
			}
			node = target
		case "call_expression":
			node = firstNamedChild(node)
		default:
			return out
		}
	}
	return out
}

// resolveRootedCalls writes the edge for each noted call, against the declarations
// the calling file sees (scope.go). A chain that reaches the repository gets the
// bare edge, for resolveTypedCalls and resolveMethodCalls to bind as any member
// call. Any other binds among openCandidates: one is bound, several stay bare, none
// gets no edge. A file's top-level fact left with no edge is dropped, as the walker
// drops one whose statements call nothing.
func resolveRootedCalls(allFacts []facts.Fact, index *swiftIndexes) []facts.Fact {
	var empty map[int]bool
	for i := range allFacts {
		f := &allFacts[i]
		notes, ok := f.PropAny(propRootedCalls).([]swiftRootedCall)
		if !ok {
			continue
		}
		f.DelProp(propRootedCalls)
		ix := index.forFile(f.File)
		for _, n := range notes {
			target := n.method
			if !ix.reachesRepo(n.root, n.via) {
				switch open := ix.openCandidates(n.method); len(open) {
				case 0:
					continue
				case 1:
					target = open[0]
				}
			}
			if !f.HasRelation(facts.RelCalls, target) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
			}
		}
		if f.Kind == facts.KindFileRef && len(f.Relations) == 0 {
			if empty == nil {
				empty = map[int]bool{}
			}
			empty[i] = true
		}
	}
	if len(empty) == 0 {
		return allFacts
	}
	out := allFacts[:0]
	for i := range allFacts {
		if !empty[i] {
			out = append(out, allFacts[i])
		}
	}
	return out
}

// noteMetatype records `Type.self`, a type handed on as a value
// (`responseDecodable(of: Payload.self)`, `register(Cell.self)`), as a names edge to
// the type. Nothing is called or constructed, so a type used only this way read as
// unused. canonicalizeTargets keeps it where the project declares the type.
func (w *astWalker) noteMetatype(nav *sitter.Node) {
	suffix := nav.ChildByFieldName("suffix")
	target := nav.ChildByFieldName("target")
	if suffix == nil || target == nil || kindOf(target) != "simple_identifier" {
		return
	}
	if id := findFirstIdentifier(suffix, w.src); id == nil || nodeText(id, w.src) != "self" {
		return
	}
	if name := nodeText(target, w.src); isTypeName(name) && !isSystemType(name) {
		w.addOwnerEdge(facts.RelNames, name)
	}
}
