package javaextractor

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// A type is used without being called, instantiated, extended or applied as an
// annotation when code names it as a value or a declaration's type: a class literal
// (`Param.class`, the SPI and reflection lookups), a static member read
// (`Param.DEFAULT_NONE`, `Color.RED`), and a declared parameter or field type, its
// type arguments included. Each records the type's candidate FQNs the way an
// annotation does (annotationrefs.go), so canonicalizeTargets keeps a RelNames edge
// only to a type the repository declares.
//
// A name javac resolves to something else records nothing: a type parameter in
// scope, and, for a static member read, a local, parameter or field of the same
// name (JLS 6.4.2: a variable obscures a type).

// relateTypeName records a type name as written as a use by owner.
func (w *astWalker) relateTypeName(name string, owner int) {
	if owner == noOwner || strings.ContainsAny(name, "@<[") {
		return
	}
	head := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		head = name[:i]
	}
	if w.isTypeParam(head) {
		return
	}
	w.relateCandidates(owner, typeCandidates(name, w.pkg, w.typeStack, w.importMap, w.wildcards))
}

// relateTypeUses records every type a type node names, its type arguments, bounds
// and array element included, as a use by owner. Annotations on the type are the
// annotation pass's.
func (w *astWalker) relateTypeUses(n *sitter.Node, owner int) {
	if n == nil || owner == noOwner {
		return
	}
	switch kindOf(n) {
	case "type_identifier", "scoped_type_identifier":
		w.relateTypeName(nodeText(n, w.src), owner)
		return
	case "annotation", "marker_annotation":
		return
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		w.relateTypeUses(n.NamedChild(i), owner)
	}
}

// relateParamTypes records the declared types of a parameter list (a method's,
// a constructor's or a record's components) as uses by owner.
func (w *astWalker) relateParamTypes(params *sitter.Node, owner int) {
	if params == nil {
		return
	}
	for i := uint(0); i < params.NamedChildCount(); i++ {
		p := params.NamedChild(i)
		switch kindOf(p) {
		case "formal_parameter":
			w.relateTypeUses(p.ChildByFieldName("type"), owner)
		case "spread_parameter":
			for j := uint(0); j < p.NamedChildCount(); j++ {
				if c := p.NamedChild(j); kindOf(c) != "modifiers" && kindOf(c) != "variable_declarator" {
					w.relateTypeUses(c, owner)
				}
			}
		}
	}
}

// relateClassLiteral records `X.class` as a use of X by owner.
func (w *astWalker) relateClassLiteral(n *sitter.Node, owner int) {
	if t := firstNamedChild(n); t != nil {
		w.relateTypeUses(t, owner)
	}
}

// relateStaticRead records the qualifier of `Q.member` as a use of the type Q by
// owner, when Q is a name (`Param`, `Keys.Nested`, `com.example.Keys`) and not a
// variable in scope.
func (w *astWalker) relateStaticRead(n *sitter.Node, owner int) {
	obj := n.ChildByFieldName("object")
	if obj == nil || !isNameChain(obj) {
		return
	}
	name := nodeText(obj, w.src)
	head := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		head = name[:i]
	}
	if w.isVariable(head) {
		return
	}
	w.relateTypeName(strings.Join(strings.Fields(name), ""), owner)
}

// isNameChain reports whether n is an identifier or a dotted chain of them.
func isNameChain(n *sitter.Node) bool {
	switch kindOf(n) {
	case "identifier":
		return true
	case "field_access":
		o, f := n.ChildByFieldName("object"), n.ChildByFieldName("field")
		return o != nil && f != nil && kindOf(f) == "identifier" && isNameChain(o)
	}
	return false
}

// isVariable reports whether name is a variable the current scope declares: a
// local or parameter of the enclosing method, or a field of an enclosing class.
// Method scope is read whole rather than block by block, which can only drop a
// reference, never invent one.
func (w *astWalker) isVariable(name string) bool {
	if w.scopeNode != nil && w.scopeVars == nil {
		w.scopeVars = map[string]bool{}
		collectVariableNames(w.scopeNode, w.src, w.scopeVars)
	}
	if w.scopeVars[name] {
		return true
	}
	for i := len(w.fieldNames) - 1; i >= 0; i-- {
		if w.fieldNames[i][name] {
			return true
		}
	}
	return false
}

// collectVariableNames adds every variable n's subtree declares to into, nested
// type bodies excluded (their members are not in scope here).
func collectVariableNames(n *sitter.Node, src []byte, into map[string]bool) {
	switch kindOf(n) {
	case "class_body", "interface_body", "enum_body":
		return
	case "variable_declarator", "formal_parameter", "catch_formal_parameter",
		"enhanced_for_statement", "resource", "spread_parameter":
		if name := n.ChildByFieldName("name"); name != nil {
			into[nodeText(name, src)] = true
		}
	case "lambda_expression":
		if p := n.ChildByFieldName("parameters"); p != nil && kindOf(p) == "identifier" {
			into[nodeText(p, src)] = true
		}
	case "inferred_parameters":
		for i := uint(0); i < n.NamedChildCount(); i++ {
			into[nodeText(n.NamedChild(i), src)] = true
		}
	case "identifier":
		// A pattern variable: `o instanceof Foo f`, `case Foo f ->`.
		if p := n.Parent(); p != nil && (kindOf(p) == "type_pattern" || kindOf(p) == "instanceof_expression") {
			into[nodeText(n, src)] = true
		}
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		collectVariableNames(n.NamedChild(i), src, into)
	}
}

// fieldNamesOf returns the fields and enum constants a class body declares.
func fieldNamesOf(body *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	if body == nil {
		return out
	}
	for i := uint(0); i < body.NamedChildCount(); i++ {
		c := body.NamedChild(i)
		switch kindOf(c) {
		case "field_declaration", "constant_declaration":
			for j := uint(0); j < c.NamedChildCount(); j++ {
				if d := c.NamedChild(j); kindOf(d) == "variable_declarator" {
					if name := d.ChildByFieldName("name"); name != nil {
						out[nodeText(name, src)] = true
					}
				}
			}
		case "enum_constant":
			if name := c.ChildByFieldName("name"); name != nil {
				out[nodeText(name, src)] = true
			}
		case "enum_body_declarations":
			for k, v := range fieldNamesOf(c, src) {
				out[k] = v
			}
		}
	}
	return out
}

// typeParamNames returns the names a type_parameters node declares.
func typeParamNames(n *sitter.Node, src []byte) []string {
	if n == nil {
		return nil
	}
	var out []string
	for i := uint(0); i < n.NamedChildCount(); i++ {
		p := n.NamedChild(i)
		if kindOf(p) != "type_parameter" {
			continue
		}
		for j := uint(0); j < p.NamedChildCount(); j++ {
			if c := p.NamedChild(j); kindOf(c) == "type_identifier" || kindOf(c) == "identifier" {
				out = append(out, nodeText(c, src))
				break
			}
		}
	}
	return out
}

func (w *astWalker) isTypeParam(name string) bool {
	for _, p := range w.typeParams {
		if p == name {
			return true
		}
	}
	return false
}
