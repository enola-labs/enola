package tsextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
)

// globalObjects are the names the global object goes by. `this` at the top of a script
// is the same object, and is how a wrapper is handed it in code that predates the rest.
var globalObjects = map[string]bool{"window": true, "globalThis": true, "global": true, "self": true, "this": true}

// wrapperExposes returns the names of the declarations a wrapper hands to the world
// outside it. A wrapper keeps its contents private, so what it exposes it exposes on
// purpose, in one of three ways:
//
//	return { roll, Game: GameImpl };       // the object the wrapper evaluates to
//	return Game;                           // or a single value
//	window.Game = Game;                    // a property of the global object,
//	global.api = { roll: roll };           //   under any name the wrapper has for it
//	module.exports = Game;                 // or of the CommonJS module
//
// Those declarations are the file's exports in everything but syntax. Without this they
// were unexported functions nothing in the file calls, which is how the dead-code
// detector describes what is safest to delete.
//
// Assignment to any other object (`wp.media = media`) is not read: a namespace is a
// convention of one codebase, and reading every `a.b = c` as an export would make one
// of nearly every assignment.
func wrapperExposes(kinds *tsutil.KindTable, stmt, body *sitter.Node, src []byte) map[string]bool {
	exposed := make(map[string]bool)
	add := func(value *sitter.Node) {
		for _, name := range exposedNames(kinds, value, src) {
			exposed[name] = true
		}
	}
	globals := wrapperGlobals(kinds, stmt, src)
	for i := range body.NamedChildCount() {
		st := body.NamedChild(i)
		switch kindOf(kinds, st) {
		case "return_statement":
			// Only the wrapper's own result: a return inside a branch or a nested
			// function is not a direct child of the body.
			if st.NamedChildCount() > 0 {
				add(st.NamedChild(0))
			}
		case "expression_statement":
			assign := findChildByKind(kinds, st, "assignment_expression")
			if assign != nil && assignsToOutside(kinds, assign.ChildByFieldName("left"), globals, src) {
				add(assign.ChildByFieldName("right"))
			}
		}
	}
	return exposed
}

// wrapperGlobals returns the names the global object has inside a wrapper: its usual
// ones, and any parameter the wrapper was passed it as.
//
//	(function (root) { root.Game = Game })(window)
func wrapperGlobals(kinds *tsutil.KindTable, stmt *sitter.Node, src []byte) map[string]bool {
	names := make(map[string]bool, len(globalObjects))
	for name := range globalObjects {
		names[name] = true
	}
	call, callee := wrapperCall(kinds, stmt)
	if call == nil {
		return names
	}
	params, args := callee.ChildByFieldName("parameters"), call.ChildByFieldName("arguments")
	if params == nil || args == nil {
		return names
	}
	for i := range min(params.NamedChildCount(), args.NamedChildCount()) {
		param := params.NamedChild(i)
		if pattern := param.ChildByFieldName("pattern"); pattern != nil {
			param = pattern // TypeScript wraps a parameter's name
		}
		if kindOf(kinds, param) == "identifier" && globalObjects[nodeText(args.NamedChild(i), src)] {
			names[nodeText(param, src)] = true
		}
	}
	return names
}

// assignsToOutside reports whether an assignment target is a property of the global
// object or of the CommonJS module: `window.a`, `root.ns.a`, `module.exports`, `exports.a`.
func assignsToOutside(kinds *tsutil.KindTable, left *sitter.Node, globals map[string]bool, src []byte) bool {
	if kindOf(kinds, left) != "member_expression" {
		return false
	}
	if text := nodeText(left, src); text == "module.exports" || strings.HasPrefix(text, "module.exports.") {
		return true
	}
	root := left
	for kindOf(kinds, root) == "member_expression" {
		root = root.ChildByFieldName("object")
	}
	if root == nil {
		return false
	}
	name := nodeText(root, src)
	return globals[name] || name == "exports"
}

// exposedNames returns the local names a value hands out: the name itself, or the
// values of an object's properties. A function or class written in place is not a
// declaration and names nothing.
func exposedNames(kinds *tsutil.KindTable, value *sitter.Node, src []byte) []string {
	value = unwrapExpression(kinds, value)
	switch kindOf(kinds, value) {
	case "identifier":
		return []string{nodeText(value, src)}
	case "object":
		var names []string
		for i := range value.NamedChildCount() {
			member := value.NamedChild(i)
			switch kindOf(kinds, member) {
			case "shorthand_property_identifier":
				names = append(names, nodeText(member, src))
			case "pair":
				if v := member.ChildByFieldName("value"); kindOf(kinds, v) == "identifier" {
					names = append(names, nodeText(v, src))
				}
			}
		}
		return names
	}
	return nil
}

// markExposed marks the symbols a wrapper hands out, and their members, as exported.
func markExposed(ff []facts.Fact, dir string, exposed map[string]bool) {
	if len(exposed) == 0 {
		return
	}
	for i := range ff {
		if ff[i].Kind != facts.KindSymbol {
			continue
		}
		local, ok := strings.CutPrefix(ff[i].Name, dir+".")
		if !ok {
			continue
		}
		owner, _, _ := strings.Cut(local, ".")
		if _, has := ff[i].Props["exported"]; has && exposed[owner] {
			ff[i].SetProp("exported", true)
		}
	}
}
