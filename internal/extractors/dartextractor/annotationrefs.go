package dartextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// relDartTypeRef carries a type name an annotation argument spells, from the walk to
// resolveCallTargets. The walker cannot tell `Users` the table class from a constant
// it does not know, so the relation keeps the short name until the library's types
// are indexed: a name that binds to a declared type becomes a RelNames edge, as Java
// records an annotation's class literal, and any other name is dropped. RelNames, not
// a call: a use that keeps the type out of the dead-code report and out of coupling.
const relDartTypeRef = "dart_type_ref"

// constructionReferences records the classes an expression outside a function body
// builds: a parameter default, a class-body field initializer, an annotation argument.
// Function bodies record the same constructions in scanCallChain, with the same
// constructor rule. Named constructors (`Foo.named()`) need the body walk's receiver
// scope to bind and are left to it.
func (w *walker) constructionReferences(n *sitter.Node, shadows map[string]bool, b *bodyWalk) {
	if n == nil {
		return
	}
	if dartLexicalScope(n) {
		local := make(map[string]bool, len(shadows))
		for name, bound := range shadows {
			local[name] = bound
		}
		dartBoundNames(n, w.src, local)
		if kindOf(n) == "local_function_declaration" {
			if params := firstOfKind(n, "formal_parameter_list"); params != nil {
				dartBoundNames(params, w.src, local)
			}
		}
		shadows = local
	}
	switch kindOf(n) {
	case "const_object_expression", "new_expression":
		b.addRelation(facts.RelInstantiates, dartCreatedType(n, w.src))
	}
	kids := namedChildren(n)
	for i, c := range kids {
		if kindOf(c) != "selector" || childOfKind(c, "argument_part") == nil {
			continue
		}
		if name, receiver, baseIsType := w.calleeOf(kids, i); name != "" && w.constructorInvocation(kids, i, name, receiver, baseIsType, shadows) {
			b.addRelation(facts.RelInstantiates, name)
		}
	}
	for _, c := range kids {
		w.constructionReferences(c, shadows, b)
	}
}

// dartCreatedType is the class a `const`/`new` expression builds. The grammar
// spells an import prefix as a type: `const i0.Value()` is two type_identifiers,
// so the last one is the class, and `new p.Foo()` is a type_identifier and an
// identifier, which is also how `new Foo.named()` and `const bool.fromEnvironment()`
// read, so the identifier is the class only when it is capitalised and the first
// name is not.
func dartCreatedType(n *sitter.Node, src []byte) string {
	name := ""
	for _, c := range namedChildren(n) {
		switch kindOf(c) {
		case "type_identifier":
			name = c.Utf8Text(src)
			continue
		case "identifier":
			if text := c.Utf8Text(src); name != "" && !namesAType(name) && namesAType(text) {
				return text
			}
		}
		if name != "" {
			break
		}
	}
	return name
}

// constructorInvocation is the rule that reads `Name(...)` or `Name<T>(...)` at
// kids[i] as building class Name rather than calling a value of that spelling.
func (w *walker) constructorInvocation(kids []*sitter.Node, i int, name, receiver string, baseIsType bool, shadows map[string]bool) bool {
	previous := i - 1
	if previous >= 0 && kindOf(kids[previous]) == "type_arguments" {
		previous--
	}
	if !baseIsType || receiver != name || previous < 0 || kindOf(kids[previous]) == "selector" || shadows[name] || w.callableValue(name) != "" {
		return false
	}
	_, field := w.fieldTypes[name]
	return !field
}

// fileValueWalk is the collector whose relations land on the file's reference-only
// fact: what a declaration without a symbol of its own names.
func (w *walker) fileValueWalk() *bodyWalk {
	if w.fieldValues == nil {
		w.fieldValues = &bodyWalk{seenRel: map[string]bool{}}
	}
	return w.fieldValues
}

// annotationReferences records what the annotations on a declaration name in their
// arguments: `@DriftDatabase(tables: [Users])` registers a table class only there,
// `@JsonKey(fromJson: _parse)` passes a function, `@Default(Mode())` builds a value.
// It finds the annotations in n's own syntax (its signature and parameters) and stops
// at bodies, which have their own walks. The caller decides which fact owns the uses
// and which member scope is in force, as Dart resolves member metadata in the class.
func (w *walker) annotationReferences(n *sitter.Node, b *bodyWalk) {
	if n == nil {
		return
	}
	switch kindOf(n) {
	case "annotation":
		if args := childOfKind(n, "arguments"); args != nil {
			// Metadata is constant, so a function invocation there does not compile;
			// only the value pass's tear-offs are kept.
			values := &bodyWalk{seenRel: map[string]bool{}}
			w.valueReferences(args, map[string]bool{}, values)
			for _, r := range values.relations {
				if r.Kind != facts.RelCalls {
					b.addRelation(r.Kind, r.Target)
				}
			}
			w.constructionReferences(args, map[string]bool{}, b)
			w.annotationTypeNames(args, b)
		}
		return
	case "class_body", "extension_body", "enum_body", "function_body", "block", "function_expression":
		return
	}
	for _, c := range namedChildren(n) {
		w.annotationReferences(c, b)
	}
}

// annotationTypeNames records the capitalised names an annotation argument spells as
// values: a list element, an argument, or the base of a static read (`Mode.fast`).
// A construction is recorded by constructionReferences; a label is not a value.
func (w *walker) annotationTypeNames(n *sitter.Node, b *bodyWalk) {
	if kindOf(n) == "identifier" {
		name := n.Utf8Text(w.src)
		parent := kindOf(n.Parent())
		if namesAType(name) && parent != "label" && !strings.HasSuffix(parent, "assignable_selector") && !dartInvocationTarget(n) && w.callableValue(name) == "" {
			b.addRelation(relDartTypeRef, name)
		}
	}
	for _, c := range namedChildren(n) {
		w.annotationTypeNames(c, b)
	}
}
