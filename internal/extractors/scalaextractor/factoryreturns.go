package scalaextractor

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Locals of a def, and what they hold.
//
//	def sync(): Unit = {
//	  val engine = Factory.makeEngine()     // declared: def makeEngine(): Engine
//	  engine.run()                          // Engine.run
//	}
//
// A val in a def body was emitted as a member of the enclosing type, and the calls
// in its initializer were attributed to that phantom member: the def above called
// nothing and a flow through it stopped there. A val or var in a def body is now a
// local. Its initializer is walked as the def's, and it is typed by its ascribed
// type, the class it constructs, or what the def it is assigned from declares it
// returns, which may be in another file: the walker writes that as the def with
// callMark after it (`Factory.makeEngine()`), each def records its declared return
// type (propReturnType), and canonicalizeTargets reads the mark as that type. A
// mark it cannot read leaves the call by its bare name, as before.

const (
	callMark = "()"
	// propReturnType holds, on a def, its return type as its file names it. It does
	// not leave the extractor.
	propReturnType = "scala_return_type"
)

// handleLocalValue walks a val or var of a def body as the def's, and binds its
// type in the innermost scope.
func (w *astWalker) handleLocalValue(n *sitter.Node) {
	nameNode := n.ChildByFieldName("pattern")
	if nameNode == nil {
		nameNode = n.ChildByFieldName("name")
	}
	w.walkMembers(n, nameNode)
	if nameNode == nil || kindOf(nameNode) != "identifier" || len(w.localTypes) == 0 {
		return
	}
	name := w.text(nameNode)
	t := w.baseTypeName(n.ChildByFieldName("type"))
	if t == "" {
		t = w.valueType(n.ChildByFieldName("value"))
	}
	scope := w.localTypes[len(w.localTypes)-1]
	if t != "" {
		scope[name] = t
	} else {
		delete(scope, name) // shadows a typed binding of the same name
	}
}

// valueType is the type of what an expression yields: a class constructed with
// `new` or a case class's apply, or a def's result, marked.
func (w *astWalker) valueType(n *sitter.Node) string {
	for n != nil && kindOf(n) == "parenthesized_expression" {
		n = firstNamedChild(n)
	}
	if n == nil {
		return ""
	}
	switch kindOf(n) {
	case "instance_expression":
		for i := uint(0); i < n.ChildCount(); i++ {
			if c := n.Child(i); c.IsNamed() && isTypeNode(kindOf(c)) {
				return w.resolveTypeName(w.baseTypeName(c))
			}
		}
	case "call_expression":
		fn := firstNamedChild(n)
		if fn == nil {
			return ""
		}
		switch kindOf(fn) {
		case "identifier":
			name := w.text(fn)
			if isTypeName(name) {
				return w.resolveTypeName(name) // `Engine(…)`: a case class's apply
			}
			if w.declaresMember(name) {
				return w.qualify(name) + callMark
			}
		case "field_expression":
			recvNode, method := w.splitFieldExpression(fn)
			if recv := w.receiverTypeOf(recvNode); recv != "" && method != "" {
				return recv + "." + method + callMark
			}
		}
	}
	return ""
}

// receiverTypeOf is the type a call's receiver has, as a walker target names it:
// an object, a typed binding, or a call's result.
func (w *astWalker) receiverTypeOf(recv *sitter.Node) string {
	if recv == nil {
		return ""
	}
	switch kindOf(recv) {
	case "identifier", "type_identifier":
		name := w.text(recv)
		if isTypeName(name) {
			return w.resolveTypeName(name)
		}
		if t := w.lookupLocalType(name); t != "" {
			if strings.HasSuffix(t, callMark) {
				return t
			}
			return w.resolveTypeName(t)
		}
	case "call_expression", "parenthesized_expression":
		return w.valueType(recv)
	}
	return ""
}

// returnMarks reads marked call targets against the merged facts.
type returnMarks struct {
	returns  map[string]string // def -> return type as its file names it
	files    map[string]string // def -> its file
	filePkg  map[string]string
	typeIdx  map[string]string
	pkgDir   map[string]string
	declared map[string]bool
}

func newReturnMarks(all []facts.Fact, filePkg, typeIdx, pkgDir map[string]string) returnMarks {
	r := returnMarks{returns: map[string]string{}, files: map[string]string{}, filePkg: filePkg, typeIdx: typeIdx, pkgDir: pkgDir, declared: map[string]bool{}}
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		r.declared[f.Name] = true
		if t, ok := f.PropAny(propReturnType).(string); ok {
			r.returns[f.Name] = t
			r.files[f.Name] = f.File
			f.DelProp(propReturnType)
		}
	}
	return r
}

// typeOf returns the canonical type a marked receiver holds, seen from a file in
// package pkg, or "".
func (r returnMarks) typeOf(mark, pkg string, depth int) string {
	if !strings.HasSuffix(mark, callMark) || depth > 8 {
		return ""
	}
	path := strings.TrimSuffix(mark, callMark)
	var owner, method string
	if i := strings.LastIndex(path, callMark+"."); i >= 0 {
		owner, method = r.typeOf(path[:i+len(callMark)], pkg, depth+1), path[i+len(callMark)+1:]
	} else if dot := strings.LastIndex(path, "."); dot > 0 {
		owner, method = path[:dot], path[dot+1:]
		if !strings.Contains(owner, "/") {
			owner = resolveTypeTarget(owner, pkg, r.typeIdx, r.pkgDir)
		}
	}
	def := owner + "." + method
	ret, ok := r.returns[def]
	if owner == "" || !ok {
		return ""
	}
	t := resolveTypeTarget(ret, r.filePkg[r.files[def]], r.typeIdx, r.pkgDir)
	if !r.declared[t] {
		return ""
	}
	return t
}

// resolve rewrites a call target through a mark (`Factory.makeEngine().run`) to
// the method of the type the mark holds, or to the bare method name when the mark
// cannot be read.
func (r returnMarks) resolve(target, pkg string) string {
	i := strings.LastIndex(target, callMark+".")
	if i < 0 {
		return target
	}
	member := target[i+len(callMark)+1:]
	if t := r.typeOf(target[:i+len(callMark)], pkg, 0); t != "" {
		return t + "." + member
	}
	return member
}
