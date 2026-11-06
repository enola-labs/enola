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
	conflict := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		switch kindOf(n) {
		case "anonymous_function", "arrow_function", "function_definition", "class_declaration":
			return // another scope
		case "assignment_expression":
			if name := variableName(n.ChildByFieldName("left"), w.src); name != "" && name != "this" {
				t := w.valueType(n.ChildByFieldName("right"), out)
				switch prior, seen := out[name]; {
				case conflict[name]:
				case t == "" || (seen && prior != t):
					delete(out, name)
					conflict[name] = true
				default:
					out[name] = t
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
		if recv := w.receiverOf(n.ChildByFieldName("object"), locals); recv != "" {
			return recv + "::" + phpText(name, w.src) + callMark
		}
	}
	return ""
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
