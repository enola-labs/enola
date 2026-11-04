package javaextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// What a factory returns.
//
//	var engine = EngineFactory.makeEngine();      // declared: Engine makeEngine()
//	engine.run();                                 // Engine.run
//	EngineFactory.makeEngine().run();             // the same, chained
//
// A `var` was typed only by the `new` it is initialised with, and a call on a call's
// result only where the method is the enclosing class's own. The method's return
// type is in its declaration, often in another file, so the walker writes the
// receiver's type as the method's owner and name with callMark after it
// (`com.acme.EngineFactory.makeEngine()`), and each method records the type it
// returns (propReturnType). resolveTypedCalls reads the mark as that type before
// it reads any typed call, so a declared library type is classed as it is for a
// local written with that type.

const (
	// callMark follows a method whose result a receiver is.
	callMark = "()"
	// propReturnType holds, on a method, its return type's fully-qualified name.
	// It does not leave the extractor.
	propReturnType = "return_type_fqn"
)

// callResultType returns the type of what a method invocation yields: the
// declared return type of a method of the enclosing classes, or, for a method of
// a typed receiver, that receiver's type and the method, marked. "" otherwise.
func (w *astWalker) callResultType(inv *sitter.Node) string {
	if inv == nil || kindOf(inv) != "method_invocation" {
		return ""
	}
	name := inv.ChildByFieldName("name")
	if name == nil {
		return ""
	}
	obj := inv.ChildByFieldName("object")
	if obj == nil || kindOf(obj) == "this" {
		// `getRepo().save(x)`, `this.getRepo().save(x)`.
		key := nodeText(name, w.src) + "()"
		for i := len(w.fieldTypes) - 1; i >= 0; i-- {
			if t, ok := w.fieldTypes[i][key]; ok {
				return t
			}
		}
		return ""
	}
	if kindOf(obj) == "super" {
		return ""
	}
	if recv := w.receiverType(obj); recv != "" {
		return recv + "." + nodeText(name, w.src) + callMark
	}
	return ""
}

// resolveCallMarks replaces each marked receiver type in the walker's notes by the
// type the marked method returns, and drops the notes whose type is not known.
func resolveCallMarks(all []facts.Fact, typeIndex map[string]string, declaring func(typ, method string) string) {
	returns := make(map[string]string)
	for i := range all {
		if t, ok := all[i].PropAny(propReturnType).(string); ok {
			returns[all[i].Name] = t
			all[i].DelProp(propReturnType)
		}
	}
	var typeOf func(t string, depth int) string
	typeOf = func(t string, depth int) string {
		if !strings.HasSuffix(t, callMark) {
			return t
		}
		if depth > 8 {
			return ""
		}
		path := strings.TrimSuffix(t, callMark)
		var recv, method string
		if i := strings.LastIndex(path, callMark+"."); i >= 0 {
			recv, method = typeOf(path[:i+len(callMark)], depth+1), path[i+len(callMark)+1:]
		} else if dot := strings.LastIndexByte(path, '.'); dot > 0 {
			recv, method = path[:dot], path[dot+1:]
		}
		sym, ok := typeIndex[recv]
		if !ok {
			return "" // a library's method: what it returns is not read
		}
		m := declaring(sym, method)
		if m == "" {
			return ""
		}
		return returns[m]
	}
	for i := range all {
		calls, ok := all[i].PropAny(propTypedCalls).([]typedCall)
		if !ok {
			continue
		}
		kept := calls[:0]
		for _, c := range calls {
			if c.typ = typeOf(c.typ, 0); c.typ != "" {
				kept = append(kept, c)
			}
		}
		all[i].SetProp(propTypedCalls, kept)
	}
}
