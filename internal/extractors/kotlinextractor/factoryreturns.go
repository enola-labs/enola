package kotlinextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// What a factory returns.
//
//	fun makeEngine(): Engine = …            // engine/Factory.kt
//
//	val engine = makeEngine()               // api/Sync.kt
//	engine.run()                            // Engine.run
//
// A local was typed by its written type or by the constructor it is initialised
// from. One bound to what a function returns had no type, so its calls were
// recorded by bare name and a flow through the function ended at the factory.
//
// The function's type is in its own file, and often another one, so the walker
// writes the local's type as the callee's canonical name with callMark after it
// (`api.makeEngine()`, `api.Registry.default()`, `api.make().session()`), and each
// function records the type it returns (propReturnType). resolveTypedCalls reads
// the mark as that type once every file's facts are in. The type is the declared
// return type, or for an expression body with none, the class it constructs.

const (
	// callMark follows the canonical name of a function whose result a value is.
	callMark = "()"
	// propReturnType holds, on a function, the canonical name its return type would
	// have. It does not leave the extractor.
	propReturnType = "return_type_candidate"
)

// functionReturnType returns the canonical name a function's return type would
// have: the type written after its parameters, or with none, the class its
// expression body constructs. A builder's `fun proxy(p: Proxy) = apply { … }`, and
// `= this`, return owner, the class the function is declared in.
func (w *astWalker) functionReturnType(fn *sitter.Node, owner string) string {
	pastParams := false
	for i := uint(0); i < fn.ChildCount(); i++ {
		c := fn.Child(i)
		switch kindOf(c) {
		case "function_value_parameters":
			pastParams = true
		case "user_type", "nullable_type":
			if pastParams {
				return w.typeCandidate(lastTypeIdentifier(c, w.src))
			}
		case "function_body":
			if !pastParams {
				return ""
			}
			// `fun make() = Engine(…)`: an expression body, no block.
			if findChildByKind(c, "block") != nil {
				return ""
			}
			for j := uint(0); j < c.NamedChildCount(); j++ {
				e := c.NamedChild(j)
				if kindOf(e) == "this_expression" {
					return owner
				}
				if kindOf(e) != "call_expression" {
					continue
				}
				if callee := firstNamedChild(e); callee != nil && kotlinIsIdentifier(callee) {
					name := nodeText(callee, w.src)
					if name == "apply" {
						return owner
					}
					if isCapitalized(name) {
						return w.typeCandidate(name)
					}
				}
			}
			return ""
		}
	}
	return ""
}

// callResultType returns the deferred type of what a call yields: a bare call of a
// function by its canonical name, and a method of a typed receiver, each with
// callMark after it. A constructor is typed as one (constructedType) and is not
// handled here.
func (w *astWalker) callResultType(call *sitter.Node) string {
	if call == nil || kindOf(call) != "call_expression" {
		return ""
	}
	callee := kotlinUnprefixed(firstNamedChild(call))
	if callee == nil {
		return ""
	}
	if kotlinIsIdentifier(callee) {
		name := nodeText(callee, w.src)
		if isCapitalized(name) {
			return ""
		}
		if _, local := w.localTypes[name]; local {
			return "" // a value of a type, invoked
		}
		if target := w.resolveCall(name); target != "" {
			return target + callMark
		}
		return ""
	}
	if kindOf(callee) != "navigation_expression" {
		return ""
	}
	member, _ := calleeName(callee, w.src)
	recv := kotlinUnprefixed(firstNamedChild(callee))
	if member == "" || recv == nil {
		return ""
	}
	if t := w.receiverType(recv); t != "" {
		return t + "." + member + callMark
	}
	return ""
}

// initializerCallType is callResultType for the call a property is initialised from.
func (w *astWalker) initializerCallType(prop *sitter.Node) string {
	for i := uint(0); i < prop.NamedChildCount(); i++ {
		if c := prop.NamedChild(i); kindOf(c) == "call_expression" {
			return w.callResultType(c)
		}
	}
	return ""
}

// returnTypes resolves the marks the walker wrote against the return types the
// functions recorded.
type returnTypes struct {
	returns   map[string]string
	exists    map[string]bool
	classes   map[string]bool
	declaring func(typ, method string) string
}

// typeOf returns a type with its marks resolved: the type a marked call returns,
// "" when that is not known, and an unmarked type as it is.
func (r returnTypes) typeOf(t string) string {
	return r.resolve(t, 0)
}

func (r returnTypes) resolve(t string, depth int) string {
	if !strings.HasSuffix(t, callMark) {
		return t
	}
	if depth > 8 {
		return ""
	}
	path := strings.TrimSuffix(t, callMark)
	var fn string
	if i := strings.LastIndex(path, callMark+"."); i >= 0 {
		// `make().session()`: a method of what the call before it returned.
		if recv := r.resolve(path[:i+len(callMark)], depth+1); recv != "" {
			fn = r.declaring(recv, path[i+len(callMark)+1:])
		}
	} else if r.exists[path] {
		fn = path
	} else if dot := strings.LastIndexByte(path, '.'); dot > 0 {
		fn = r.declaring(path[:dot], path[dot+1:])
	}
	if fn == "" {
		return ""
	}
	// `Http2Connection.Builder(…)`: a class reached by name is constructed.
	if r.classes[fn] {
		return fn
	}
	ret := r.returns[fn]
	if ret == "" || r.exists[ret] {
		return ret
	}
	// `fun newBuilder(): Builder` in OkHttpClient names OkHttpClient.Builder: a
	// simple name resolves among the types the function is nested in first.
	simple := ret[strings.LastIndexByte(ret, '.')+1:]
	for owner := fn; ; {
		dot := strings.LastIndexByte(owner, '.')
		if dot <= 0 {
			return ""
		}
		owner = owner[:dot]
		if r.classes[owner+"."+simple] {
			return owner + "." + simple
		}
	}
}
