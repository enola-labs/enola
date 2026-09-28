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
