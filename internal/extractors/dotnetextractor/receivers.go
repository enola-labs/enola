package dotnetextractor

import (
	"sort"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Calls on a receiver whose type the source declares.
//
//	private readonly ILibraryManager _libraryManager;   // a field
//	public IFileSystem FileSystem { get; }              // a property
//	class Saver(IItemRepository items) { … }            // a primary-constructor parameter
//	void Run(IEnumerable<Item> batch, ILogger log)      // a parameter
//	Folder parent = FindParent(item);                   // a local
//
// handleInvocation refuses to guess a receiver's type, and that stands: a call on
// a receiver of unknown type is still recorded bare. This is the other case, where
// nothing has to be guessed because the declaration says it. `_libraryManager.
// DeleteItem(…)` is a call on an ILibraryManager, and an edge to
// ILibraryManager.DeleteItem is what lets anything that follows calls edges get
// from a provider to the library behind the interface.
//
// The walker records the receiver's type as written and the method name;
// resolveTypedCalls binds the type like any other reference and finds the
// declaration, on the type or on a base type or interface of it.

// propTypedCalls is the walker's note to resolveTypedCalls. It never leaves the
// extractor.
const propTypedCalls = "typed_calls"

// typedCall is one call whose receiver type is declared.
type typedCall struct {
	written string // as the in-loop metric records it: `_libraryManager.DeleteItem`
	typ     string // the receiver's type as the source names it
	method  string
	// base marks `base.M(…)`: the receiver is the enclosing type's base, and typ
	// is empty.
	base bool
}

// collectMemberTypes maps the fields, properties and primary-constructor
// parameters of a type declaration to their declared types.
func (w *astWalker) collectMemberTypes(decl, body *sitter.Node) map[string]string {
	out := make(map[string]string)
	if params := findChildByKind(decl, "parameter_list"); params != nil {
		w.bindParameterList(out, params)
	}
	if body == nil {
		return out
	}
	for i := uint(0); i < body.NamedChildCount(); i++ {
		m := body.NamedChild(i)
		switch kindOf(m) {
		case "field_declaration":
			if vd := findChildByKind(m, "variable_declaration"); vd != nil {
				w.bindVariableDeclaration(out, vd)
			}
		case "property_declaration":
			name := m.ChildByFieldName("name")
			if t := w.receiverTypeOf(m.ChildByFieldName("type")); t != "" && name != nil {
				out[nodeText(name, w.src)] = t
			}
		}
	}
	return out
}

// receiverTypeOf is targetForType for a declaration a call may be made on. An
// array is not its element.
func (w *astWalker) receiverTypeOf(typeNode *sitter.Node) string {
	if typeNode == nil || kindOf(typeNode) == "array_type" {
		return ""
	}
	return w.targetForType(typeNode)
}

func (w *astWalker) bindParameterList(into map[string]string, params *sitter.Node) {
	for i := uint(0); i < params.NamedChildCount(); i++ {
		p := params.NamedChild(i)
		if kindOf(p) != "parameter" {
			continue
		}
		name := p.ChildByFieldName("name")
		if name == nil {
			continue
		}
		if t := w.receiverTypeOf(p.ChildByFieldName("type")); t != "" {
			into[nodeText(name, w.src)] = t
		} else {
			delete(into, nodeText(name, w.src))
		}
	}
}

// bindVariableDeclaration records each declarator of `T a = …, b;`. With `var`
// the one accepted initialiser is a `new T(…)`, which states the type as plainly
// as a declaration does.
func (w *astWalker) bindVariableDeclaration(into map[string]string, vd *sitter.Node) {
	typeNode := vd.ChildByFieldName("type")
	declared := w.receiverTypeOf(typeNode)
	implicit := typeNode != nil && kindOf(typeNode) == "implicit_type"
	for i := uint(0); i < vd.NamedChildCount(); i++ {
		d := vd.NamedChild(i)
		if kindOf(d) != "variable_declarator" {
			continue
		}
		name := d.ChildByFieldName("name")
		if name == nil {
			name = d.NamedChild(0)
		}
		if name == nil || kindOf(name) != "identifier" {
			continue
		}
		t := declared
		if implicit {
			t = ""
			if created := findCreation(d); created != nil {
				t = w.receiverTypeOf(created.ChildByFieldName("type"))
			}
		}
		if t != "" {
			into[nodeText(name, w.src)] = t
		} else {
			delete(into, nodeText(name, w.src)) // shadows a typed name with an untyped one
		}
	}
}

// findCreation returns the `new T(…)` a declarator is initialised with, when that
// is the whole initialiser.
func findCreation(declarator *sitter.Node) *sitter.Node {
	for i := uint(0); i < declarator.NamedChildCount(); i++ {
		c := declarator.NamedChild(i)
		if kindOf(c) == "equals_value_clause" && c.NamedChildCount() == 1 {
			c = c.NamedChild(0)
		}
		if kindOf(c) == "object_creation_expression" {
			return c
		}
	}
	return nil
}

// noteDeclaration records the locals a statement declares, as the walk passes it.
func (w *astWalker) noteDeclaration(node *sitter.Node, kind string) {
	if w.localTypes == nil {
		return
	}
	switch kind {
	case "local_declaration_statement", "using_statement":
		if vd := findChildByKind(node, "variable_declaration"); vd != nil {
			w.bindVariableDeclaration(w.localTypes, vd)
		}
	case "foreach_statement":
		left := node.ChildByFieldName("left")
		if left == nil || kindOf(left) != "identifier" {
			return
		}
		if t := w.receiverTypeOf(node.ChildByFieldName("type")); t != "" {
			w.localTypes[nodeText(left, w.src)] = t
		} else {
			delete(w.localTypes, nodeText(left, w.src))
		}
	}
}

// receiverType returns the declared type of a call's receiver, or "".
func (w *astWalker) receiverType(recv *sitter.Node) string {
	if recv == nil {
		return ""
	}
	switch kindOf(recv) {
	case "identifier":
		name := nodeText(recv, w.src)
		if t, ok := w.localTypes[name]; ok {
			return t
		}
		for i := len(w.memberTypes) - 1; i >= 0; i-- {
			if t, ok := w.memberTypes[i][name]; ok {
				return t
			}
		}
	case "member_access_expression":
		// `this._repo.Save(x)`
		if o := recv.ChildByFieldName("expression"); o != nil && kindOf(o) == "this_expression" && len(w.memberTypes) > 0 {
			if n := recv.ChildByFieldName("name"); n != nil {
				return w.memberTypes[len(w.memberTypes)-1][nodeText(n, w.src)]
			}
		}
	}
	return ""
}

// noteTypedCall records a call on a receiver of declared type against the member
// being walked.
func (w *astWalker) noteTypedCall(written, typ, method string) {
	if w.metrics == nil || typ == "" {
		return
	}
	w.metrics.typedCalls = append(w.metrics.typedCalls, typedCall{written: written, typ: typ, method: method})
}

// noteBaseCall records `base.M(…)` against the member being walked. The walker
// binds it to this type's own M, the only M it knows, so nothing that follows
// calls edges got from an override to what it extends: `base.Update(item)` saved
// the list to disk, and the override carried no I/O.
func (w *astWalker) noteBaseCall(method string) {
	if w.metrics == nil {
		return
	}
	w.metrics.typedCalls = append(w.metrics.typedCalls, typedCall{written: "base." + method, method: method, base: true})
}

// resolveTypedCalls turns the walker's notes into calls edges. resolve binds a
// type name as written from a namespace, as it does for every other reference.
//
// The method is looked for on the type and then up its base types and interfaces,
// nearest first, and the edge is made only when it lands on a declaration. The
// same name replaces the written text in the in-loop lists, so the performance
// analyzer reads a resolved callee where it read `_libraryManager.DeleteItem`.
//
// A receiver whose type does not resolve is a library's, and the call is classed
// by that library's member: named in io_calls, which makes the member io_direct,
// or, when it is in a loop and does no I/O, in pure_calls, which the performance
// analyzer takes over a reading of the name.
func resolveTypedCalls(all []facts.Fact, resolve func(target, fromNS string) (string, bool)) {
	exists := make(map[string]bool, len(all))
	supers := make(map[string][]string)
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		exists[f.Name] = true
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

	// Which declarations make a library I/O call, before any is marked: see
	// overloadedWithoutIO.
	makesIO := make(map[int]bool)
	for i := range all {
		calls, _ := all[i].PropAny(propTypedCalls).([]typedCall)
		fromNS, _ := all[i].PropAny("namespace").(string)
		for _, c := range calls {
			if c.base {
				continue
			}
			if _, known := resolve(c.typ, fromNS); !known && csLibraryCall(c.typ, c.method) == csCallIO {
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
		fromNS, _ := f.PropAny("namespace").(string)
		rename := make(map[string]string)
		ioCalls := make(map[string]bool)
		pureCalls := make(map[string]bool)
		for _, c := range calls {
			if c.base {
				// The nearest declaration above the enclosing type.
				if dot := strings.LastIndexByte(f.Name, '.'); dot > 0 {
					for _, super := range supers[f.Name[:dot]] {
						if target := declaring(super, c.method); target != "" && target != f.Name {
							if !f.HasRelation(facts.RelCalls, target) {
								f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
							}
							break
						}
					}
				}
				continue
			}
			typ, known := resolve(c.typ, fromNS)
			if !known {
				// A library's type: the call is what that library's member is
				// (ioprim.go).
				switch csLibraryCall(c.typ, c.method) {
				case csCallIO:
					ioCalls[c.written] = true
				case csCallPure:
					pureCalls[c.written] = true
				}
				continue
			}
			target := declaring(typ, c.method)
			if target == "" || target == f.Name {
				continue
			}
			if !f.HasRelation(facts.RelCalls, target) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
			}
			rename[c.written] = target
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
			if !mixed[f.Name] {
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
