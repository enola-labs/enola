package rustextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Calls on a receiver whose type the source states.
//
//	impl Handle {
//	    fn enqueue(me: &Arc<Self>, …) {
//	        me.schedule(n);                     // Handle's, through the Arc
//	        me.shared.registry.register(…);     // a field chain: Registry's
//	    }
//	}
//	let frame = Frame::<T, S>::from_raw(ptr);   // what from_raw returns
//	frame.poll();
//
// A method call was recorded by its bare name, which says a method of that name is
// used and nothing about whose. Where the receiver's type is written down there is
// nothing to infer: a parameter's or a let's declared type, `self` in an impl, a
// struct literal, a struct field's declared type, and what a function of the crate
// is declared to return. `&`, `Arc`, `Rc` and `Box` are seen through, as method
// calls auto-deref through them. Nothing else is guessed.
//
// A file cannot see the struct fields and return types the others declare, so the
// walker writes down the expression (a base type, then fields and calls) and
// resolveTypedCalls evaluates it once every file has been read. A step it cannot
// take leaves the call as it was: a missing edge beats a wrong one.

// The walker's notes to resolveTypedCalls. None leaves the extractor.
const (
	propTypedCalls = "rust_typed_calls"
	propFieldTypes = "rust_field_types"
	propReturns    = "rust_returns"
)

// rustStep is one step from a type to another: a field of it, or what a method or
// associated function of it returns.
type rustStep struct {
	call bool
	name string
}

// rustTypeExpr is a type the walker could not name yet: base, then each step.
type rustTypeExpr struct {
	base  string
	steps []rustStep
}

func (e rustTypeExpr) then(s rustStep) rustTypeExpr {
	steps := make([]rustStep, len(e.steps), len(e.steps)+1)
	copy(steps, e.steps)
	return rustTypeExpr{base: e.base, steps: append(steps, s)}
}

// rustTypedCall is one method call whose receiver's type is an expression.
type rustTypedCall struct {
	recv   rustTypeExpr
	method string
}

// rustDerefWrappers hold the value a method is called on: a call on an Arc<T> is a
// call on T.
var rustDerefWrappers = map[string]bool{"Arc": true, "Rc": true, "Box": true}

// rustPreludeTypes are the standard types code names without importing them; a
// receiver of one has no method of the crate's.
var rustPreludeTypes = map[string]bool{
	"Option": true, "Result": true, "Vec": true, "String": true, "Box": true,
	"Some": true, "None": true, "Ok": true, "Err": true,
}

// implType is the canonical name of the type of the impl block being walked.
func (w *astWalker) implType() string {
	if len(w.typeStack) == 0 {
		return ""
	}
	parts := append(append([]string{}, w.modStack...), w.typeStack...)
	return w.dir + "." + strings.Join(parts, ".")
}

// typeCandidate is the canonical name a type written by its simple name would
// have in this crate: Self's, an import's, or this module's.
func (w *astWalker) typeCandidate(name string) string {
	switch {
	case name == "":
		return ""
	case name == "Self":
		return w.implType()
	case !isCapitalized(name), rustPreludeTypes[name]:
		return ""
	}
	if target, ok := w.importMap[name]; ok {
		return target // "" for an import from outside the crate
	}
	return w.dir + "." + w.qualifyMod(name)
}

// typeOf is the canonical name of the type a type node spells, seen through
// references and the deref wrappers, or "".
func (w *astWalker) typeOf(node *sitter.Node) string {
	if node == nil {
		return ""
	}
	switch kindOf(node) {
	case "reference_type":
		return w.typeOf(node.ChildByFieldName("type"))
	case "generic_type":
		name := simpleTypeName(node.ChildByFieldName("type"), w.src)
		if rustDerefWrappers[name] {
			if args := node.ChildByFieldName("type_arguments"); args != nil && args.NamedChildCount() > 0 {
				return w.typeOf(args.NamedChild(0))
			}
			return ""
		}
		return w.typeOf(node.ChildByFieldName("type"))
	case "type_identifier":
		return w.typeCandidate(nodeText(node, w.src))
	case "scoped_type_identifier":
		// `task::Notified`: the module path names the directory.
		path := node.ChildByFieldName("path")
		name := node.ChildByFieldName("name")
		if path == nil || name == nil {
			return ""
		}
		if dir := w.modulePathDir(strings.Split(nodeText(path, w.src), "::")); dir != "" {
			// `worker::Shared` is declared in worker/mod.rs, in worker/, or in
			// worker.rs, in the directory above; with both a worker.rs and a
			// worker/ only the declarations can tell. See canonType.
			return rustEitherDir + dir + "." + nodeText(name, w.src)
		}
	}
	return ""
}

