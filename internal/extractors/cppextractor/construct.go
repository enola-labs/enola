package cppextractor

import (
	"strconv"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// Creating an object without the Type(args) expression.
//
//	Holder h(x);  Holder h{};  Holder h;           // a local of a class type
//	struct Owner { Holder member; };                // a value member
//	new Holder(x)                                   // a new-expression
//	std::make_unique<Holder>(x)                     // a standard factory
//	make_uniq<Holder>(x)                            // a project factory template
//	arena.Make<Holder>(x)  Make<Holder>(x)          // a member factory template
//	rows.emplace_back(x)                            // a container of Holder
//
// Each constructs a Holder (and later destroys it), so each records an instantiates
// edge, as Holder(x) does. The walker records the type as the source writes it
// (relConstructCandidate); the project pass keeps it only when that names a class
// or struct the caller can see, the same include visibility a call obeys. A name
// that resolves to nothing, to an enum or to a template parameter is dropped:
// there is no bare-name fallback.
//
// A project factory is a function template whose body holds `new P(…)` for one of
// its own type parameters P, or calls another factory with P in that factory's
// constructed position (a wrapper of std::make_unique, or of another project
// factory). The definition records which position it constructs (propFactoryNew,
// propFactoryForwards); a call `f<A, B>(…)` records its callee, written type
// arguments and receiver (relFactoryCandidate). The project pass finds the factory
// through the call edge the caller already resolved, or for a member template
// through the receiver's written type: the caller's own class for `Make<A>()` and
// `this->Make<A>()`, a parameter or local's type, or the type a member of the
// caller's class is declared with (propFieldTemplates), a smart pointer's pointee
// included. Then it takes the type argument in the constructed position.
//
// emplace_back, emplace_front and emplace construct the element type of a standard
// container (std::vector<Row> rows; rows.emplace_back(…) builds a Row), declared by
// a parameter, a local or a member of the caller's class.

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
	// propFieldTemplates holds, on a class, "kind|written type" by member name
	// for the members a factory call or an emplace may go through: "obj" for a
	// class-typed member (or a pointer or reference to one), "ptr" for a smart
	// pointer's pointee, "elem" for a container's element. It does not leave the
	// extractor.
	propFieldTemplates = "factory_field_types"
	// relFieldElementCandidate is a provisional edge naming a member of the
	// caller's class an emplace was called on. It does not leave the extractor.
	relFieldElementCandidate = "field_element_candidate"
)

// smartPointers reach their first type argument through ->, whatever qualifies
// them (a project may wrap the standard ones under the same name).
var smartPointers = map[string]bool{"unique_ptr": true, "shared_ptr": true}

// elementContainers hold values of their first type argument, which emplaceMethods
// construct in place.
var elementContainers = map[string]bool{
	"vector": true, "deque": true, "list": true, "forward_list": true,
	"set": true, "multiset": true, "unordered_set": true, "unordered_multiset": true,
	"queue": true, "priority_queue": true, "stack": true,
}

var emplaceMethods = map[string]bool{"emplace_back": true, "emplace_front": true, "emplace": true}

// templateArgument returns a template type's leaf name and its first type
// argument as written ("vector", "Row" for std::vector<Row>), or "" for anything
// else. An argument with a declarator (Row *) is "".
func templateArgument(kinds *tsutil.KindTable, t *sitter.Node, src []byte) (leaf, arg string) {
	for t != nil && kindOf(kinds, t) == "qualified_identifier" {
		t = t.ChildByFieldName("name")
	}
	if t == nil || kindOf(kinds, t) != "template_type" {
		return "", ""
	}
	leaf = nodeText(t.ChildByFieldName("name"), src)
	args := t.ChildByFieldName("arguments")
	if args == nil {
		return leaf, ""
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		a := args.Child(i)
		if !a.IsNamed() {
			continue
		}
		if kindOf(kinds, a) == "type_descriptor" && a.ChildByFieldName("declarator") == nil {
			arg = strings.Join(strings.Fields(nodeText(a.ChildByFieldName("type"), src)), "")
		}
		break
	}
	return leaf, arg
}

