package javaextractor

import (
	"sort"
	"strings"
	"unicode"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Calls on a receiver whose type the source declares.
//
//	private final UserRepository userRepository;     // a field
//	void sync(DeviceService devices) { … }           // a parameter
//	Device d = devices.findById(id);                 // a local
//
// A call was an edge only when it was on `this`. `userRepository.findByEmail(…)`
// was recorded as the text it was written as, for the performance metric, and
// nowhere else: the method it reaches had one caller fewer than it has, and nothing
// that follows calls edges got from a service to its repository. Dependency
// injection is exactly this shape, so the call graph stopped at every layer.
//
// The receiver's type is read where it is declared: a field of the enclosing
// classes, a parameter, a local with a written type, `var x = new T(…)`, the
// variable of a for-each, and a capitalised name that is not a variable, which is
// a static call on a type. Nothing is inferred. A call on a call's result, on a
// lambda parameter or on a `var` initialised any other way stays unresolved.
//
// The walker cannot tell whether the type is one this repository declares, or
// which class in its hierarchy declares the method. It records the pair as
// written and resolveTypedCalls settles both once every file has been read.

// propTypedCalls is the walker's note to resolveTypedCalls. It never leaves the
// extractor.
const propTypedCalls = "typed_calls"

// typedCall is one call whose receiver type is known.
type typedCall struct {
	written string // as the in-loop metric records it: `userRepository.findByEmail`
	typ     string // the receiver type's fully-qualified name
	method  string
}

// collectFieldTypes maps the fields a class body declares to their types.
func (w *astWalker) collectFieldTypes(body *sitter.Node) map[string]string {
	out := make(map[string]string)
	if body == nil {
		return out
	}
	for i := uint(0); i < body.NamedChildCount(); i++ {
		if c := body.NamedChild(i); kindOf(c) == "field_declaration" {
			w.bindDeclared(out, c.ChildByFieldName("type"), c)
		}
	}
	return out
}

// bindDeclared records, for each declarator under decl, the declared type.
func (w *astWalker) bindDeclared(into map[string]string, typeNode, decl *sitter.Node) {
	t := w.receiverTypeOf(typeNode)
	for i := uint(0); i < decl.NamedChildCount(); i++ {
		d := decl.NamedChild(i)
		if kindOf(d) != "variable_declarator" {
			continue
		}
		name := d.ChildByFieldName("name")
		if name == nil || kindOf(name) != "identifier" {
			continue
		}
		dt := t
		// `var svc = new DeviceService(…)`: the one inference, and it is a statement
		// of the type as much as a declaration is.
		if dt == "" && typeNode != nil && nodeText(typeNode, w.src) == "var" {
			if v := d.ChildByFieldName("value"); v != nil && kindOf(v) == "object_creation_expression" {
				dt = w.receiverTypeOf(v.ChildByFieldName("type"))
			}
		}
		if dt != "" {
			into[nodeText(name, w.src)] = dt
		} else {
			delete(into, nodeText(name, w.src)) // shadows a typed name with an untyped one
		}
	}
}

// receiverTypeOf is targetForType for a declaration a call may be made on. An
// array is not its element: `devices.clone()` is not a method of Device.
func (w *astWalker) receiverTypeOf(typeNode *sitter.Node) string {
	if typeNode == nil || kindOf(typeNode) == "array_type" {
		return ""
	}
	return w.targetForType(typeNode)
}

// bindParameters records a method's typed parameters.
func (w *astWalker) bindParameters(method *sitter.Node) {
	params := method.ChildByFieldName("parameters")
	if params == nil {
		return
	}
	for i := uint(0); i < params.NamedChildCount(); i++ {
		p := params.NamedChild(i)
		if kindOf(p) != "formal_parameter" {
			continue
		}
		name := p.ChildByFieldName("name")
		if t := w.receiverTypeOf(p.ChildByFieldName("type")); t != "" && name != nil {
			w.localTypes[nodeText(name, w.src)] = t
		}
	}
}

// noteDeclaration records the locals a statement declares, as the walk passes it.
func (w *astWalker) noteDeclaration(node *sitter.Node, kind string) {
	if w.localTypes == nil {
		return
	}
	switch kind {
	case "local_variable_declaration":
		w.bindDeclared(w.localTypes, node.ChildByFieldName("type"), node)
	case "enhanced_for_statement":
		name := node.ChildByFieldName("name")
		if name == nil {
			return
		}
		if t := w.receiverTypeOf(node.ChildByFieldName("type")); t != "" {
			w.localTypes[nodeText(name, w.src)] = t
		} else {
			delete(w.localTypes, nodeText(name, w.src))
		}
	}
}

// receiverType returns the declared type of a call's receiver, or "".
func (w *astWalker) receiverType(obj *sitter.Node) string {
	switch kindOf(obj) {
	case "identifier":
		name := nodeText(obj, w.src)
		if t, ok := w.localTypes[name]; ok {
			return t
		}
		for i := len(w.fieldTypes) - 1; i >= 0; i-- {
			if t, ok := w.fieldTypes[i][name]; ok {
				return t
			}
		}
		// `Devices.parse(x)`: a static call names its type.
		if r := []rune(name); len(r) > 0 && unicode.IsUpper(r[0]) && !isScreamingSnake(name) {
			return w.fqnForSimple(name)
		}
	case "field_access":
		// `this.repo.save(x)`
		if o := obj.ChildByFieldName("object"); o != nil && kindOf(o) == "this" && len(w.fieldTypes) > 0 {
			if f := obj.ChildByFieldName("field"); f != nil {
				return w.fieldTypes[len(w.fieldTypes)-1][nodeText(f, w.src)]
			}
		}
	}
	return ""
}

// fqnForSimple resolves a simple type name the way targetForType does.
func (w *astWalker) fqnForSimple(simple string) string {
	if fqn, ok := w.importMap[simple]; ok {
		return fqn
	}
	if javaLangTypes[simple] {
		return ""
	}
	if w.pkg != "" {
		return w.pkg + "." + simple
	}
	return simple
}

// noteTypedCall records a call on a receiver of declared type against the method
// being walked.
func (w *astWalker) noteTypedCall(written, typ, method string) {
	if w.metrics == nil || typ == "" {
		return
	}
	w.metrics.typedCalls = append(w.metrics.typedCalls, typedCall{written, typ, method})
}

// resolveTypedCalls turns the walker's notes into calls edges, for the receivers
// whose type this repository declares. typeIndex maps a fully-qualified type name
// to its symbol.
//
// The method is looked for on the type and then up its supertypes, nearest first:
// a call on a field of type DeviceServiceImpl whose method AbstractService
// declares is an edge to AbstractService's. The edge lands on a declaration or is
// not made. The same name replaces the written text in the in-loop lists, so the
// performance analyzer reads a resolved callee where it read `repo.save`.
//
// One case has no declaration to land on and is still known: a method inherited
// by a Spring Data repository, a Feign client or a Room DAO from its framework
// base (`userRepository.save(…)`, where UserRepository declares only its
// finders). Every method of such a type is a round trip, so the caller is marked
// io_direct and the call is named in io_calls.
func resolveTypedCalls(all []facts.Fact, typeIndex map[string]string) {
	exists := make(map[string]bool, len(all))
	supers := make(map[string][]string)
	ioType := make(map[string]bool)
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		exists[f.Name] = true
		if b, _ := f.PropAny(propIOType).(bool); b {
			ioType[f.Name] = true
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImplements {
				supers[f.Name] = append(supers[f.Name], r.Target)
			}
		}
	}
	declaring := func(typ, method string) string {
		seen := map[string]bool{}
		for queue := []string{typ}; len(queue) > 0; {
			t := queue[0]
			queue = queue[1:]
			if seen[t] {
				continue
			}
			seen[t] = true
			if exists[t+"."+method] {
				return t + "." + method
			}
			queue = append(queue, supers[t]...)
		}
		return ""
	}

	for i := range all {
		f := &all[i]
		calls, ok := f.PropAny(propTypedCalls).([]typedCall)
		if !ok {
			continue
		}
		f.DelProp(propTypedCalls)
		have := make(map[string]bool)
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				have[r.Target] = true
			}
		}
		rename := make(map[string]string)
		ioCalls := make(map[string]bool)
		for _, c := range calls {
			typ, known := typeIndex[c.typ]
			if !known {
				continue
			}
			target := declaring(typ, c.method)
			switch {
			case target != "" && target != f.Name:
				if !have[target] {
					have[target] = true
					f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
				}
				rename[c.written] = target
			case target == "" && ioType[typ]:
				ioCalls[c.written] = true
			}
		}
		for _, key := range []string{"calls_in_loop", "calls_in_scaling_loop"} {
			if list, ok := f.PropAny(key).([]string); ok {
				for j, c := range list {
					if to, ok := rename[c]; ok {
						list[j] = to
					}
				}
			}
		}
		if len(ioCalls) > 0 {
			names := make([]string, 0, len(ioCalls))
			for c := range ioCalls {
				names = append(names, c)
			}
			sort.Strings(names)
			f.SetProp("io_direct", true)
			f.SetProp("io_calls", names)
		}
	}
}

// propIOType marks a type every method of which is a round trip: a Spring Data
// repository, a Feign client, a Room DAO.
const propIOType = "io_type"

// writtenCall is how the in-loop metric spells a call on a receiver.
func writtenCall(recv, name string) string {
	if recv == "" {
		return name
	}
	return strings.TrimSpace(recv) + "." + name
}
