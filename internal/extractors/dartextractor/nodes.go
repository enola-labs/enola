package dartextractor

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// This file holds the tree-sitter navigation helpers. They are collected here rather
// than inlined because the Dart grammar has two shapes that every caller has to cope
// with, and coping with them ad hoc is how a walker acquires subtle inconsistencies:
//
//  1. A signature and its body are SIBLINGS, not parent and child (nextBody).
//  2. An annotation is a sibling PRECEDING what it annotates inside a class body, but a
//     CHILD of the declaration at file scope (annotationsBefore / annotationNames).

// namedChildren returns a node's named children in source order.
func namedChildren(n *sitter.Node) []*sitter.Node {
	if n == nil {
		return nil
	}
	count := n.NamedChildCount()
	out := make([]*sitter.Node, 0, count)
	for i := uint(0); i < count; i++ {
		if c := n.NamedChild(i); c != nil {
			out = append(out, c)
		}
	}
	return out
}

// lineOf returns a node's 1-based start line.
func lineOf(n *sitter.Node) int {
	if n == nil {
		return 0
	}
	return int(n.StartPosition().Row) + 1
}

// childOfKind returns the first direct named child of any of the given kinds.
func childOfKind(n *sitter.Node, kinds ...string) *sitter.Node {
	if n == nil {
		return nil
	}
	for _, c := range namedChildren(n) {
		for _, k := range kinds {
			if kindOf(c) == k {
				return c
			}
		}
	}
	return nil
}

// firstOfKind searches the subtree (breadth-first over named nodes) for the first node
// of any of the given kinds.
func firstOfKind(n *sitter.Node, kinds ...string) *sitter.Node {
	if n == nil {
		return nil
	}
	want := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		want[k] = true
	}
	queue := []*sitter.Node{n}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range namedChildren(cur) {
			if want[kindOf(c)] {
				return c
			}
			queue = append(queue, c)
		}
	}
	return nil
}

// identifierChild returns the first direct `identifier` child's text.
func identifierChild(n *sitter.Node, src []byte) string {
	for _, c := range namedChildren(n) {
		if kindOf(c) == "identifier" {
			return c.Utf8Text(src)
		}
	}
	return ""
}

// identifierNames returns every `identifier` declared in a variable-list subtree, which
// is how Dart spells `int a, b, c` and `final x = 1`.
func identifierNames(n *sitter.Node, src []byte) []string {
	var out []string
	var visit func(*sitter.Node)
	visit = func(node *sitter.Node) {
		switch kindOf(node) {
		case "initialized_identifier", "static_final_declaration":
			if name := identifierChild(node, src); name != "" {
				out = append(out, name)
			}
			return
		case "identifier":
			out = append(out, node.Utf8Text(src))
			return
		}
		for _, c := range namedChildren(node) {
			visit(c)
		}
	}
	for _, c := range namedChildren(n) {
		visit(c)
	}
	return out
}

// nextBody returns the `function_body` that follows kids[i], or nil.
//
// The absence of a body is meaningful rather than merely uninteresting: it is how Dart
// spells an abstract member, and the abstractness rule reads it. So this deliberately
// looks only at the IMMEDIATELY following sibling — scanning further ahead would happily
// pair an abstract signature with the next concrete member's body.
func nextBody(kids []*sitter.Node, i int) *sitter.Node {
	if i+1 >= len(kids) {
		return nil
	}
	if kindOf(kids[i+1]) == "function_body" {
		return kids[i+1]
	}
	return nil
}

// annotationsBefore collects the annotation names immediately preceding kids[i].
func annotationsBefore(kids []*sitter.Node, i int, src []byte) []string {
	var out []string
	for j := i - 1; j >= 0; j-- {
		if kindOf(kids[j]) != "annotation" {
			break
		}
		if name := annotationName(kids[j], src); name != "" {
			out = append(out, name)
		}
	}
	// Reversed by the backwards scan; restore source order so the prop is stable.
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out
}

