package phpextractor

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// What a local holds.
//
//	$engine = EngineFactory::make();           // declared: make(): Engine
//	$engine->run();                            // Engine::run
//	$cart = new Cart();  $cart->add($item);    // Cart::add
//	$q = $this->repo->query()->where(…);       // Builder::where, by query(): Builder
//
// A receiver was typed only as `$this`, a typed property of `$this`, a typed
// parameter or an object created in place, so a call on a local named nothing.
// A local now takes the type of what it is assigned: an object it creates, or what
// a method returns, declared in that method's signature and often in another file.
// The walker writes the latter as the method with callMark after it
// (`App\EngineFactory::make()`), each method records its declared return type
// (propReturnType), and bindReceiverCalls reads the mark as that type. A local
// assigned two different types has none.

const (
	callMark = "()"
	// readMark is a property read on a value: `T::rel@` is what `$t->rel` holds,
	// and elementMark an element of a collection: `T::get()[]` is what a foreach
	// over `T::get()` binds. bindRelationReads reads both (relationreads.go).
	readMark    = "@"
	elementMark = "[]"
	// propReturnType holds, on a method, the class its return type declares. It
	// does not leave the extractor.
	propReturnType = "php_return_type"
)

// callableLocalTypes maps the locals a callable body assigns to the type of what
// they are assigned.
func (w *phpWalker) callableLocalTypes(body *sitter.Node) map[string]string {
	out := map[string]string{}
	if body == nil {
		return out
	}
	// assigned holds what assignments give a local, and bound what a foreach, a
	// callback or a closure parameter binds it to. A binding never displaces an
	// assignment: `$x = $this->make(); foreach ($xs as $x)` keeps make's type, and
	// a name bound to two types has none.
	assigned, bound := localTypeSet{types: map[string]string{}}, localTypeSet{types: map[string]string{}}
	view := func(name string) {
		if t, ok := assigned.types[name]; ok {
			out[name] = t
		} else if t, ok := bound.types[name]; ok && !assigned.conflict[name] {
			out[name] = t
		} else {
			delete(out, name)
		}
	}
	assign := func(name, t string) {
		if name != "" && name != "this" {
			assigned.set(name, t)
			view(name)
		}
	}
	bind := func(name, t string) {
		if name != "" && name != "this" {
			bound.set(name, t)
			view(name)
		}
	}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		switch kindOf(n) {
		case "function_definition", "class_declaration":
			return // another scope
		case "anonymous_function", "arrow_function":
			// Another scope, whose code the enclosing callable's walk reads: a
			// parameter of declared type is that type.
			if params := n.ChildByFieldName("parameters"); params != nil {
				for name, t := range w.parameterTypes(params, nil) {
					bind(name, t)
				}
			}
			return
		case "assignment_expression":
			if name := variableName(n.ChildByFieldName("left"), w.src); name != "" {
				assign(name, w.valueType(n.ChildByFieldName("right"), out))
			}
		case "foreach_statement":
			if v := foreachValue(n); v != nil {
				if t := w.valueType(n.NamedChild(0), out); t != "" {
					bind(variableName(v, w.src), t+elementMark)
				}
			}
		case "member_call_expression", "nullsafe_member_call_expression":
			// `$models->each(function ($m) { … })`: the callback's first parameter,
			// when undeclared, is an element of what the call runs on.
			if name := n.ChildByFieldName("name"); name != nil && elementCallbacks[phpText(name, w.src)] {
				if p := firstCallbackParam(n); p != nil && p.ChildByFieldName("type") == nil {
					if t := w.chainReceiver(n.ChildByFieldName("object"), out); t != "" {
						bind(variableName(p.ChildByFieldName("name"), w.src), t+elementMark)
					}
				}
			}
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(body)
	return out
}

// localTypeSet is the one type each local is given, or none once it is given two.
type localTypeSet struct {
	types    map[string]string
	conflict map[string]bool
}

func (s *localTypeSet) set(name, t string) {
	if s.conflict == nil {
		s.conflict = map[string]bool{}
	}
	switch prior, seen := s.types[name]; {
	case s.conflict[name]:
	case t == "" || (seen && prior != t):
		delete(s.types, name)
		s.conflict[name] = true
	default:
		s.types[name] = t
	}
}

// valueType is the type of what an expression yields, marked where it is a
// method's result: an object created, a static call, or a call on a receiver of
// known type.
func (w *phpWalker) valueType(n *sitter.Node, locals map[string]string) string {
	for n != nil && kindOf(n) == "parenthesized_expression" && n.NamedChildCount() == 1 {
		n = n.NamedChild(0)
	}
	if n == nil {
		return ""
	}
	switch kindOf(n) {
	case "object_creation_expression":
		return w.className(w.creationClass(n))
	case "scoped_call_expression":
		scope, name := n.ChildByFieldName("scope"), n.ChildByFieldName("name")
		if scope == nil || name == nil || kindOf(name) != "name" {
			return ""
		}
		if s := phpText(scope, w.src); s != "parent" {
			if cls := w.className(s); cls != "" {
				return cls + "::" + phpText(name, w.src) + callMark
			}
		}
	case "member_call_expression", "nullsafe_member_call_expression":
		name := n.ChildByFieldName("name")
		if name == nil || kindOf(name) != "name" {
			return ""
		}
		if recv := w.chainReceiver(n.ChildByFieldName("object"), locals); recv != "" {
			return recv + "::" + phpText(name, w.src) + callMark
		}
	case "member_access_expression", "nullsafe_member_access_expression":
		if t := w.propTypes[thisProperty(n, w.src)]; t != "" {
			return t
		}
		name := n.ChildByFieldName("name")
		if name == nil || kindOf(name) != "name" {
			return ""
		}
		if recv := w.receiverOf(n.ChildByFieldName("object"), locals); recv != "" {
			return recv + "::" + phpText(name, w.src) + readMark
		}
	}
	return ""
}

// elementCallbacks are the collection methods that run their callback on each
// element, the element first; each runs it on each model of a query too.
var elementCallbacks = map[string]bool{
	"each": true, "map": true, "filter": true, "reject": true, "flatMap": true, "mapWithKeys": true,
	"transform": true, "sortBy": true, "sortByDesc": true, "keyBy": true, "groupBy": true,
	"every": true, "contains": true, "first": true, "last": true, "partition": true, "sum": true,
	"some": true, "search": true, "takeWhile": true, "skipWhile": true, "takeUntil": true, "skipUntil": true,
}

// foreachValue is the variable a foreach binds each value to, nil for a list()
// destructuring.
func foreachValue(n *sitter.Node) *sitter.Node {
	if n.NamedChildCount() < 2 {
		return nil
	}
	v := n.NamedChild(1)
	if kindOf(v) == "pair" && v.NamedChildCount() > 0 {
		v = v.NamedChild(v.NamedChildCount() - 1)
	}
	if kindOf(v) != "variable_name" {
		return nil
	}
	return v
}

// firstCallbackParam is the first parameter of a call's first argument when that
// argument is a closure.
func firstCallbackParam(call *sitter.Node) *sitter.Node {
	arg := positionalArg(call.ChildByFieldName("arguments"), 0)
	if arg == nil || !isClosure(arg) {
		return nil
	}
	params := arg.ChildByFieldName("parameters")
	if params == nil || params.NamedChildCount() == 0 {
		return nil
	}
	if p := params.NamedChild(0); kindOf(p) == "simple_parameter" {
		return p
	}
	return nil
}

// chainReceiver is receiverOf, and for a property read on a value of known type
// what that read holds (`$album->photos->first()`).
func (w *phpWalker) chainReceiver(obj *sitter.Node, locals map[string]string) string {
	switch kindOf(obj) {
	case "member_access_expression", "nullsafe_member_access_expression":
		return w.valueType(obj, locals)
	}
	return w.receiverOf(obj, locals)
}

// receiverOf is the type of a call's receiver: `$this`, a typed parameter or local,
// a typed property of `$this`, an object created in place, or a call's result.
func (w *phpWalker) receiverOf(obj *sitter.Node, locals map[string]string) string {
	switch kindOf(obj) {
	case "variable_name":
		name := variableName(obj, w.src)
		if name == "this" {
			if len(w.typeStack) > 0 {
				return w.typeStack[len(w.typeStack)-1]
			}
			return ""
		}
		if t := w.paramTypes[name]; t != "" {
			return t
		}
		return locals[name]
	case "member_access_expression":
		return w.propTypes[thisProperty(obj, w.src)]
	}
	return w.valueType(obj, locals)
}

// resolveReturnMarks reads a marked type as the type the marked method returns.
type returnMarks struct {
	ix      *classIndex
	returns map[string]string
}

func (r returnMarks) typeOf(t string, depth int) string {
	if !strings.HasSuffix(t, callMark) {
		return t
	}
	if depth > 8 {
		return ""
	}
	path := strings.TrimSuffix(t, callMark)
	sep := strings.LastIndex(path, "::")
	if sep <= 0 {
		return ""
	}
	cls, method := r.typeOf(path[:sep], depth+1), path[sep+2:]
	if cls == "" {
		return ""
	}
	var decl string
	if r.ix.hasClass(cls) {
		decl = r.ix.declaringMethod(cls, method)
	} else if r.ix.methods[cls+"::"+method] {
		decl = cls + "::" + method // an interface's
	}
	return r.returns[decl]
}