// exprType is the type of a value expression as far as the source states it.
func (w *astWalker) exprType(node *sitter.Node) (rustTypeExpr, bool) {
	if node == nil {
		return rustTypeExpr{}, false
	}
	switch kindOf(node) {
	case "self":
		if t := w.implType(); t != "" {
			return rustTypeExpr{base: t}, true
		}
	case "identifier":
		if t, ok := w.localTypes[nodeText(node, w.src)]; ok {
			return t, true
		}
	case "parenthesized_expression", "reference_expression":
		inner := node.ChildByFieldName("value")
		if inner == nil && node.NamedChildCount() > 0 {
			inner = node.NamedChild(0)
		}
		return w.exprType(inner)
	case "unary_expression":
		// `*guard`: a deref of what a wrapper holds.
		if node.ChildCount() > 0 && nodeText(node.Child(0), w.src) == "*" && node.NamedChildCount() > 0 {
			return w.exprType(node.NamedChild(0))
		}
	case "field_expression":
		field := node.ChildByFieldName("field")
		if field == nil || kindOf(field) != "field_identifier" {
			return rustTypeExpr{}, false
		}
		if recv, ok := w.exprType(node.ChildByFieldName("value")); ok {
			return recv.then(rustStep{name: nodeText(field, w.src)}), true
		}
	case "struct_expression":
		if t := w.typeCandidate(simpleTypeName(node.ChildByFieldName("name"), w.src)); t != "" {
			return rustTypeExpr{base: t}, true
		}
	case "call_expression":
		fn := node.ChildByFieldName("function")
		if fn != nil && kindOf(fn) == "generic_function" {
			fn = fn.ChildByFieldName("function")
		}
		if fn == nil {
			return rustTypeExpr{}, false
		}
		switch kindOf(fn) {
		case "field_expression":
			// `recv.method()`: what the method returns.
			field := fn.ChildByFieldName("field")
			if field == nil || kindOf(field) != "field_identifier" {
				return rustTypeExpr{}, false
			}
			if recv, ok := w.exprType(fn.ChildByFieldName("value")); ok {
				return recv.then(rustStep{call: true, name: nodeText(field, w.src)}), true
			}
		case "scoped_identifier":
			// `Type::from_raw(ptr)`: what the associated function returns.
			if t := w.scopedCallType(fn); t != "" {
				return rustTypeExpr{base: t}.then(rustStep{call: true, name: nodeText(fn.ChildByFieldName("name"), w.src)}), true
			}
		}
	}
	return rustTypeExpr{}, false
}

// scopedCallType is the type a `Type::f` callee names, "" for a module path.
func (w *astWalker) scopedCallType(fn *sitter.Node) string {
	path := fn.ChildByFieldName("path")
	if path == nil || fn.ChildByFieldName("name") == nil {
		return ""
	}
	if kindOf(path) == "generic_type" {
		path = path.ChildByFieldName("type")
	}
	if path == nil {
		return ""
	}
	switch kindOf(path) {
	case "identifier", "type_identifier":
		return w.typeCandidate(nodeText(path, w.src))
	case "scoped_identifier", "scoped_type_identifier":
		return w.typeOf(path)
	}
	return ""
}

// calleeReceiverType is the type a callee is called on: the receiver of
// `recv.method`, or the type of `Type::f`.
func (w *astWalker) calleeReceiverType(fn *sitter.Node) (rustTypeExpr, bool) {
	if fn != nil && kindOf(fn) == "generic_function" {
		fn = fn.ChildByFieldName("function")
	}
	if fn == nil {
		return rustTypeExpr{}, false
	}
	switch kindOf(fn) {
	case "field_expression":
		return w.exprType(fn.ChildByFieldName("value"))
	case "scoped_identifier":
		if t := w.scopedCallType(fn); t != "" {
			return rustTypeExpr{base: t}, true
		}
	}
	return rustTypeExpr{}, false
}

// bindParameters records the declared types of a function's parameters.
func (w *astWalker) bindParameters(params *sitter.Node) {
	if params == nil {
		return
	}
	for i := uint(0); i < params.NamedChildCount(); i++ {
		p := params.NamedChild(i)
		if kindOf(p) != "parameter" {
			continue
		}
		pat := p.ChildByFieldName("pattern")
		if pat == nil || kindOf(pat) != "identifier" {
			continue
		}
		if t := w.typeOf(p.ChildByFieldName("type")); t != "" {
			w.localTypes[nodeText(pat, w.src)] = rustTypeExpr{base: t}
		}
	}
}

// letType is the type a let declaration gives its name: the written type, else
// its value's, else none (which shadows an earlier binding of the name).
func (w *astWalker) letType(let *sitter.Node) (string, rustTypeExpr, bool) {
	pat := let.ChildByFieldName("pattern")
	if pat == nil || kindOf(pat) != "identifier" {
		return "", rustTypeExpr{}, false
	}
	name := nodeText(pat, w.src)
	if typ := let.ChildByFieldName("type"); typ != nil {
		t := w.typeOf(typ)
		return name, rustTypeExpr{base: t}, t != ""
	}
	t, ok := w.exprType(let.ChildByFieldName("value"))
	return name, t, ok
}

