package tsextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// nestedFunctions returns the named functions declared inside another function: a local
// function declaration, a variable bound to a function or arrow, a named function
// expression, and a method or function-valued key of an object literal or local class.
// Anonymous ones are left out, since nothing can refer to them by name. Which of them
// already are symbols is facts.AttachNested's to tell.
func nestedFunctions(kinds *tsutil.KindTable, root *sitter.Node, src []byte) []facts.Nested {
	var out []facts.Nested
	var walk func(n *sitter.Node, inFunction bool)
	walk = func(n *sitter.Node, inFunction bool) {
		for i := range n.NamedChildCount() {
			c := n.NamedChild(i)
			k := kindOf(kinds, c)
			if inFunction {
				if name := nestedFunctionName(kinds, c, k, src); name != "" {
					out = append(out, facts.Nested{Name: name, Line: int(c.StartPosition().Row) + 1, EndLine: tsutil.EndLine(c)})
				}
			}
			walk(c, inFunction || tsFunctionKinds[k])
		}
	}
	walk(root, false)
	return out
}

// nestedFunctionName is the name n declares a function under, or "" when it declares none.
func nestedFunctionName(kinds *tsutil.KindTable, n *sitter.Node, k string, src []byte) string {
	isFunctionValue := func(v *sitter.Node) bool {
		if v == nil {
			return false
		}
		switch kindOf(kinds, v) {
		case "arrow_function", "function_expression", "function", "generator_function":
			return true
		}
		return false
	}
	switch k {
	case "function_declaration", "generator_function_declaration", "method_definition":
		return tsutil.Text(n.ChildByFieldName("name"), src)
	case "function_expression", "function", "generator_function":
		// A function a declarator or a key binds is named there, once.
		if p := n.Parent(); p != nil {
			if pk := kindOf(kinds, p); pk == "variable_declarator" || pk == "pair" {
				return ""
			}
		}
		return tsutil.Text(n.ChildByFieldName("name"), src)
	case "variable_declarator":
		if name := n.ChildByFieldName("name"); name != nil && kindOf(kinds, name) == "identifier" && isFunctionValue(n.ChildByFieldName("value")) {
			return tsutil.Text(name, src)
		}
	case "pair":
		if key := n.ChildByFieldName("key"); key != nil && kindOf(kinds, key) == "property_identifier" && isFunctionValue(n.ChildByFieldName("value")) {
			return tsutil.Text(key, src)
		}
	}
	return ""
}