// recordFieldTemplates records, on the class, the members a member factory call or
// an emplace may go through (propFieldTemplates).
func (w *astWalker) recordFieldTemplates(node *sitter.Node) {
	idx := w.currentOwner()
	if w.lang != langCpp || idx < 0 || hasStorageClass(w, node, "static") {
		return
	}
	t := node.ChildByFieldName("type")
	entry := ""
	if typ := w.writtenType(t); typ != "" && kindOf(w.kinds, t) != "template_type" {
		entry = "obj|" + typ
	} else if leaf, arg := templateArgument(w.kinds, t, w.src); arg != "" {
		if typ := w.writtenTypeText(arg); typ != "" && smartPointers[leaf] {
			entry = "ptr|" + typ
		} else if typ != "" && elementContainers[leaf] {
			entry = "elem|" + typ
		}
	}
	if entry == "" {
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		if node.FieldNameForChild(uint32(i)) != "declarator" {
			continue
		}
		if leaf := declaratorLeafName(w.kinds, node.Child(i), w.src); leaf != "" {
			m, _ := w.out[idx].PropAny(propFieldTemplates).(map[string]string)
			if m == nil {
				m = map[string]string{}
				w.out[idx].SetProp(propFieldTemplates, m)
			}
			m[leaf] = entry
		}
	}
}

// emitEmplaceConstruction records the element type an emplace on root constructs:
// a parameter or local container's directly, a member's for the project pass.
func (w *astWalker) emitEmplaceConstruction(root string) {
	if w.lang != langCpp {
		return
	}
	if v, ok := w.receiverTemplates[root]; ok {
		if leaf, arg, _ := strings.Cut(v, "|"); elementContainers[leaf] {
			if typ := w.writtenTypeText(arg); typ != "" {
				w.addOwnerEdge(relConstructCandidate, typ)
			}
		}
		return
	}
	if field := fieldRoot(root); field != "" && w.enclosingTypeReceiver() != "" && (field != root || !w.valueLocal(field)) {
		w.addOwnerEdge(relFieldElementCandidate, field)
	}
}

// factoryReceiver says how the project pass finds the member template a call
// root.f<A>(…) reaches: "this" through the caller's class, "t:Type" through a
// parameter or local's type or a smart pointer local's pointee, "f:member" through
// a member of the caller's class. ok is false for a receiver none of these reach.
func (w *astWalker) factoryReceiver(root string) (recv string, ok bool) {
	if root == "this" {
		return "this", true
	}
	if typ := w.receiverTypes[root]; typ != "" {
		return "t:" + typ, !strings.HasSuffix(typ, callMark)
	}
	if v, local := w.receiverTemplates[root]; local {
		if leaf, arg, _ := strings.Cut(v, "|"); smartPointers[leaf] {
			if typ := w.writtenTypeText(arg); typ != "" {
				return "t:" + typ, true
			}
		}
		return "", false
	}
	field := fieldRoot(root)
	if field == "" || w.enclosingTypeReceiver() == "" || field == root && w.valueLocal(field) {
		return "", false
	}
	return "f:" + field, true
}

// stdFactories construct their first type argument, whatever qualifies them: the
// standard ones, brought in by a using-directive or reached through a namespace
// alias of std (`base_std::make_shared<T>`).
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

