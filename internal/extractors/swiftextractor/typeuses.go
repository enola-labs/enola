package swiftextractor

import (
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// A type written in a declaration or a body is a use of it, wherever it is written:
// a parameter, return or property type, an enum case's associated value, a generic
// constraint or an associatedtype's, a closure parameter, a local annotation, a cast
// target, a protocol composition, a typealias's right-hand side, and the type path
// of a static read or a nested type built through its owner
// (`Colors.Background.pressed`, `AFError.Reason(...)`). Each is recorded as a names
// edge from the declaration it is written in, to the type path as written.
//
// The edge binds only to a type the file can see by that name, as Swift looks it up
// (resolveNamedTypes): a type nested in the declaration's enclosing types, innermost
// first, else the top-level type of the name in the module the file sees
// (typevisibility.go). A name no visible declaration has, a generic parameter and
// `Self` record nothing.

// noteTypeUse records the user_type ut as a names edge from the current owner.
func (w *astWalker) noteTypeUse(ut *sitter.Node) {
	path := userTypePath(ut, w.src)
	if path == "" {
		return
	}
	head, _, _ := strings.Cut(path, ".")
	if genericParamInScope(ut, head, w.src) {
		return
	}
	w.addOwnerEdge(facts.RelNames, path)
}

// userTypePath returns the dotted name a user_type writes (`Outer.Inner` of
// `Outer.Inner<T>`), or "" when a component is not a type name or it starts at Self.
func userTypePath(ut *sitter.Node, src []byte) string {
	var parts []string
	for i := uint(0); i < ut.ChildCount(); i++ {
		c := ut.Child(i)
		if kindOf(c) != "type_identifier" {
			continue
		}
		name := strings.Trim(nodeText(c, src), "`")
		if !isTypeName(name) || (len(parts) == 0 && name == "Self") {
			return ""
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, ".")
}

// genericParamInScope reports whether name is a generic parameter or associated type
// declared by a declaration enclosing n: it names no project type there.
func genericParamInScope(n *sitter.Node, name string, src []byte) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch kindOf(p) {
		case "function_declaration", "init_declaration", "subscript_declaration", "class_declaration",
			"protocol_declaration", "typealias_declaration", "protocol_function_declaration":
			if tp := findChildByKind(p, "type_parameters"); tp != nil {
				for i := uint(0); i < tp.ChildCount(); i++ {
					if c := tp.Child(i); kindOf(c) == "type_parameter" && typeParameterName(c, src) == name {
						return true
					}
				}
			}
			if kindOf(p) == "protocol_declaration" {
				if body := findChildByKind(p, "protocol_body"); body != nil {
					for i := uint(0); i < body.ChildCount(); i++ {
						c := body.Child(i)
						if kindOf(c) == "associatedtype_declaration" {
							if id := findChildByKind(c, "type_identifier"); id != nil && nodeText(id, src) == name {
								return true
							}
						}
					}
				}
			}
		}
	}
	return false
}

func typeParameterName(tp *sitter.Node, src []byte) string {
	if id := findChildByKind(tp, "type_identifier"); id != nil {
		return nodeText(id, src)
	}
	return ""
}

// noteTypeUsesIn records every user_type under n, and the type path of a static read
// in a parameter's default value, outside function bodies, computed property bodies
// and lambdas: the walk of the body records those. An inheritance specifier's own
// type is an implements edge already; its type arguments are uses.
func (w *astWalker) noteTypeUsesIn(n *sitter.Node) {
	if n == nil {
		return
	}
	switch kindOf(n) {
	case "function_body", "computed_property", "lambda_literal", "class_body", "enum_class_body",
		"modifiers", "willset_didset_block":
		return
	case "user_type":
		if p := n.Parent(); p == nil || kindOf(p) != "inheritance_specifier" {
			w.noteTypeUse(n)
		}
	case "navigation_expression":
		w.noteStaticPath(n) // a parameter's default value: `= Config.current`
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		w.noteTypeUsesIn(n.Child(i))
	}
}

