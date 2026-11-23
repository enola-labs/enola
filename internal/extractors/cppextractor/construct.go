package cppextractor

import (
	"strconv"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Creating an object without the Type(args) expression.
//
//	Holder h(x);  Holder h{};  Holder h;           // a local of a class type
//	struct Owner { Holder member; };                // a value member
//	new Holder(x)                                   // a new-expression
//	std::make_unique<Holder>(x)                     // a standard factory
//	make_uniq<Holder>(x)                            // a project factory template
//
// Each constructs a Holder (and later destroys it), so each records an instantiates
// edge, as Holder(x) does. The walker records the type as the source writes it
// (relConstructCandidate); the project pass keeps it only when that names a class
// or struct the caller can see, the same include visibility a call obeys. A name
// that resolves to nothing, to an enum or to a template parameter is dropped:
// there is no bare-name fallback.
//
// A project factory is a function template whose return statement holds `new P(…)`
// for one of its own type parameters P, or calls another factory with P in that
// factory's constructed position (a wrapper of std::make_unique, or of another
// project factory). The definition records which position it constructs
// (propFactoryNew, propFactoryForwards); a call `f<A, B>(…)` records its callee and
// written type arguments (relFactoryCandidate). The project pass finds the factory
// through the call edge the caller already resolved, then the type argument in the
// constructed position.

const (
	// relConstructCandidate is a provisional edge naming a constructed type as
	// written ("ns::Holder"). It does not leave the extractor.
	relConstructCandidate = "construct_candidate"
	// relFactoryCandidate is a provisional edge for a call to a template with
	// explicit type arguments: "callee|Arg0;Arg1". It does not leave the extractor.
	relFactoryCandidate = "factory_call_candidate"
	// propFactoryNew holds, on a function template, the position of the type
	// parameter its return statement constructs. It does not leave the extractor.
	propFactoryNew = "factory_new_param"
	// propFactoryForwards holds, on a function template, "callee|argPos|ownParam"
	// for each factory call in its return statement whose type argument at argPos
	// is its own parameter ownParam. It does not leave the extractor.
	propFactoryForwards = "factory_forwards"
)

// stdFactories construct their first type argument. Unqualified, they are the
// standard ones brought in by a using-directive.
var stdFactories = map[string]bool{
	"make_unique": true, "make_shared": true, "make_unique_for_overwrite": true,
	"make_shared_for_overwrite": true, "allocate_shared": true,
}

// templateParamNames lists a template_parameter_list's parameter names in order,
// "" for one without a name, so a position indexes the template arguments.
func (w *astWalker) templateParamNames(params *sitter.Node) []string {
	var names []string
	for i := uint(0); i < params.ChildCount(); i++ {
		p := params.Child(i)
		if !p.IsNamed() || kindOf(w.kinds, p) == "comment" {
			continue
		}
		name := ""
		if n := p.ChildByFieldName("name"); n != nil {
			name = nodeText(n, w.src)
		} else if d := p.ChildByFieldName("declarator"); d != nil {
			name = declaratorLeafName(w.kinds, d, w.src)
		} else if id := findChildByKind(w.kinds, p, "type_identifier"); id != nil {
			name = nodeText(id, w.src)
		}
		names = append(names, name)
	}
	return names
}

// isTemplateParam reports whether name is a type parameter of an enclosing
// template.
func (w *astWalker) isTemplateParam(name string) bool {
	for _, params := range w.templateParams {
		for _, p := range params {
			if p == name {
				return true
			}
		}
	}
	return false
}

// writtenType returns a constructed type as the source writes it, without template
// arguments ("ns::Holder" for ns::Holder<int>), or "" for anything that is not a
// project class name: a builtin, auto, a standard-library type or a template
// parameter.
func (w *astWalker) writtenType(t *sitter.Node) string {
	if t == nil {
		return ""
	}
	switch kindOf(w.kinds, t) {
	case "type_identifier", "qualified_identifier", "template_type":
	default:
		return ""
	}
	return w.writtenTypeText(strings.Join(strings.Fields(nodeText(t, w.src)), ""))
}

func isIdentRune(r rune) bool {
	return r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r > 127
}

// emitDeclConstructions records the class objects a local declaration creates:
// `T x;`, `T x(a);`, `T x{…};`, `T x = …;` and `T a[3];`. A pointer or reference
// creates none, `extern` declares one made elsewhere, and `T x();` declares a
// function. `T x(a, b)` with bare names parses as a prototype with unnamed
// parameters; inside a body it is the object.
func (w *astWalker) emitDeclConstructions(node *sitter.Node) {
	if w.lang != langCpp || hasStorageClass(w, node, "extern") {
		return
	}
	typ := w.writtenType(node.ChildByFieldName("type"))
	if typ == "" {
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		if node.FieldNameForChild(uint32(i)) == "declarator" && w.declaresObject(node.Child(i)) {
			w.addOwnerEdge(relConstructCandidate, typ)
			return
		}
	}
}

// emitFieldConstruction records a class's non-static value member of a class
// type: constructing the class constructs the member.
func (w *astWalker) emitFieldConstruction(node *sitter.Node) {
	if w.lang != langCpp || hasStorageClass(w, node, "static") {
		return
	}
	typ := w.writtenType(node.ChildByFieldName("type"))
	if typ == "" {
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		if node.FieldNameForChild(uint32(i)) != "declarator" {
			continue
		}
		switch kindOf(w.kinds, node.Child(i)) {
		case "field_identifier", "array_declarator":
			w.addOwnerEdge(relConstructCandidate, typ)
			return
		}
	}
}

// declaresObject reports whether a declarator of a local declaration names an
// object of the declared type itself (not a pointer, a reference or a function).
func (w *astWalker) declaresObject(d *sitter.Node) bool {
	switch kindOf(w.kinds, d) {
	case "identifier", "array_declarator":
		return true
	case "init_declarator":
		inner := d.ChildByFieldName("declarator")
		k := kindOf(w.kinds, inner)
		return k == "identifier" || k == "array_declarator"
	case "function_declarator":
		params := d.ChildByFieldName("parameters")
		if params == nil || params.NamedChildCount() == 0 {
			return false // T x(); declares a function
		}
		for j := uint(0); j < params.ChildCount(); j++ {
			p := params.Child(j)
			if !p.IsNamed() {
				continue
			}
			if kindOf(w.kinds, p) != "parameter_declaration" || p.ChildByFieldName("declarator") != nil {
				return false
			}
			switch kindOf(w.kinds, p.ChildByFieldName("type")) {
			case "type_identifier", "qualified_identifier":
			default:
				return false // a builtin type: a real prototype
			}
		}
		return true
	}
	return false
}

func hasStorageClass(w *astWalker, node *sitter.Node, class string) bool {
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		if kindOf(w.kinds, c) == "storage_class_specifier" && nodeText(c, w.src) == class {
			return true
		}
	}
	return false
}

