package pythonextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// What a class holds in `self`.
//
//	class Transfer(BaseOperator):
//	    client: S3Client                          # declared on the class
//
//	    def __init__(self, hook: S3Hook):
//	        self.hook = hook                      # a typed parameter, kept
//	        self.store = GCSHook()                # constructed
//	        self.log: Logger = get_logger()       # annotated
//
//	    @cached_property
//	    def dest(self) -> GCSHook: ...            # a property, by its return type
//
// A call was resolved on a local or a parameter whose type is written and never
// on `self.x`, so `self.hook.load_file(…)` named nothing: the method it reaches
// had one caller fewer than it has, and the I/O behind a hook stopped at the
// operator that holds it. Python keeps its collaborators in attributes, so this
// was most of what a class calls.
//
// Only statements are read, never values: an attribute's type is the annotation
// on it, the constructor it is assigned from, the annotation of the parameter it
// is assigned from, or the return annotation of the property of that name. An
// attribute assigned anything else has no declared type and is not resolved, and
// one assigned two different types keeps neither.

// collectSelfAttrTypes maps the attributes of a class to their declared types.
func collectSelfAttrTypes(body *sitter.Node, src []byte, importMap map[string]string, module string) map[string]string {
	out := make(map[string]string)
	if body == nil {
		return out
	}
	conflict := make(map[string]bool)
	set := func(name, typ string) {
		if name == "" || typ == "" || conflict[name] {
			return
		}
		if prev, ok := out[name]; ok && prev != typ {
			conflict[name] = true
			delete(out, name)
			return
		}
		out[name] = typ
	}
	resolve := func(n *sitter.Node) string {
		if n == nil {
			return ""
		}
		return resolveTypeNamePy(pyText(n, src), module, importMap)
	}

	for i := uint(0); i < body.NamedChildCount(); i++ {
		member := body.NamedChild(i)
		var decorators []string
		if kindOf(member) == "decorated_definition" {
			for j := uint(0); j < member.NamedChildCount(); j++ {
				if d := member.NamedChild(j); kindOf(d) == "decorator" {
					decorators = append(decorators, pyText(d, src))
				}
			}
			member = member.ChildByFieldName("definition")
			if member == nil {
				continue
			}
		}
		switch kindOf(member) {
		case "expression_statement":
			// `client: S3Client` in the class body.
			if st := member.NamedChild(0); st != nil && kindOf(st) == "assignment" {
				left, typ := st.ChildByFieldName("left"), st.ChildByFieldName("type")
				if left != nil && kindOf(left) == "identifier" && typ != nil {
					set(pyText(left, src), resolve(typ))
				}
			}
		case "function_definition":
			name := pyFuncName(member, src)
			if isPropertyDecorated(decorators) {
				set(name, resolve(member.ChildByFieldName("return_type")))
				continue
			}
			params := collectParamTypes(member.ChildByFieldName("parameters"), src, importMap, module)
			collectSelfAssignments(member.ChildByFieldName("body"), src, importMap, module, params, set)
		}
	}
	return out
}

func isPropertyDecorated(decorators []string) bool {
	for _, d := range decorators {
		d = strings.TrimPrefix(strings.TrimSpace(d), "@")
		if i := strings.IndexByte(d, '('); i >= 0 {
			d = d[:i]
		}
		switch lastComponent(d) {
		case "property", "cached_property":
			return true
		}
	}
	return false
}

// collectSelfAssignments reads the `self.x = …` statements of a method body.
func collectSelfAssignments(node *sitter.Node, src []byte, importMap map[string]string, module string, params map[string]string, set func(name, typ string)) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "function_definition", "class_definition", "lambda":
		return // another scope
	case "assignment":
		left := node.ChildByFieldName("left")
		name := selfAttrName(left, src)
		if name == "" {
			return
		}
		if typ := node.ChildByFieldName("type"); typ != nil {
			set(name, resolveTypeNamePy(pyText(typ, src), module, importMap))
			return
		}
		right := node.ChildByFieldName("right")
		if right == nil {
			return
		}
		switch kindOf(right) {
		case "identifier":
			set(name, params[pyText(right, src)])
		case "call":
			if fn := right.ChildByFieldName("function"); fn != nil && kindOf(fn) == "identifier" {
				if ctor := pyText(fn, src); pyCapitalized(ctor) && !pyBuiltins[ctor] {
					set(name, resolveTypeNamePy(ctor, module, importMap))
				}
			}
		}
		return
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		collectSelfAssignments(node.NamedChild(i), src, importMap, module, params, set)
	}
}

// selfAttrName returns x for the expression `self.x`, and "" for anything else.
func selfAttrName(n *sitter.Node, src []byte) string {
	if n == nil || kindOf(n) != "attribute" {
		return ""
	}
	obj, attr := n.ChildByFieldName("object"), n.ChildByFieldName("attribute")
	if obj == nil || attr == nil || kindOf(obj) != "identifier" || pyText(obj, src) != "self" {
		return ""
	}
	return pyText(attr, src)
}

// selfAttrType returns the declared type of `self.x` in the innermost class.
func (w *pyWalker) selfAttrType(objNode *sitter.Node) string {
	if len(w.attrTypes) == 0 {
		return ""
	}
	if name := selfAttrName(objNode, w.src); name != "" {
		return w.attrTypes[len(w.attrTypes)-1][name]
	}
	return ""
}