// noteStaticPath records the type path a navigation chain is rooted at: the leading
// capitalized components of `Colors.Background.Accent.pressed` or
// `AFError.UnexpectedInputStreamLength(...)`. A call through the root alone
// (`Router.shared.go()`) is recorded as a call of the root already.
func (w *astWalker) noteStaticPath(nav *sitter.Node) {
	if p := nav.Parent(); p != nil && kindOf(p) == "navigation_expression" {
		return // the outermost navigation records the chain
	}
	var parts []string
	var walk func(n *sitter.Node) bool
	walk = func(n *sitter.Node) bool {
		switch kindOf(n) {
		case "simple_identifier":
			name := strings.Trim(nodeText(n, w.src), "`")
			if !isTypeName(name) || name == "Self" {
				return false
			}
			parts = append(parts, name)
			return true
		case "navigation_expression":
			target := n.ChildByFieldName("target")
			if target == nil || !walk(target) {
				return false
			}
			suffix := n.ChildByFieldName("suffix")
			if suffix == nil {
				return false
			}
			id := findChildByKind(suffix, "simple_identifier")
			if id == nil {
				return false
			}
			if name := strings.Trim(nodeText(id, w.src), "`"); isTypeName(name) {
				parts = append(parts, name)
				return true
			}
			return false
		}
		return false
	}
	walk(nav)
	if len(parts) == 0 {
		return
	}
	if len(parts) == 1 {
		if p := nav.Parent(); p != nil && kindOf(p) == "call_expression" && firstNamedChild(p) == nav {
			return
		}
	}
	if genericParamInScope(nav, parts[0], w.src) {
		return
	}
	w.addOwnerEdge(facts.RelNames, strings.Join(parts, "."))
}

// swiftNamedTypes indexes the type and typealias declarations a names edge may bind
// to, separately for what production and test files see (scope.go).
type swiftNamedTypes struct {
	all, prod swiftNamedTypeSet
	decl      map[string]*facts.Fact // declared type name -> its fact
}

type swiftNamedTypeSet struct {
	sites map[string][]swiftTypeDeclSite // simple name -> declaring modules
	full  map[string]bool                // declared type names
	// tails maps ".Outer.Inner" (two or more trailing components) to the declared
	// names ending so: an extension of a qualified type (`extension A.B`) names its
	// members after B alone, so its scope is found by that tail.
	tails map[string][]string
}

func newSwiftNamedTypes(allFacts []facts.Fact, moduleOf func(string) string) *swiftNamedTypes {
	t := &swiftNamedTypes{
		all:  swiftNamedTypeSet{sites: map[string][]swiftTypeDeclSite{}, full: map[string]bool{}, tails: map[string][]string{}},
		prod: swiftNamedTypeSet{sites: map[string][]swiftTypeDeclSite{}, full: map[string]bool{}, tails: map[string][]string{}},
		decl: map[string]*facts.Fact{},
	}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		switch sk, _ := f.PropAny("symbol_kind").(string); sk {
		case facts.SymbolStruct, facts.SymbolClass, facts.SymbolInterface, facts.SymbolType:
		default:
			continue
		}
		dir := moduleOf(f.File)
		if !strings.HasPrefix(f.Name, dir+".") {
			continue
		}
		if t.decl[f.Name] == nil {
			t.decl[f.Name] = f
		}
		short := lastDotComponent(f.Name)
		site := swiftTypeDeclSite{dir: dir, topLevel: f.Name == dir+"."+short, outer: lastDotComponent(ownerOf(f.Name))}
		t.all.add(f.Name, short, site)
		if !isSwiftTestTarget(f.File) {
			t.prod.add(f.Name, short, site)
		}
	}
	return t
}

func (s *swiftNamedTypeSet) add(name, short string, site swiftTypeDeclSite) {
	if !s.full[name] {
		parts := strings.Split(strings.TrimPrefix(name, site.dir+"."), ".")
		for i := 0; i+2 <= len(parts); i++ {
			key := "." + strings.Join(parts[i:], ".")
			s.tails[key] = append(s.tails[key], name)
		}
	}
	s.full[name] = true
	for _, d := range s.sites[short] {
		if d == site {
			return
		}
	}
	s.sites[short] = append(s.sites[short], site)
}

func (t *swiftNamedTypes) forFile(file string) *swiftNamedTypeSet {
	if isSwiftTestTarget(file) {
		return &t.all
	}
	return &t.prod
}

// resolve returns the declared type a names edge from `from` to the dotted path
// means, or "" when the file sees none. The head binds as Swift looks it up: in each
// of from's enclosing types, innermost first, a type nested in it or inherited from
// one of its supertypes, in from's own module or the module the head resolves to;
// else the top-level type of the module it resolves to. Each further component
// extends the name while a nested type of it exists.
func (t *swiftNamedTypes) resolve(from *facts.Fact, path string, vis *swiftTypeVisibility) string {
	return t.resolveDepth(from, path, vis, 0)
}

// maxInheritDepth bounds the supertype walk of one lookup.
const maxInheritDepth = 4