// emitNewConstruction records `new T(…)`. In a function template's body, `new P(…)`
// of its own type parameter makes the template a factory.
func (w *astWalker) emitNewConstruction(node *sitter.Node) {
	if w.lang != langCpp {
		return
	}
	t := node.ChildByFieldName("type")
	if typ := w.writtenType(t); typ != "" {
		w.addOwnerEdge(relConstructCandidate, typ)
		return
	}
	if kindOf(w.kinds, t) != "type_identifier" {
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

// handleFactoryCall reads a call with explicit type arguments, f<A, B>(…) or
// root.f<A, B>(…). A standard factory constructs its first argument. Any other
// records a candidate the project pass checks against the factories; in a function
// template's body, passing its own parameter on records a forward.
func (w *astWalker) handleFactoryCall(callee *sitter.Node, name string, kind calleeKind, root string) {
	if w.lang != langCpp || name == "" {
		return
	}
	fn := callee
	recv := ""
	switch kindOf(w.kinds, fn) {
	case "qualified_identifier":
		for fn != nil && kindOf(w.kinds, fn) == "qualified_identifier" {
			fn = fn.ChildByFieldName("name")
		}
	case "field_expression":
		if fn = fn.ChildByFieldName("field"); kindOf(w.kinds, fn) != "template_method" {
			return
		}
		var ok bool
		if recv, ok = w.factoryReceiver(root); !ok {
			return
		}
	case "template_function":
		if w.enclosingTypeReceiver() != "" {
			recv = "this" // a member template of the caller's class, or a call edge
		}
	}
	switch kindOf(w.kinds, fn) {
	case "template_function", "template_method":
	default:
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
	std := stdFactories[name] && kind != calleeField
	if len(w.factoryParams) > 0 {
		for pos, a := range written {
			own := paramPosition(w.factoryParams, a)
			if own < 0 {
				continue
			}
			if std && pos == 0 {
				w.markFactory(own)
			} else if !std {
				if idx := w.currentOwner(); idx >= 0 {
					w.out[idx].AddPropStrings(propFactoryForwards, name+"|"+strconv.Itoa(pos)+"|"+strconv.Itoa(own)+"|"+recv)
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
	w.addOwnerEdge(relFactoryCandidate, name+"|"+strings.Join(written, ";")+"|"+recv)
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
	if w.declaresTypeInFile(leaf) {
		return fileLocalMark + text
	}
	return text
}

// fileLocalMark prefixes a written type whose simple name this file declares
// itself (a nested class, a typedef or an alias): the name may only bind to a
// class of the caller's own directory, since the declaration the source means
// may be one the type index does not hold.
const fileLocalMark = "@"

// declaresTypeInFile reports whether the file defines a class, struct, union or
// enum (with a body), a typedef or an alias of that simple name.
func (w *astWalker) declaresTypeInFile(name string) bool {
	if w.fileTypes == nil {
		w.fileTypes = map[string]bool{}
		if w.root != nil {
			collectFileTypeNames(w, *w.root)
		}
	}
	return w.fileTypes[name]
}

// collectFileTypeNames recurses by value, for the reason walkForCallsAt gives.
func collectFileTypeNames(w *astWalker, nv sitter.Node) {
	n := &nv
	switch kindOf(w.kinds, n) {
	case "class_specifier", "struct_specifier", "union_specifier", "enum_specifier":
		if name := n.ChildByFieldName("name"); name != nil && n.ChildByFieldName("body") != nil {
			w.fileTypes[simpleTypeName(name, w.src)] = true
		}
	case "type_definition":
		for i := uint(0); i < n.ChildCount(); i++ {
			if n.FieldNameForChild(uint32(i)) == "declarator" {
				w.fileTypes[declaratorLeafName(w.kinds, n.Child(i), w.src)] = true
			}
		}
	case "alias_declaration":
		if name := n.ChildByFieldName("name"); name != nil {
			w.fileTypes[nodeText(name, w.src)] = true
		}
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		if c, ok := n.ChildAt(i); ok {
			collectFileTypeNames(w, c)
		}
	}
}

// resolveConstructions turns the provisional construction edges into instantiates
// edges to the class or struct they name, after canonicalizeTargets has resolved
// the calls (a factory is found through its call edge), and drops the rest along
// with the factory props.
func resolveConstructions(allFacts []facts.Fact, types typeIndex, visibility *includeVisibility) {
	kinds := map[string]string{}
	aliases := newTypeIndex()
	members := map[string]memberTypes{}
	for i := range allFacts {
		if allFacts[i].Kind == facts.KindSymbol {
			sk := allFacts[i].PropString("symbol_kind")
			kinds[allFacts[i].Name] = sk
			if sk == facts.SymbolType {
				aliases.add(lastScopeComponent(allFacts[i].Name), factpath.Dir(allFacts[i].File), allFacts[i].Name)
			}
			if m := fieldTemplates(&allFacts[i]); m != nil {
				members[allFacts[i].Name] = memberTypes{fields: m, source: sourceDir(&allFacts[i])}
			}
		}
	}
	resolveClass := func(written, source string) string {
		class := resolveWrittenType(written, source, types, aliases, visibility)
		if k := kinds[class]; k != facts.SymbolClass && k != facts.SymbolStruct {
			return ""
		}
		return class
	}
	// ownClass is the class a method belongs to, found from the receiver its
	// definition writes, as any written type is.
	ownClass := func(f *facts.Fact) string {
		receiver, _, _ := strings.Cut(f.PropString("receiver"), "<")
		if receiver == "" || f.PropString("symbol_kind") != facts.SymbolMethod {
			return ""
		}
		return resolveClass(receiver, sourceDir(f))
	}
	// member returns what a member of the caller's class is declared with.
	member := func(f *facts.Fact, field string) (kind, written, source string) {
		m, ok := members[ownClass(f)]
		if !ok {
			return "", "", ""
		}
		kind, written, _ = strings.Cut(m.fields[field], "|")
		return kind, written, m.source
	}
	// callees lists the factory a call `callee<…>` on recv may reach, most specific
	// first (factoryReceiver says what recv holds).
	callees := func(f *facts.Fact, callee, recv string) []string {
		var class string
		switch {
		case recv == "":
			return []string{calledTarget(f.Relations, callee)}
		case recv == "this":
			if class = ownClass(f); class == "" {
				return []string{calledTarget(f.Relations, callee)}
			}
			return []string{class + "::" + callee, calledTarget(f.Relations, callee)}
		case strings.HasPrefix(recv, "t:"):
			class = resolveClass(recv[2:], sourceDir(f))
		case strings.HasPrefix(recv, "f:"):
			if kind, written, source := member(f, recv[2:]); kind == "obj" || kind == "ptr" {
				class = resolveClass(written, source)
			}
		}
		if class == "" {
			return nil
		}
		return []string{class + "::" + callee}
	}
	factories := factoryPositions(allFacts, callees)
	type builtType struct{ written, source string }
	for i := range allFacts {
		f := &allFacts[i]
		rels := f.Relations
		var built []builtType
		kept := rels[:0]
		source := sourceDir(f)
		for _, r := range rels {
			switch r.Kind {
			case relConstructCandidate:
				built = append(built, builtType{r.Target, source})
			case relFactoryCandidate:
				parts := strings.SplitN(r.Target, "|", 3)
				if len(parts) != 3 {
					continue
				}
				written := strings.Split(parts[1], ";")
				for _, target := range callees(f, parts[0], parts[2]) {
					if pos, ok := factories[target]; ok {
						if pos < len(written) && written[pos] != "" {
							built = append(built, builtType{written[pos], source})
						}
						break
					}
				}
			case relFieldElementCandidate:
				if kind, written, from := member(f, r.Target); kind == "elem" {
					built = append(built, builtType{written, from})
				}
			default:
				kept = append(kept, r)
			}
		}
		for _, typ := range built {
			class := resolveClass(typ.written, typ.source)
			if class == "" {
				continue
			}
			r := facts.Relation{Kind: facts.RelInstantiates, Target: class}
			if !containsRelation(kept, r) {
				kept = append(kept, r)
			}
		}
		f.Relations = kept
	}
	for i := range allFacts {
		allFacts[i].DelProp(propFactoryNew)
		allFacts[i].DelProp(propFactoryForwards)
		allFacts[i].DelProp(propFieldTemplates)
	}
}

// memberTypes is a class's propFieldTemplates and the directory its written types
// resolve from.
type memberTypes struct {
	fields map[string]string
	source string
}

// fieldTemplates reads propFieldTemplates as the walker wrote it or as the
// extractor cache decoded it.
func fieldTemplates(f *facts.Fact) map[string]string {
	switch m := f.PropAny(propFieldTemplates).(type) {
	case map[string]string:
		return m
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, v := range m {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
		return out
	}
	return nil
}

// resolveWrittenType finds the declaration a written type names: its simple name
// through the type index and the caller's include visibility, kept only when the
// declaration's qualified name ends with what the source wrote. A typedef or alias
// of that name the caller sees in another directory may be what it means, and a
// name its own file declares binds only in its own directory: both are dropped.
func resolveWrittenType(written, source string, types, aliases typeIndex, visibility *includeVisibility) string {
	written, local := strings.CutPrefix(written, fileLocalMark)
	simple := lastScopeComponent(written)
	dir := types.resolve(simple, source, visibility, nil)
	if dir == "" || local && dir != source {
		return ""
	}
	if alias := aliases.resolve(simple, source, visibility, nil); alias != "" && alias != dir {
		return ""
	}
	class := types.canonical(dir, simple)
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
// callees lists the factories a forward's call may reach.
func factoryPositions(allFacts []facts.Fact, callees func(f *facts.Fact, callee, recv string) []string) map[string]int {
	out := map[string]int{}
	var forwarding []int
	for i := range allFacts {
		if pos, err := strconv.Atoi(allFacts[i].PropString(propFactoryNew)); err == nil {
			out[allFacts[i].Name] = pos
		} else if len(allFacts[i].PropStrings(propFactoryForwards)) > 0 {
			forwarding = append(forwarding, i)
		}
	}
	for changed := true; changed; {
		changed = false
		for _, i := range forwarding {
			f := &allFacts[i]
			if _, done := out[f.Name]; done {
				continue
			}
			for _, fwd := range f.PropStrings(propFactoryForwards) {
				parts := strings.Split(fwd, "|")
				if len(parts) != 4 {
					continue
				}
				argPos, err1 := strconv.Atoi(parts[1])
				own, err2 := strconv.Atoi(parts[2])
				if err1 != nil || err2 != nil {
					continue
				}
				for _, callee := range callees(f, parts[0], parts[3]) {
					if pos, ok := out[callee]; ok {
						if pos == argPos {
							out[f.Name] = own
							changed = true
						}
						break
					}
				}
				if _, done := out[f.Name]; done {
					break
				}
			}
		}
	}
	return out
}
