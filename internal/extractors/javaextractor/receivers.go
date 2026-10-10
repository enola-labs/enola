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
// classes, the return type of one of their methods (`getRepo().save(x)`), a
// parameter, a local with a written type, `var x = new T(…)`, the
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
	ambiguous := make(map[string]bool)
	for i := uint(0); i < body.NamedChildCount(); i++ {
		c := body.NamedChild(i)
		switch kindOf(c) {
		case "field_declaration":
			w.bindDeclared(out, c.ChildByFieldName("type"), c)
		case "method_declaration":
			// What a method of the class returns, under `name()`: the type of
			// `getJdbcTemplate().execute(…)`'s receiver. Overloads that return
			// different types give none.
			name := c.ChildByFieldName("name")
			if name == nil {
				continue
			}
			key := nodeText(name, w.src) + "()"
			t := w.receiverTypeOf(c.ChildByFieldName("type"))
			if prev, seen := out[key]; t == "" || ambiguous[key] || (seen && prev != t) {
				ambiguous[key] = true
				delete(out, key)
				continue
			}
			out[key] = t
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
	case "method_invocation":
		// `getRepo().save(x)`, `this.getRepo().save(x)`: a method of an enclosing
		// class, by its declared return type.
		if o := obj.ChildByFieldName("object"); o == nil || kindOf(o) == "this" {
			if n := obj.ChildByFieldName("name"); n != nil {
				key := nodeText(n, w.src) + "()"
				for i := len(w.fieldTypes) - 1; i >= 0; i-- {
					if t, ok := w.fieldTypes[i][key]; ok {
						return t
					}
				}
			}
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
//
// A receiver whose type the repository does not declare is a library's, and the
// call is classed by that library's member: named in io_calls, or, when it is in
// a loop and does no I/O, in pure_calls, which the performance analyzer takes
// over a reading of the name.
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

	liftIOTypes(all, supers, ioType)

	// Which declarations make a library I/O call, before any is marked: see
	// overloadedWithoutIO.
	makesIO := make(map[int]bool)
	for i := range all {
		calls, _ := all[i].PropAny(propTypedCalls).([]typedCall)
		for _, c := range calls {
			if _, known := typeIndex[c.typ]; !known && javaLibraryCall(c.typ, c.method) == javaCallIO {
				makesIO[i] = true
				break
			}
		}
	}
	mixed := overloadedWithoutIO(all, makesIO)

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
		pureCalls := make(map[string]bool)
		for _, c := range calls {
			typ, known := typeIndex[c.typ]
			if !known {
				// A library's type: the call is what that library's member is
				// (ioprim.go).
				switch javaLibraryCall(c.typ, c.method) {
				case javaCallIO:
					ioCalls[c.written] = true
				case javaCallPure:
					pureCalls[c.written] = true
				}
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
		// A call that does no I/O is worth recording only where its name would be
		// read: in a loop.
		var pure []string
		for _, key := range []string{"calls_in_loop", "calls_in_scaling_loop"} {
			if list, ok := f.PropAny(key).([]string); ok {
				for j, c := range list {
					if to, ok := rename[c]; ok {
						list[j] = to
					} else if pureCalls[c] && !ioCalls[c] {
						pureCalls[c] = false
						pure = append(pure, c)
					}
				}
			}
		}
		if len(pure) > 0 {
			sort.Strings(pure)
			f.SetProp("pure_calls", pure)
		}
		if len(ioCalls) > 0 {
			names := make([]string, 0, len(ioCalls))
			for c := range ioCalls {
				names = append(names, c)
			}
			sort.Strings(names)
			// A call on a type of this repository whose every method is a round
			// trip marks the name as before. One on a library type does so unless
			// the name is overloaded and an overload makes none.
			if !makesIO[i] || !mixed[f.Name] {
				f.SetProp("io_direct", true)
			}
			f.SetProp("io_calls", names)
		}
	}
}

// overloadedWithoutIO returns the names borne by several declarations of which at
// least one makes no library I/O call and is not io_direct already.
//
// Overloads share one fact name, and performs_io travels by name. `toJsonNode(Path)`
// reads a file and `toJsonNode(String)` parses a string: marking the name
// io_direct for the first would make every caller of the second reach I/O, and a
// utility class has exactly this shape. So a library call makes an overloaded
// name io_direct only when every overload makes one, is io_direct already, or
// delegates to another overload. The call is still named in io_calls on the
// overload that makes it.
func overloadedWithoutIO(all []facts.Fact, makesIO map[int]bool) map[string]bool {
	count := make(map[string]int)
	io := make(map[string]int)
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		count[f.Name]++
		// An overload that hands on to another of the same name does what that
		// one does: `execute(String...)` looping over `execute(String, boolean)`.
		if direct, _ := f.PropAny("io_direct").(bool); direct || makesIO[i] || f.HasRelation(facts.RelCalls, f.Name) {
			io[f.Name]++
		}
	}
	out := make(map[string]bool)
	for name, n := range count {
		if n > 1 && io[name] < n {
			out[name] = true
		}
	}
	return out
}

// liftIOTypes marks an interface as an io_type when every type that extends it
// is one, and its methods as io_direct.
//
//	interface EventRepository<T> { void removeEvents(…); }
//	interface ErrorEventRepository extends EventRepository<ErrorEvent>, JpaRepository<…> {}
//
// The shared interface is no Spring Data repository itself, so its methods
// carried no I/O, and a call through it (`getEventRepository(type).removeEvents(…)`)
// reached none: the queries are declared on the interfaces below it, which
// nothing calls by name. Where every one of those is a repository, a Feign client
// or a DAO, a method of the shared interface is a round trip whichever is behind
// it.
func liftIOTypes(all []facts.Fact, supers map[string][]string, ioType map[string]bool) {
	isInterface := make(map[string]bool)
	for i := range all {
		if k, _ := all[i].PropAny("symbol_kind").(string); k == "interface" {
			isInterface[all[i].Name] = true
		}
	}
	children := make(map[string][]string)
	for child, parents := range supers {
		for _, p := range parents {
			children[p] = append(children[p], child)
		}
	}
	lifted := make(map[string]bool)
	for changed := true; changed; {
		changed = false
		for t, kids := range children {
			if ioType[t] || !isInterface[t] {
				continue
			}
			every := true
			for _, k := range kids {
				if !ioType[k] {
					every = false
					break
				}
			}
			if every {
				ioType[t], lifted[t], changed = true, true, true
			}
		}
	}
	if len(lifted) == 0 {
		return
	}
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if lifted[f.Name] {
			f.SetProp(propIOType, true)
			continue
		}
		if dot := strings.LastIndexByte(f.Name, '.'); dot > 0 && lifted[f.Name[:dot]] {
			if k, _ := f.PropAny("symbol_kind").(string); k == "method" {
				f.SetProp("io_direct", true)
			}
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
