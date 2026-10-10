package pythonextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// What a call returns, where the source says so.
//
//	class GCSHook(GoogleBaseHook):
//	    def get_conn(self) -> storage.Client: ...
//
//	    def delete(self, bucket_name, object_name):
//	        client = self.get_conn()               # a storage.Client
//	        client.bucket(bucket_name)             # storage.Client.bucket
//	        self.get_conn().delete_blob(…)         # storage.Client.delete_blob
//
// A hook reaches its client through an accessor, and the accessor is where the
// client's type is written. Calls on its result named nothing, so the method
// that makes the request carried no I/O, and neither did anything that calls it.
//
// Only declarations are read. The accessor is a method of the same class or a
// function of the same file, and its type is its return annotation. A method the
// class inherits is not followed, and an unannotated one says nothing. A local
// takes a type only when every binding of that name in the function gives it the
// same one.

// pyValueType reduces a return annotation to the name of the type a returned
// value has, or "" when the annotation does not name one: `S3Client | None` and
// `Optional[S3Client]` are an S3Client, `Queue[Job]` is a Queue, and `list[Job]`,
// `Awaitable[Job]` and `A | B` are none.
func pyValueType(ann string) string {
	ann = strings.TrimSpace(ann)
	ann = strings.Trim(ann, `"'`)
	if ann == "" {
		return ""
	}
	if i := strings.IndexByte(ann, '['); i >= 0 {
		if !strings.HasSuffix(ann, "]") {
			return ""
		}
		base, inner := strings.TrimSpace(ann[:i]), ann[i+1:len(ann)-1]
		if lastComponent(base) == "Optional" {
			return pyValueType(inner)
		}
		if pyBuiltinTypes[lastComponent(base)] {
			return ""
		}
		return base
	}
	if strings.Contains(ann, "|") {
		var only string
		for _, part := range strings.Split(ann, "|") {
			part = strings.TrimSpace(part)
			if part == "None" || part == "" {
				continue
			}
			if only != "" {
				return ""
			}
			only = part
		}
		return pyValueType(only)
	}
	if pyBuiltinTypes[ann] {
		return ""
	}
	return ann
}

// pyReturnAnnotation returns the text of a function's return annotation.
func pyReturnAnnotation(fn *sitter.Node, src []byte) string {
	ret := fn.ChildByFieldName("return_type")
	if ret == nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(pyText(ret, src)), "->"))
}

// pyDefinition unwraps a decorated definition.
func pyDefinition(n *sitter.Node) *sitter.Node {
	if n != nil && kindOf(n) == "decorated_definition" {
		return n.ChildByFieldName("definition")
	}
	return n
}

// collectMethodReturnTypes maps the methods a class body defines to the types
// their return annotations name.
func collectMethodReturnTypes(body *sitter.Node, src []byte, importMap map[string]string, module string) map[string]string {
	out := make(map[string]string)
	if body == nil {
		return out
	}
	for i := uint(0); i < body.NamedChildCount(); i++ {
		member := pyDefinition(body.NamedChild(i))
		if member == nil || kindOf(member) != "function_definition" {
			continue
		}
		if t := pyValueType(pyReturnAnnotation(member, src)); t != "" {
			if resolved := resolveTypeNamePy(t, module, importMap); resolved != "" {
				out[pyFuncName(member, src)] = resolved
			}
		}
	}
	return out
}

// collectFuncReturnAnnotations maps a file's top-level functions to their return
// annotations, unresolved: a name in one may be imported further down the file.
func collectFuncReturnAnnotations(root *sitter.Node, src []byte) map[string]string {
	out := make(map[string]string)
	for i := uint(0); i < root.NamedChildCount(); i++ {
		def := pyDefinition(root.NamedChild(i))
		if def == nil || kindOf(def) != "function_definition" {
			continue
		}
		if t := pyValueType(pyReturnAnnotation(def, src)); t != "" {
			out[pyFuncName(def, src)] = t
		}
	}
	return out
}

// pyUnwrapValue strips what stands between an expression and the value it
// yields: parentheses and `await`.
func pyUnwrapValue(n *sitter.Node) *sitter.Node {
	for n != nil {
		switch kindOf(n) {
		case "parenthesized_expression", "await":
			n = n.NamedChild(0)
		default:
			return n
		}
	}
	return nil
}

// callResultType returns the declared type of what a call expression yields:
// `self.get_conn()` by the method's return annotation, `make_client()` by the
// function's. Anything else, and a name the function rebinds, is "".
func (w *pyWalker) callResultType(n *sitter.Node) string {
	n = pyUnwrapValue(n)
	if n == nil || kindOf(n) != "call" {
		return ""
	}
	fn := n.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch kindOf(fn) {
	case "attribute":
		if name := selfAttrName(fn, w.src); name != "" && len(w.returnTypes) > 0 {
			return w.returnTypes[len(w.returnTypes)-1][name]
		}
	case "identifier":
		name := pyText(fn, w.src)
		if w.localBound[name] {
			return ""
		}
		if ann, ok := w.funcReturns[name]; ok {
			return resolveTypeNamePy(ann, w.module, w.importMap)
		}
	}
	return ""
}