// emitNewConstruction records `new T(…)`. In a function template's return
// statement, `new P(…)` of its own type parameter makes the template a factory.
func (w *astWalker) emitNewConstruction(node *sitter.Node) {
	if w.lang != langCpp {
		return
	}
	t := node.ChildByFieldName("type")
	if typ := w.writtenType(t); typ != "" {
		w.addOwnerEdge(relConstructCandidate, typ)
		return
	}
	if w.returnDepth == 0 || kindOf(w.kinds, t) != "type_identifier" {
		return
	}
	if pos := paramPosition(w.factoryParams, nodeText(t, w.src)); pos >= 0 {
		w.markFactory(pos)
	}
}

// markFactory records that the function being walked constructs its type
// parameter at pos.
func (w *astWalker) markFactory(pos int) {
	if idx := w.currentOwner(); idx >= 0 && w.out[idx].PropString(propFactoryNew) == "" {
		w.out[idx].SetProp(propFactoryNew, strconv.Itoa(pos))
	}
}

func paramPosition(params []string, name string) int {
	for i, p := range params {
		if p != "" && p == name {
			return i
		}
	}
	return -1
}

// handleFactoryCall reads a call with explicit type arguments, f<A, B>(…). A
// standard factory constructs its first argument. Any other records a candidate
// the project pass checks against the factories; in a function template's return
// statement, passing its own parameter on records a forward.
func (w *astWalker) handleFactoryCall(callee *sitter.Node, name string, kind calleeKind, root string) {
	if w.lang != langCpp || name == "" || kind == calleeField {
		return
	}
	fn := callee
	if kindOf(w.kinds, fn) == "qualified_identifier" {
		for fn != nil && kindOf(w.kinds, fn) == "qualified_identifier" {
			fn = fn.ChildByFieldName("name")
		}
	}
	if kindOf(w.kinds, fn) != "template_function" {
		return
	}
	args := fn.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	var written []string
	for i := uint(0); i < args.ChildCount(); i++ {
		a := args.Child(i)
		if !a.IsNamed() {
			continue
		}
		typ := ""
		if kindOf(w.kinds, a) == "type_descriptor" && a.ChildByFieldName("declarator") == nil {
			typ = nodeText(a.ChildByFieldName("type"), w.src)
		}
		written = append(written, strings.Join(strings.Fields(typ), ""))
	}
	std := stdFactories[name] && (kind == calleePlain || root == "std")
	if w.returnDepth > 0 && len(w.factoryParams) > 0 {
		for pos, a := range written {
			own := paramPosition(w.factoryParams, a)
			if own < 0 {
				continue
			}
			if std && pos == 0 {
				w.markFactory(own)
			} else if !std {
				if idx := w.currentOwner(); idx >= 0 {
					w.out[idx].AddPropStrings(propFactoryForwards, name+"|"+strconv.Itoa(pos)+"|"+strconv.Itoa(own))
				}
			}
		}
	}
	if std {
		if len(written) > 0 {
			if typ := w.writtenTypeText(written[0]); typ != "" {
				w.addOwnerEdge(relConstructCandidate, typ)
			}
		}
		return
	}
	if kind == calleeQualified && systemNamespaces[root] {
		return
	}
	for i, a := range written {
		written[i] = w.writtenTypeText(a)
	}
	w.addOwnerEdge(relFactoryCandidate, name+"|"+strings.Join(written, ";"))
}