// annotationNames collects annotations that are direct children of a declaration.
func annotationNames(n *sitter.Node, src []byte) []string {
	var out []string
	for _, c := range namedChildren(n) {
		if kindOf(c) != "annotation" {
			continue
		}
		if name := annotationName(c, src); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// annotationName reduces `@Foo(bar)` and `@foo` to `Foo` / `foo`.
func annotationName(n *sitter.Node, src []byte) string {
	for _, c := range namedChildren(n) {
		switch kindOf(c) {
		case "identifier", "type_identifier":
			return c.Utf8Text(src)
		}
	}
	txt := strings.TrimPrefix(strings.TrimSpace(n.Utf8Text(src)), "@")
	if i := strings.IndexAny(txt, "(. \t\n"); i > 0 {
		txt = txt[:i]
	}
	return txt
}

// supertypeNames returns every type named by `extends`, `with` and `implements`.
//
// All three collapse into one `implements` edge, which is enola's model: the graph has
// a single conformance relation and does not distinguish inheritance from mixin
// application from interface implementation.
func supertypeNames(n *sitter.Node, src []byte) []string {
	var out []string
	seen := map[string]bool{}
	add := func(node *sitter.Node) {
		var visit func(*sitter.Node)
		visit = func(x *sitter.Node) {
			if kindOf(x) == "type_identifier" {
				name := x.Utf8Text(src)
				// A type argument is not a supertype: `extends State<HomePage>`
				// conforms to State, not to HomePage.
				if name != "" && !seen[name] {
					seen[name] = true
					out = append(out, name)
				}
				return
			}
			if kindOf(x) == "type_arguments" {
				return
			}
			for _, c := range namedChildren(x) {
				visit(c)
			}
		}
		visit(node)
	}
	for _, c := range namedChildren(n) {
		switch kindOf(c) {
		case "superclass", "interfaces", "mixins", "enum_interfaces":
			add(c)
		}
	}
	return out
}

// signatureName returns the declared name from a function/getter/setter signature.
//
// It takes the LAST direct identifier rather than the first, because a signature leads
// with its return type: in `Future<void> run(String id)` the identifiers encountered
// are the type's and then the method's. Type arguments are skipped for the same reason.
func signatureName(n *sitter.Node, src []byte) string {
	name := ""
	for _, c := range namedChildren(n) {
		switch kindOf(c) {
		case "identifier":
			name = c.Utf8Text(src)
		case "formal_parameter_list":
			// Everything after the parameter list is body or initializers.
			return name
		}
	}
	return name
}

// constructorName returns the member name for a constructor: the type's own name for an
// unnamed constructor, `<Type>.<name>`'s tail for a named one.
//
// Both forms are given the type name as their prefix by the caller, so an unnamed
// constructor becomes `dir.User.User`. That is deliberate: it keeps every member of a
// type inside the type's namespace, so `has_method` and the declares edges stay uniform,
// and it makes the unnamed constructor addressable at all.
func constructorName(n *sitter.Node, typeName string, src []byte) string {
	var ids []string
	for _, c := range namedChildren(n) {
		switch kindOf(c) {
		case "identifier":
			ids = append(ids, c.Utf8Text(src))
		case "formal_parameter_list":
			goto done
		}
	}
done:
	if len(ids) >= 2 {
		return ids[0] + "." + ids[1]
	}
	if len(ids) == 1 {
		// A concise named constructor (`new named()`, `factory named()`) spells the
		// name without the type; it is the same constructor as `Type.named()`.
		if ids[0] != typeName {
			return typeName + "." + ids[0]
		}
		return ids[0]
	}
	return typeName
}

// hasToken reports whether a node has a direct child token with the given text, which
// is how the grammar represents `static` and `factory` (unnamed tokens rather than
// named nodes, unlike `abstract` and `sealed`).
func hasToken(n *sitter.Node, token string) bool {
	if n == nil {
		return false
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c != nil && !c.IsNamed() && kindOf(c) == token {
			return true
		}
	}
	// `static` sits on the enclosing `declaration`, not on the signature, so check one
	// level down too.
	for _, c := range namedChildren(n) {
		for i := uint(0); i < c.ChildCount(); i++ {
			g := c.Child(i)
			if g != nil && !g.IsNamed() && kindOf(g) == token {
				return true
			}
		}
	}
	return false
}

// isAsync reports whether a function body is `async` or `async*`.
func isAsync(body *sitter.Node) bool {
	if body == nil {
		return false
	}
	for i := uint(0); i < body.ChildCount(); i++ {
		c := body.Child(i)
		if c != nil && !c.IsNamed() && (kindOf(c) == "async" || kindOf(c) == "async*") {
			return true
		}
	}
	return false
}

func isUpper(b byte) bool { return b >= 'A' && b <= 'Z' }

// namesAType reports whether an identifier looks like a type name.
//
// Leading underscores are skipped first, because Dart spells privacy with one and
// Flutter leans on it constantly: the State class behind every StatefulWidget is
// conventionally `_FooState`. Testing the raw first byte classifies all of them as
// lowercase, so `_FooState()` reads as a function call rather than a construction —
// which loses the instantiates edge on the single most common private type in the
// language.
func namesAType(s string) bool {
	i := 0
	for i < len(s) && s[i] == '_' {
		i++
	}
	return i < len(s) && isUpper(s[i])
}

// builtinTypes are the types that are never a collaborator, so an `injects` edge to one
// would be noise rather than a dependency.
var builtinTypes = map[string]bool{
	"String": true, "int": true, "double": true, "num": true, "bool": true,
	"List": true, "Map": true, "Set": true, "Iterable": true, "Object": true,
	"Function": true, "Future": true, "Stream": true, "Duration": true,
	"DateTime": true, "Uri": true, "Key": true, "BuildContext": true,
	"Widget": true, "Color": true, "Type": true, "Symbol": true, "Null": true,
	"Record": true, "Comparable": true, "Pattern": true, "RegExp": true,
	"StringBuffer": true, "Exception": true, "Error": true, "Enum": true,
}

func isBuiltinType(t string) bool { return builtinTypes[t] }
