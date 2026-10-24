package tsextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// classFieldTypes returns, for one class, the qualified type of each instance field
// whose type the declaration states, so a call on `this.<field>.<method>()` can name
// the method it reaches. Without it every such call was left unresolved, which is
// how a class reaches the services it is handed: the NestJS/Angular constructor
// parameter `private readonly resources: ResourceConnector` is the only place the
// type of `resources` is written.
//
// A field's type is read from:
//   - a constructor parameter property (`private readonly x: T`, `readonly x: T`);
//   - a type annotation on the field (`private x: T`, `x!: T<U>` is T);
//   - an initializer that constructs or injects it (`x = new T()`, `x = inject(T)`).
//
// The type name is qualified the way a bare call to it would be: through the file's
// imports ("<dir>.T" or "<package>.T"), else as declared in the class's own
// directory. Only a plain or generic type name qualifies; a union, an array, a
// primitive or an inline object type names no class, and the field is left out.
func classFieldTypes(kinds *tsutil.KindTable, classBody *sitter.Node, src []byte, dir string, importMap map[string]string) map[string]string {
	if classBody == nil {
		return nil
	}
	out := map[string]string{}
	qualify := func(typeName string) string {
		if q, ok := importMap[typeName]; ok {
			return q
		}
		return dir + "." + typeName
	}
	for i := range classBody.ChildCount() {
		member := classBody.Child(i)
		switch kindOf(kinds, member) {
		case "public_field_definition":
			name := findChildByKind(kinds, member, "property_identifier")
			if name == nil {
				continue
			}
			if t := annotatedTypeName(kinds, findChildByKind(kinds, member, "type_annotation"), src); t != "" {
				out[nodeText(name, src)] = qualify(t)
			} else if t := initializerTypeName(kinds, member, src); t != "" {
				out[nodeText(name, src)] = qualify(t)
			}
		case "method_definition":
			n := member.ChildByFieldName("name")
			if n == nil || nodeText(n, src) != "constructor" {
				continue
			}
			params := member.ChildByFieldName("parameters")
			if params == nil {
				continue
			}
			for j := range params.ChildCount() {
				p := params.Child(j)
				if k := kindOf(kinds, p); (k != "required_parameter" && k != "optional_parameter") || !isParameterProperty(kinds, p, src) {
					continue
				}
				id := findChildByKind(kinds, p, "identifier")
				t := annotatedTypeName(kinds, findChildByKind(kinds, p, "type_annotation"), src)
				if id != nil && t != "" {
					out[nodeText(id, src)] = qualify(t)
				}
			}
		}
	}
	return out
}

// isParameterProperty reports whether a constructor parameter also declares a field:
// it carries an accessibility modifier, readonly or override.
func isParameterProperty(kinds *tsutil.KindTable, p *sitter.Node, src []byte) bool {
	for i := range p.ChildCount() {
		c := p.Child(i)
		if kindOf(kinds, c) == "accessibility_modifier" || kindOf(kinds, c) == "override_modifier" {
			return true
		}
		if !c.IsNamed() && nodeText(c, src) == "readonly" {
			return true
		}
	}
	return false
}

// annotatedTypeName returns the class a type annotation names: T for `: T` and
// `: T<U>`, "" for anything that names no single class.
func annotatedTypeName(kinds *tsutil.KindTable, ann *sitter.Node, src []byte) string {
	if ann == nil {
		return ""
	}
	for i := range ann.ChildCount() {
		c := ann.Child(i)
		switch kindOf(kinds, c) {
		case "type_identifier":
			return nodeText(c, src)
		case "generic_type":
			if t := findChildByKind(kinds, c, "type_identifier"); t != nil {
				return nodeText(t, src)
			}
		}
	}
	return ""
}