// valueType returns the declared type of an expression a local is bound to: a
// call (see callResultType) or an attribute of self.
func (w *pyWalker) valueType(n *sitter.Node) string {
	n = pyUnwrapValue(n)
	if n == nil {
		return ""
	}
	if kindOf(n) == "call" {
		return w.callResultType(n)
	}
	return w.selfAttrType(n)
}

// inferLocalTypes adds to localTypes the locals bound to a value whose type is
// declared. A name bound more than once keeps a type only when every binding
// gives the same one, and a name that already has a type keeps it.
func (w *pyWalker) inferLocalTypes(body *sitter.Node) {
	inferred := make(map[string]string)
	unknown := make(map[string]bool)
	bind := func(target, value *sitter.Node) {
		if target == nil {
			return
		}
		if kindOf(target) != "identifier" {
			// `a, b = …` binds each name to a part of the value.
			pyBindTargets(target, w.src, unknown)
			return
		}
		name := pyText(target, w.src)
		typ := ""
		if value != nil {
			typ = w.valueType(value)
		}
		if prev, seen := inferred[name]; typ == "" || (seen && prev != typ) {
			unknown[name] = true
			return
		}
		inferred[name] = typ
	}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch kindOf(n) {
		case "function_definition", "class_definition", "lambda":
			return // another scope
		case "assignment":
			// An annotated assignment states its own type and is collectLocalTypes'.
			if n.ChildByFieldName("type") == nil {
				bind(n.ChildByFieldName("left"), n.ChildByFieldName("right"))
			}
		case "augmented_assignment", "for_statement":
			bind(n.ChildByFieldName("left"), nil)
		case "named_expression":
			bind(n.ChildByFieldName("name"), n.ChildByFieldName("value"))
		case "with_item":
			if v := n.ChildByFieldName("value"); v != nil && kindOf(v) == "as_pattern" {
				if alias := v.ChildByFieldName("alias"); alias != nil {
					bind(alias.NamedChild(0), v.NamedChild(0))
				}
			}
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(body)
	for name, typ := range inferred {
		if unknown[name] {
			continue
		}
		if _, typed := w.localTypes[name]; !typed {
			w.localTypes[name] = typ
		}
	}
}

// What a module-level name holds, where the source says so.
//
//	security_manager: SecurityManager = LocalProxy(lambda: appbuilder.sm)
//	feature_flags = FeatureFlagManager()
//
// An application keeps its services in module globals and every other module
// imports them, so `security_manager.can_access(…)` is a call on a declared type
// that named nothing. As for a local, the type is the annotation or the
// constructor, and a name bound twice to different types has none.

// collectGlobalTypes maps the top-level names of a module to their declared
// types, keyed as a symbol of that module would be (`pkg/mod.name`). A package's
// __init__ is keyed under the package directory too, which is how an import of
// the package names it.
func collectGlobalTypes(root *sitter.Node, src []byte, module string, importMap map[string]string) map[string]string {
	types := make(map[string]string)
	conflict := make(map[string]bool)
	for i := uint(0); i < root.NamedChildCount(); i++ {
		st := root.NamedChild(i)
		if kindOf(st) != "expression_statement" {
			continue
		}
		st = st.NamedChild(0)
		if st == nil || kindOf(st) != "assignment" {
			continue
		}
		left := st.ChildByFieldName("left")
		if left == nil || kindOf(left) != "identifier" {
			continue
		}
		name, typ := pyText(left, src), ""
		if ann := st.ChildByFieldName("type"); ann != nil {
			if t := pyValueType(pyText(ann, src)); t != "" {
				typ = resolveTypeNamePy(t, module, importMap)
			}
		} else if right := st.ChildByFieldName("right"); right != nil && kindOf(right) == "call" {
			if fn := right.ChildByFieldName("function"); fn != nil && kindOf(fn) == "identifier" {
				if ctor := pyText(fn, src); pyCapitalized(ctor) && !pyBuiltins[ctor] {
					typ = resolveTypeNamePy(ctor, module, importMap)
				}
			}
		}
		if prev, seen := types[name]; typ == "" || (seen && prev != typ) {
			conflict[name] = true
			continue
		}
		types[name] = typ
	}
	out := make(map[string]string, len(types))
	for name, typ := range types {
		if conflict[name] {
			continue
		}
		out[module+"."+name] = typ
		if pkg, ok := strings.CutSuffix(module, "/__init__"); ok {
			out[pkg+"."+name] = typ
		}
	}
	return out
}
