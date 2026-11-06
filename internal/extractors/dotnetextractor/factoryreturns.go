package dotnetextractor

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// What a factory returns.
//
//	var engine = EngineFactory.Make();               // declared: Engine Make()
//	var conn = await _connections.OpenAsync();       // declared: Task<Connection> OpenAsync()
//	engine.Run();                                    // Engine.Run
//
// A `var` was typed only by the `new` it is initialised with, so a call on what a
// method returned had a bare name and no receiver type. The method's return type
// is in its declaration, often in another file and namespace, so the walker writes
// the receiver's type as the method's owner, as written, and its name, followed by
// callMark or awaitMark (`EngineFactory.Make()`, `_connections.OpenAsync(await)`).
// Each method records its return type as written, with its namespace, and the
// type a `Task<T>` or `ValueTask<T>` gives once awaited. resolveCallMarks reads a
// mark as the type it names before any typed call is resolved: a type of the
// repository as its symbol, prefixed boundMark; a library's as written, prefixed
// libraryMark, so it is classed by its name and never bound from the caller's
// namespace.

const (
	callMark  = "()"
	awaitMark = "(await)"
	// boundMark prefixes a receiver type already bound to its symbol, libraryMark
	// one the repository does not declare.
	boundMark   = "@"
	libraryMark = "!"
	// propReturnType and propAwaitedType hold, on a method, its return type as
	// written and what awaiting it gives. Neither leaves the extractor.
	propReturnType  = "return_type_written"
	propAwaitedType = "awaited_type_written"
)

// taskTypes are the types whose awaited value is their type argument.
var taskTypes = map[string]bool{"Task": true, "ValueTask": true}

// recordReturns writes a method's return types onto its props.
func (w *astWalker) recordReturns(props map[string]any, method *sitter.Node) {
	ret := method.ChildByFieldName("returns")
	if ret == nil {
		return
	}
	if kindOf(ret) == "generic_name" {
		if id := findChildByKind(ret, "identifier"); id != nil && taskTypes[nodeText(id, w.src)] {
			if args := findChildByKind(ret, "type_argument_list"); args != nil && args.NamedChildCount() == 1 {
				if t := w.receiverTypeOf(args.NamedChild(0)); t != "" {
					props[propAwaitedType] = t
				}
			}
			return
		}
	}
	if t := w.receiverTypeOf(ret); t != "" {
		props[propReturnType] = t
		props[propAwaitedType] = t
	}
}

// callResultType returns the marked type of what an invocation yields, awaited
// or not: a method of the enclosing type, of a typed receiver, or a static method
// of a type named as written. "" for anything else.
func (w *astWalker) callResultType(n *sitter.Node) string {
	mark := callMark
	n = unwrapParens(n)
	if n != nil && kindOf(n) == "await_expression" {
		mark = awaitMark
		n = unwrapParens(n.NamedChild(0))
	}
	if n == nil || kindOf(n) != "invocation_expression" {
		return ""
	}
	fn := n.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch kindOf(fn) {
	case "identifier", "generic_name":
		name := simpleTypeName(nodeText(fn, w.src))
		if target, ok := w.resolveOwnMember(name); ok {
			if dot := strings.LastIndexByte(target, '.'); dot > 0 {
				return boundMark + target[:dot] + "." + name + mark
			}
		}
	case "member_access_expression":
		nameNode, recvNode := fn.ChildByFieldName("name"), fn.ChildByFieldName("expression")
		if nameNode == nil || recvNode == nil {
			return ""
		}
		name := simpleTypeName(nodeText(nameNode, w.src))
		recv := w.receiverType(recvNode)
		if recv == "" && isTypeNameShaped(recvNode, nodeText(recvNode, w.src)) {
			recv = nodeText(recvNode, w.src) // `EngineFactory.Make()`: static
		}
		if recv != "" {
			return recv + "." + name + mark
		}
	}
	return ""
}

func unwrapParens(n *sitter.Node) *sitter.Node {
	for n != nil && kindOf(n) == "parenthesized_expression" {
		n = n.NamedChild(0)
	}
	return n
}

