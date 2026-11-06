package tsextractor

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
)

// What a factory returns, where the factory is in another file.
//
//	export async function getEngine(): Promise<Engine> { … }    // engine/factory.ts
//
//	const engine = await getEngine();                          // api/store.ts
//	await engine.addNodes(nodes);                              // Engine.addNodes
//
// fileReturnTypes types a call on one of the file's own functions. A factory is
// nearly always imported, so the local held nothing a call could resolve on and a
// flow through the function ended at the factory. Each function and method now
// records what it returns (propReturnType) and what awaiting its result gives
// (propAwaitedType), and the walker types the local by a deferred step on the
// callee, "<callee>::()" or "<callee>::(await)", which resolveMemberTypedCalls
// evaluates once every file's facts are in.
//
// The type is the return annotation. An async function's `Promise<T>` is a T once
// awaited and nothing before. A function with no annotation whose every return is
// `new X(…)` returns an X.

const (
	// returnStep is the step from a function to what it returns, awaitStep to what
	// awaiting its result gives.
	returnStep = "()"
	awaitStep  = "(await)"
	// propReturnType and propAwaitedType hold those types on a function or method.
	// Neither leaves the extractor.
	propReturnType  = "ts_return_type"
	propAwaitedType = "ts_awaited_type"
)

// promiseTypes are the generics whose awaited value is their first argument.
var promiseTypes = map[string]bool{"Promise": true, "PromiseLike": true}

// declaredReturns returns the qualified type a function returns and the one
// awaiting its result gives, either "" where the source does not say.
func declaredReturns(kinds *tsutil.KindTable, fn *sitter.Node, src []byte, dir string, importMap map[string]string, declared map[string]bool) (ret, awaited string) {
	if fn == nil {
		return "", ""
	}
	if ann := fn.ChildByFieldName("return_type"); ann != nil {
		if inner, ok := promiseArgument(kinds, ann, src); ok {
			return "", qualifyTSType(inner, dir, importMap, declared)
		}
		ret = qualifyTSType(annotatedTypeName(kinds, ann, src), dir, importMap, declared)
		return ret, ret
	}
	t := qualifyTSType(constructedReturn(kinds, fn, src), dir, importMap, declared)
	if hasChildKind(kinds, fn, "async") {
		return "", t
	}
	return t, t
}

// promiseArgument returns T for an annotation `: Promise<T>`, with ok=false for any
// other annotation.
func promiseArgument(kinds *tsutil.KindTable, ann *sitter.Node, src []byte) (string, bool) {
	g := findChildByKind(kinds, ann, "generic_type")
	if g == nil {
		return "", false
	}
	base := findChildByKind(kinds, g, "type_identifier")
	if base == nil || !promiseTypes[nodeText(base, src)] {
		return "", false
	}
	args := findChildByKind(kinds, g, "type_arguments")
	if args == nil || args.NamedChildCount() != 1 {
		return "", true
	}
	if a := args.NamedChild(0); kindOf(kinds, a) == "type_identifier" {
		return nodeText(a, src), true
	}
	return "", true
}

// constructedReturn returns X when every value a function returns is `new X(…)`,
// and "" otherwise.
func constructedReturn(kinds *tsutil.KindTable, fn *sitter.Node, src []byte) string {
	body := fn.ChildByFieldName("body")
	if body == nil {
		return ""
	}
	ctor := func(v *sitter.Node) string {
		for v != nil && kindOf(kinds, v) == "parenthesized_expression" {
			v = v.NamedChild(0)
		}
		if v == nil || kindOf(kinds, v) != "new_expression" {
			return ""
		}
		if c := v.ChildByFieldName("constructor"); c != nil && kindOf(kinds, c) == "identifier" {
			return nodeText(c, src)
		}
		return ""
	}
	if kindOf(kinds, body) != "statement_block" {
		return ctor(body) // an arrow's expression body
	}
	var typ string
	ok := true
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil || !ok {
			return
		}
		switch kindOf(kinds, n) {
		case "function_declaration", "function_expression", "arrow_function", "method_definition", "class_declaration", "class":
			return // another function's returns
		case "return_statement":
			t := ""
			if n.NamedChildCount() > 0 {
				t = ctor(n.NamedChild(0))
			}
			if t == "" || (typ != "" && t != typ) {
				ok = false
				return
			}
			typ = t
		}
		for i := range n.NamedChildCount() {
			walk(n.NamedChild(i))
		}
	}
	walk(body)
	if !ok {
		return ""
	}
	return typ
}

// recordReturns writes onto a function or method fact the types resolveMemberTypedCalls
// reads for the steps through it.
func recordReturns(f *facts.Fact, kinds *tsutil.KindTable, fn *sitter.Node, ctx *extractCtx) {
	if ctx.typeNames == nil {
		ctx.typeNames = &typeNameCache{}
	}
	ret, awaited := declaredReturns(kinds, fn, ctx.src, ctx.dir, ctx.importMap, ctx.typeNames.of(kinds, fn, ctx.src))
	if ret != "" {
		f.SetProp(propReturnType, ret)
	}
	if awaited != "" {
		f.SetProp(propAwaitedType, awaited)
	}
}

// factoryResultType returns the deferred type of what a call yields: `f()` or
// `await f()` on a function of this file or another, `X.m()` on an imported or
// declared class or a typed local, and a method of what another call returned
// (`f().m()`). "" for anything else.
func factoryResultType(kinds *tsutil.KindTable, value *sitter.Node, src []byte, dir string, importMap, locals map[string]string, declared map[string]bool) string {
	awaiting := false
	if value != nil && kindOf(kinds, value) == "await_expression" {
		awaiting = true
		value = value.NamedChild(0)
	}
	if value == nil || kindOf(kinds, value) != "call_expression" {
		return ""
	}
	step := returnStep
	if awaiting {
		step = awaitStep
	}
	fn := value.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch kindOf(kinds, fn) {
	case "identifier":
		// A name that is no function of the repository (a parameter, a global) names
		// no fact, and the deferred type is dropped when it is evaluated.
		name := nodeText(fn, src)
		if q, ok := importMap[name]; ok {
			if q == "" {
				return ""
			}
			return q + memberSep + step
		}
		return dir + "." + name + memberSep + step
	case "member_expression":
		obj, prop := fn.ChildByFieldName("object"), fn.ChildByFieldName("property")
		if obj == nil || prop == nil || kindOf(kinds, prop) != "property_identifier" {
			return ""
		}
		for kindOf(kinds, obj) == "parenthesized_expression" && obj.NamedChildCount() > 0 {
			obj = obj.NamedChild(0)
		}
		var recv string
		switch kindOf(kinds, obj) {
		case "identifier":
			name := nodeText(obj, src)
			if t, ok := locals[name]; ok {
				recv = t
			} else if q := importMap[name]; q != "" {
				recv = q
			} else if declared[name] {
				recv = dir + "." + name
			}
		case "call_expression", "await_expression":
			recv = factoryResultType(kinds, obj, src, dir, importMap, locals, declared)
		}
		if recv == "" {
			return ""
		}
		if strings.Contains(recv, memberSep) {
			// A method of a deferred type: "<type>::m()".
			return recv + memberSep + nodeText(prop, src) + step
		}
		return recv + "." + nodeText(prop, src) + memberSep + step
	}
	return ""
}
