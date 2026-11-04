package cppextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
)

// What a factory returns.
//
//	Engine* makeEngine();                 // engine.h, defined in engine.cpp
//
//	auto e = makeEngine();                // api.cpp
//	e->run();                             // Engine::run
//
// A receiver was typed only by an explicit declaration, so `auto` held nothing a
// call could resolve on. The type is the factory's return type, in another file,
// so the walker types the local as the callee with callMark after it
// (`makeEngine()`, `Factory::make()`, `Engine::session()` on a typed receiver), and
// each function definition records its return type's simple name (propReturnType).
// canonicalizeTargets reads the mark as that type, finding the callee as it finds
// a call's target, and the receiver call candidate goes on from there. The return
// type obeys the explicit declaration's rule: a project class name, through a
// pointer or reference, and never a template (a unique_ptr<T> is not a T).

const (
	callMark = "()"
	// propReturnType holds, on a function definition, its return type's simple
	// name. It does not leave the extractor.
	propReturnType = "return_type_simple"
)

// returnTypeName returns a function definition's return type when it is a simple
// project type name, and "".
func returnTypeName(node *sitter.Node, src []byte) string {
	typeNode := node.ChildByFieldName("type")
	if typeNode == nil || strings.Contains(nodeText(typeNode, src), "<") {
		return ""
	}
	if t := simpleTypeName(typeNode, src); t != "" && isTypeName(t) {
		return t
	}
	return ""
}

// autoCallType returns the mark for `auto x = f()`, `auto x = A::f()` and
// `auto x = y.f()` on a receiver of known type, and "" for anything else.
func autoCallType(kinds *tsutil.KindTable, value *sitter.Node, src []byte, known map[string]string) string {
	if value == nil || kindOf(kinds, value) != "call_expression" {
		return ""
	}
	fn := value.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch kindOf(kinds, fn) {
	case "identifier", "qualified_identifier":
		return nodeText(fn, src) + callMark
	case "field_expression":
		recv, field := fn.ChildByFieldName("argument"), fn.ChildByFieldName("field")
		if recv == nil || field == nil || kindOf(kinds, recv) != "identifier" {
			return ""
		}
		if t := known[nodeText(recv, src)]; t != "" && !strings.HasSuffix(t, callMark) {
			return t + "::" + nodeText(field, src) + callMark
		}
	}
	return ""
}

// isAuto reports whether a declaration's type is `auto`.
func isAuto(kinds *tsutil.KindTable, typeNode *sitter.Node, src []byte) bool {
	return typeNode != nil && kindOf(kinds, typeNode) == "placeholder_type_specifier" && strings.TrimSpace(nodeText(typeNode, src)) == "auto"
}

// returnIndex finds the function a mark names and the type it returns.
type returnIndex struct {
	returns   map[string]string
	known     map[string]bool
	functions functionNames
	types     typeIndex
	vis       *includeVisibility
}

func newReturnIndex(allFacts []facts.Fact, known map[string]bool, functions functionNames, types typeIndex, vis *includeVisibility) returnIndex {
	r := returnIndex{returns: map[string]string{}, known: known, functions: functions, types: types, vis: vis}
	for i := range allFacts {
		if t, ok := allFacts[i].PropAny(propReturnType).(string); ok {
			r.returns[allFacts[i].Name] = t
			allFacts[i].DelProp(propReturnType)
		}
	}
	return r
}

// typeOf returns the simple name of the type the callee in a mark returns, seen
// from a file in source, or "". The callee is found as a call's target is: in the
// caller's directory, by a type's directory, or by a name the repository declares
// once and the caller can see.
func (r returnIndex) typeOf(mark, source string) string {
	callee := strings.TrimSuffix(mark, callMark)
	var fn string
	switch {
	case r.known[source+"."+callee]:
		fn = source + "." + callee
	case strings.Contains(callee, "::"):
		scope := callee[:strings.LastIndex(callee, "::")]
		if dir := r.types.resolve(scope, source, r.vis, nil); dir != "" && r.known[dir+"."+callee] {
			fn = dir + "." + callee
		} else if c := r.functions.qualified[callee]; c != "" && r.vis.allows(source, c) {
			fn = c
		}
	default:
		if c := r.functions.short[callee]; c != "" && r.vis.allows(source, c) {
			fn = c
		}
	}
	return r.returns[fn]
}

// resolveMarkedCandidate rewrites a receiver call candidate whose receiver is a
// mark (`makeEngine()::run`) to the returned type's (`Engine::run`).
func (r returnIndex) resolveMarkedCandidate(target, source string) (string, bool) {
	i := strings.LastIndex(target, callMark+"::")
	if i < 0 {
		return target, true
	}
	t := r.typeOf(target[:i+len(callMark)], source)
	if t == "" {
		return "", false
	}
	return t + "::" + target[i+len(callMark)+2:], true
}