// isMarked reports whether a receiver type is a call's result.
func isMarked(t string) bool {
	return strings.HasSuffix(t, callMark) || strings.HasSuffix(t, awaitMark)
}

// lastStep splits a marked path at its last call step: `A.Make().Session` is
// (`A.Make()`, `Session`); ok=false when the path has none.
func lastStep(path string) (recv, method string, ok bool) {
	i, j := strings.LastIndex(path, callMark+"."), strings.LastIndex(path, awaitMark+".")
	switch {
	case i < 0 && j < 0:
		return "", "", false
	case j > i:
		return path[:j+len(awaitMark)], path[j+len(awaitMark)+1:], true
	default:
		return path[:i+len(callMark)], path[i+len(callMark)+1:], true
	}
}

// resolveCallMarks replaces each marked receiver type in the walker's notes by
// the type the method returns, and drops the notes whose type is not known.
func resolveCallMarks(all []facts.Fact, resolve func(target, fromNS string) (string, bool), declaring func(typ, method string) string) {
	type ret struct{ typ, awaited, ns string }
	returns := make(map[string]ret)
	for i := range all {
		f := &all[i]
		t, _ := f.PropAny(propReturnType).(string)
		a, _ := f.PropAny(propAwaitedType).(string)
		if t == "" && a == "" {
			continue
		}
		ns, _ := f.PropAny("namespace").(string)
		returns[f.Name] = ret{t, a, ns}
		f.DelProp(propReturnType)
		f.DelProp(propAwaitedType)
	}
	// bind returns a receiver type's symbol, or "".
	var typeOf func(t, fromNS string, depth int) string
	bind := func(t, fromNS string, depth int) string {
		switch {
		case strings.HasPrefix(t, boundMark):
			return strings.TrimPrefix(t, boundMark)
		case strings.HasPrefix(t, libraryMark):
			return ""
		case isMarked(t):
			if r := typeOf(t, fromNS, depth+1); strings.HasPrefix(r, boundMark) {
				return strings.TrimPrefix(r, boundMark)
			}
			return ""
		}
		sym, ok := resolve(t, fromNS)
		if !ok {
			return ""
		}
		return sym
	}
	typeOf = func(t, fromNS string, depth int) string {
		if !isMarked(t) || depth > 8 {
			if isMarked(t) {
				return ""
			}
			return t
		}
		awaited := strings.HasSuffix(t, awaitMark)
		path := strings.TrimSuffix(strings.TrimSuffix(t, awaitMark), callMark)
		recv, method, ok := lastStep(path)
		if !ok {
			dot := strings.LastIndexByte(path, '.')
			if dot <= 0 {
				return ""
			}
			recv, method = path[:dot], path[dot+1:]
		}
		owner := bind(recv, fromNS, depth)
		if owner == "" {
			return ""
		}
		m := declaring(owner, method)
		r, known := returns[m]
		if m == "" || !known {
			return ""
		}
		written := r.typ
		if awaited {
			written = r.awaited
		}
		if written == "" {
			return ""
		}
		if sym, ok := resolve(written, r.ns); ok {
			return boundMark + sym
		}
		return libraryMark + written
	}
	for i := range all {
		f := &all[i]
		calls, ok := f.PropAny(propTypedCalls).([]typedCall)
		if !ok {
			continue
		}
		fromNS, _ := f.PropAny("namespace").(string)
		kept := calls[:0]
		for _, c := range calls {
			if !c.base && isMarked(c.typ) {
				if c.typ = typeOf(c.typ, fromNS, 0); c.typ == "" {
					continue
				}
			}
			kept = append(kept, c)
		}
		f.SetProp(propTypedCalls, kept)
	}
}

// boundType reads a typed call's receiver type: bound already, a library's, or
// as written and resolved from the caller's namespace.
func boundType(typ, fromNS string, resolve func(target, fromNS string) (string, bool)) (sym, written string, known bool) {
	switch {
	case strings.HasPrefix(typ, boundMark):
		return strings.TrimPrefix(typ, boundMark), "", true
	case strings.HasPrefix(typ, libraryMark):
		return "", strings.TrimPrefix(typ, libraryMark), false
	}
	sym, known = resolve(typ, fromNS)
	return sym, typ, known
}