// writtenTypeText applies writtenType's rules to a type argument's text.
func (w *astWalker) writtenTypeText(text string) string {
	if i := strings.IndexByte(text, '<'); i >= 0 {
		if strings.Contains(text[strings.LastIndexByte(text, '>')+1:], "::") {
			return "" // Outer<T>::Inner: a member of an instantiation
		}
		text = text[:i]
	}
	text = strings.TrimPrefix(text, "::")
	if text == "" {
		return ""
	}
	scopes := strings.Split(text, "::")
	leaf := scopes[len(scopes)-1]
	if leaf == "" || systemNamespaces[scopes[0]] || cppBuiltinTypes[leaf] || w.isTemplateParam(scopes[0]) {
		return ""
	}
	for _, r := range text {
		if r != ':' && !isIdentRune(r) {
			return ""
		}
	}
	return text
}

// resolveConstructions turns the provisional construction edges into instantiates
// edges to the class or struct they name, after canonicalizeTargets has resolved
// the calls (a factory is found through its call edge), and drops the rest along
// with the factory props.
func resolveConstructions(allFacts []facts.Fact, types typeIndex, visibility *includeVisibility) {
	kinds := map[string]string{}
	for i := range allFacts {
		if allFacts[i].Kind == facts.KindSymbol {
			kinds[allFacts[i].Name] = allFacts[i].PropString("symbol_kind")
		}
	}
	factories := factoryPositions(allFacts)
	for i := range allFacts {
		f := &allFacts[i]
		f.DelProp(propFactoryNew)
		f.DelProp(propFactoryForwards)
		rels := f.Relations
		var built []string
		kept := rels[:0]
		for _, r := range rels {
			switch r.Kind {
			case relConstructCandidate:
				built = append(built, r.Target)
			case relFactoryCandidate:
				callee, args, _ := strings.Cut(r.Target, "|")
				target := calledTarget(rels, callee)
				pos, ok := factories[target]
				written := strings.Split(args, ";")
				if ok && pos < len(written) && written[pos] != "" {
					built = append(built, written[pos])
				}
			default:
				kept = append(kept, r)
			}
		}
		source := sourceDir(f)
		for _, typ := range built {
			class := resolveWrittenType(typ, source, types, visibility)
			if k := kinds[class]; k != facts.SymbolClass && k != facts.SymbolStruct {
				continue
			}
			r := facts.Relation{Kind: facts.RelInstantiates, Target: class}
			if !containsRelation(kept, r) {
				kept = append(kept, r)
			}
		}
		f.Relations = kept
	}
}

// resolveWrittenType finds the declaration a written type names: its simple name
// through the type index and the caller's include visibility, kept only when the
// declaration's qualified name ends with what the source wrote.
func resolveWrittenType(written, source string, types typeIndex, visibility *includeVisibility) string {
	dir := types.resolve(lastScopeComponent(written), source, visibility, nil)
	if dir == "" {
		return ""
	}
	class := types.canonical(dir, lastScopeComponent(written))
	if !strings.Contains(written, "::") {
		return class
	}
	if class == dir+"."+written || strings.HasSuffix(class, "::"+written) {
		return class
	}
	return ""
}

// calledTarget returns the one resolved call target whose simple name is callee.
func calledTarget(rels []facts.Relation, callee string) string {
	found := ""
	for _, r := range rels {
		if r.Kind != facts.RelCalls || lastScopeComponent(r.Target) != callee {
			continue
		}
		if found != "" && found != r.Target {
			return ""
		}
		found = r.Target
	}
	return found
}

// factoryPositions maps each factory template to the type parameter position it
// constructs: its own `new P`, or a forward to a factory, followed to a fixpoint.
func factoryPositions(allFacts []facts.Fact) map[string]int {
	out := map[string]int{}
	for i := range allFacts {
		if pos, err := strconv.Atoi(allFacts[i].PropString(propFactoryNew)); err == nil {
			out[allFacts[i].Name] = pos
		}
	}
	for changed := true; changed; {
		changed = false
		for i := range allFacts {
			f := &allFacts[i]
			if _, done := out[f.Name]; done {
				continue
			}
			for _, fwd := range f.PropStrings(propFactoryForwards) {
				parts := strings.Split(fwd, "|")
				if len(parts) != 3 {
					continue
				}
				argPos, err1 := strconv.Atoi(parts[1])
				own, err2 := strconv.Atoi(parts[2])
				callee := calledTarget(f.Relations, parts[0])
				if pos, ok := out[callee]; err1 == nil && err2 == nil && ok && pos == argPos {
					out[f.Name] = own
					changed = true
					break
				}
			}
		}
	}
	return out
}