func (t *swiftNamedTypes) resolveDepth(from *facts.Fact, path string, vis *swiftTypeVisibility, depth int) string {
	if strings.Contains("."+path+".", ".CodingKeys.") {
		return "" // markSynthesizedCodingKeys
	}
	set := t.forFile(from.File)
	head, rest, _ := strings.Cut(path, ".")
	sites := set.sites[head]
	if len(sites) == 0 {
		return ""
	}
	own := vis.moduleOf(from.File)
	seen := vis.resolve(from, sites)
	dirs := []string{own}
	if seen != "" && seen != own {
		dirs = append(dirs, seen)
	}
	var scope []string
	if strings.HasPrefix(from.Name, own+".") {
		scope = strings.Split(from.Name[len(own)+1:], ".")
	}
	found := ""
	for k := len(scope); k > 0 && found == ""; k-- {
		for _, d := range dirs {
			enclosing := d + "." + strings.Join(scope[:k], ".")
			if set.full[enclosing+"."+head] {
				found = enclosing + "." + head
				break
			}
		}
		for _, d := range dirs {
			if found != "" {
				break
			}
			found = t.inherited(set, d+"."+strings.Join(scope[:k], "."), head, vis, depth)
		}
	}
	// The enclosing type named after the last component of a qualified extension.
	for k := len(scope); k > 0 && found == ""; k-- {
		var hits []string
		for _, c := range set.tails["."+strings.Join(scope[:k], ".")+"."+head] {
			for _, d := range dirs {
				if strings.HasPrefix(c, d+".") {
					hits = append(hits, c)
				}
			}
		}
		if len(hits) == 1 {
			found = hits[0]
		}
	}
	if found == "" && seen != "" && set.full[seen+"."+head] {
		found = seen + "." + head
	}
	if found == "" {
		return ""
	}
	for rest != "" {
		var seg string
		seg, rest, _ = strings.Cut(rest, ".")
		if !set.full[found+"."+seg] {
			break
		}
		found += "." + seg
	}
	if found == from.Name {
		return ""
	}
	return found
}

// inherited returns the type named head nested in a supertype of the declared type
// typeName (a subclass sees its superclass's nested types by their bare names), or "".
func (t *swiftNamedTypes) inherited(set *swiftNamedTypeSet, typeName, head string, vis *swiftTypeVisibility, depth int) string {
	f := t.decl[typeName]
	if f == nil || depth >= maxInheritDepth {
		return ""
	}
	for _, r := range f.Relations {
		if r.Kind != facts.RelImplements {
			continue
		}
		sup := t.resolveDepth(f, r.Target, vis, depth+1)
		if sup == "" || sup == typeName {
			continue
		}
		if set.full[sup+"."+head] {
			return sup + "." + head
		}
		if got := t.inherited(set, sup, head, vis, depth+1); got != "" {
			return got
		}
	}
	return ""
}

// codableProtocols are the standard library protocols whose conformance the
// compiler synthesizes from a nested CodingKeys enum.
var codableProtocols = map[string]bool{"Codable": true, "Decodable": true, "Encodable": true}

// markSynthesizedCodingKeys marks a CodingKeys enum (conforming to CodingKey) nested
// in a type that conforms to Codable, Decodable or Encodable as framework_registered:
// the compiler's synthesized conformance reads it, so no source names it. The
// conformance may be stated on the type, on an extension of it, or through a
// protocol of the project that refines one of them.
//
// No names edge is recorded to a CodingKeys enum: every Codable type declares one of
// that name, so a use of one would read as a use of all of them.
func markSynthesizedCodingKeys(allFacts []facts.Fact) {
	// Keyed by declaring module and simple name: an extension fact is named after
	// the extended type's simple name.
	key := func(f *facts.Fact, name string) string {
		for _, r := range f.Relations {
			if r.Kind == facts.RelDeclares {
				return r.Target + "\x00" + lastDotComponent(name)
			}
		}
		return ""
	}
	codable := map[string]bool{}       // module + simple name -> conforms
	conforms := map[string][]string{}  // module + simple name -> protocols it conforms to
	protoKeys := map[string][]string{} // simple protocol name -> its keys
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		name := f.Name
		sk, _ := f.PropAny("symbol_kind").(string)
		if sk == facts.SymbolExtension {
			if j := strings.LastIndexByte(name, '+'); j > 0 {
				name = name[:j]
			}
		}
		k := key(f, name)
		if k == "" {
			continue
		}
		if sk == facts.SymbolInterface {
			protoKeys[lastDotComponent(name)] = append(protoKeys[lastDotComponent(name)], k)
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelImplements {
				continue
			}
			proto := strings.TrimPrefix(r.Target, "Swift.")
			if codableProtocols[proto] {
				codable[k] = true
			}
			conforms[k] = append(conforms[k], lastDotComponent(proto))
		}
	}
	for changed := true; changed; {
		changed = false
		for k, protos := range conforms {
			if codable[k] {
				continue
			}
			for _, p := range protos {
				if slices.ContainsFunc(protoKeys[p], func(pk string) bool { return codable[pk] }) {
					codable[k] = true
					changed = true
					break
				}
			}
		}
	}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol || lastDotComponent(f.Name) != "CodingKeys" || !f.HasRelation(facts.RelImplements, "CodingKey") {
			continue
		}
		if codable[key(f, ownerOf(f.Name))] {
			f.SetProp("framework_registered", true)
		}
	}
}