// initializerTypeName returns the class a field initializer constructs or injects:
// T for `new T(…)` and for Angular's `inject(T)`.
func initializerTypeName(kinds *tsutil.KindTable, field *sitter.Node, src []byte) string {
	for i := range field.ChildCount() {
		c := field.Child(i)
		switch kindOf(kinds, c) {
		case "new_expression":
			if ctor := c.ChildByFieldName("constructor"); ctor != nil && kindOf(kinds, ctor) == "identifier" {
				return nodeText(ctor, src)
			}
		case "call_expression":
			fn := c.ChildByFieldName("function")
			args := c.ChildByFieldName("arguments")
			if fn == nil || args == nil || kindOf(kinds, fn) != "identifier" || nodeText(fn, src) != "inject" {
				continue
			}
			for j := range args.ChildCount() {
				if a := args.Child(j); kindOf(kinds, a) == "identifier" {
					return nodeText(a, src)
				}
			}
		}
	}
	return ""
}

// classDataFields returns the instance fields of a class that hold a value rather
// than a function: a field with no initializer or one that is not a function
// (`private _proxy: Proxy | null`, `count = 0`, `x = new T()`), and a constructor
// parameter property. A field initialised with an arrow or function expression is
// a method in all but syntax, and is left out.
func classDataFields(kinds *tsutil.KindTable, classBody *sitter.Node, src []byte) map[string]bool {
	if classBody == nil {
		return nil
	}
	out := map[string]bool{}
	for i := range classBody.ChildCount() {
		member := classBody.Child(i)
		switch kindOf(kinds, member) {
		case "public_field_definition":
			if name := findChildByKind(kinds, member, "property_identifier"); name != nil && tsFieldHoldsData(kinds, member) {
				out[nodeText(name, src)] = true
			}
		case "method_definition":
			n := member.ChildByFieldName("name")
			params := member.ChildByFieldName("parameters")
			if n == nil || nodeText(n, src) != "constructor" || params == nil {
				continue
			}
			for j := range params.ChildCount() {
				p := params.Child(j)
				if k := kindOf(kinds, p); (k == "required_parameter" || k == "optional_parameter") && isParameterProperty(kinds, p, src) {
					if id := findChildByKind(kinds, p, "identifier"); id != nil {
						out[nodeText(id, src)] = true
					}
				}
			}
		}
	}
	return out
}

// tsFieldHoldsData reports whether a public_field_definition holds a value: it has
// no initializer, or one that is not a function.
func tsFieldHoldsData(kinds *tsutil.KindTable, member *sitter.Node) bool {
	value := member.ChildByFieldName("value")
	if value == nil {
		return true
	}
	switch kindOf(kinds, value) {
	case "arrow_function", "function_expression", "function", "generator_function":
		return false
	}
	return true
}

// classMethodReturnTypes returns, for one class, the class each method's return
// annotation names, qualified as a local's type is (qualifyTSType): a method that
// returns a global or ambient type, a union or a primitive is left out. A local
// assigned `this.method()` has that type.
func classMethodReturnTypes(kinds *tsutil.KindTable, classBody *sitter.Node, src []byte, dir string, importMap map[string]string, declared map[string]bool) map[string]string {
	if classBody == nil {
		return nil
	}
	out := map[string]string{}
	for i := range classBody.ChildCount() {
		member := classBody.Child(i)
		if kindOf(kinds, member) != "method_definition" {
			continue
		}
		name := member.ChildByFieldName("name")
		if name == nil {
			continue
		}
		if q := qualifyTSType(annotatedTypeName(kinds, member.ChildByFieldName("return_type"), src), dir, importMap, declared); q != "" {
			out[nodeText(name, src)] = q
		}
	}
	return out
}

// thisMethodCall returns m for `this.m(…)`, and "" for anything else.
func thisMethodCall(kinds *tsutil.KindTable, call *sitter.Node, src []byte) string {
	if call == nil || kindOf(kinds, call) != "call_expression" {
		return ""
	}
	fn := call.ChildByFieldName("function")
	if fn == nil || kindOf(kinds, fn) != "member_expression" {
		return ""
	}
	obj, prop := fn.ChildByFieldName("object"), fn.ChildByFieldName("property")
	if obj == nil || prop == nil || kindOf(kinds, obj) != "this" {
		return ""
	}
	return nodeText(prop, src)
}