// noteTypedCall records a call on a receiver of known type against the current
// function.
func (w *astWalker) noteTypedCall(recv rustTypeExpr, method string) {
	if w.inTestMod || len(w.ownerStack) == 0 {
		return
	}
	owner := w.currentOwner()
	notes, _ := owner.PropAny(propTypedCalls).([]rustTypedCall)
	owner.SetProp(propTypedCalls, append(notes, rustTypedCall{recv: recv, method: method}))
}

// recordFieldTypes notes the declared types of a struct's named fields.
func (w *astWalker) recordFieldTypes(f *facts.Fact, body *sitter.Node) {
	if body == nil || kindOf(body) != "field_declaration_list" {
		return
	}
	fields := make(map[string]string)
	for i := uint(0); i < body.NamedChildCount(); i++ {
		decl := body.NamedChild(i)
		if kindOf(decl) != "field_declaration" {
			continue
		}
		name := decl.ChildByFieldName("name")
		if name == nil {
			continue
		}
		if t := w.typeOf(decl.ChildByFieldName("type")); t != "" {
			fields[nodeText(name, w.src)] = t
		}
	}
	if len(fields) > 0 {
		f.SetProp(propFieldTypes, fields)
	}
}

// rustEitherDir marks a type named through a module path, declared in the
// module's directory or, by a file module, in the one above it.
const rustEitherDir = "?"

// canonType resolves a marked type to the directory that declares it.
func canonType(t string, exists map[string]bool) string {
	c, marked := strings.CutPrefix(t, rustEitherDir)
	if !marked || exists[c] {
		return c
	}
	dot := strings.LastIndexByte(c, '.')
	slash := strings.LastIndexByte(c[:max(dot, 0)], '/')
	if dot < 0 || slash < 0 {
		return c
	}
	if up := c[:slash] + c[dot:]; exists[up] {
		return up
	}
	return c
}

// resolveTypedCalls evaluates the walker's notes against the struct fields and
// return types of every file, and turns each call it can place on a method the
// crate declares into a calls edge to it. The bare-name edge the walker wrote for
// that call is replaced, and the in-loop lists name the method the same way.
func resolveTypedCalls(all []facts.Fact) {
	exists := make(map[string]bool, len(all))
	fields := make(map[string]string)
	returns := make(map[string]string)
	traits := make(map[string][]string)
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		exists[f.Name] = true
		if m, ok := f.PropAny(propFieldTypes).(map[string]string); ok {
			for field, t := range m {
				fields[f.Name+"."+field] = t
			}
			f.DelProp(propFieldTypes)
		}
		if t, ok := f.PropAny(propReturns).(string); ok {
			returns[f.Name] = t
			f.DelProp(propReturns)
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImplements {
				traits[f.Name] = append(traits[f.Name], r.Target)
			}
		}
	}
	// method finds what a type's method of that name is: its own, or a default
	// of a trait it implements.
	method := func(typ, name string) string {
		if exists[typ+"."+name] {
			return typ + "." + name
		}
		for _, tr := range traits[typ] {
			if exists[tr+"."+name] {
				return tr + "." + name
			}
		}
		return ""
	}
	eval := func(e rustTypeExpr) string {
		t := canonType(e.base, exists)
		for _, s := range e.steps {
			if t == "" {
				return ""
			}
			if s.call {
				t = returns[method(t, s.name)]
			} else {
				t = fields[t+"."+s.name]
			}
			t = canonType(t, exists)
		}
		return t
	}

	for i := range all {
		f := &all[i]
		notes, ok := f.PropAny(propTypedCalls).([]rustTypedCall)
		if !ok {
			continue
		}
		f.DelProp(propTypedCalls)
		rename := make(map[string]string)
		for _, n := range notes {
			target := method(eval(n.recv), n.method)
			if target == "" {
				continue
			}
			// Replace one bare edge the walker wrote for this call.
			for j := range f.Relations {
				if r := &f.Relations[j]; r.Kind == facts.RelCalls && r.Target == n.method {
					f.Relations = append(f.Relations[:j], f.Relations[j+1:]...)
					break
				}
			}
			if !f.HasRelation(facts.RelCalls, target) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
			}
			if prev, seen := rename[n.method]; seen && prev != target {
				rename[n.method] = "" // two receivers, two methods: the lists keep the name
			} else if !seen {
				rename[n.method] = target
			}
		}
		for _, key := range []string{"calls_in_loop", "calls_in_scaling_loop"} {
			if list, ok := f.PropAny(key).([]string); ok {
				for j, c := range list {
					if to := rename[c]; to != "" {
						list[j] = to
					}
				}
			}
		}
	}
}
