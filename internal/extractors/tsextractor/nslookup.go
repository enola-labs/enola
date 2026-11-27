package tsextractor

import (
	"slices"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// A computed lookup on a namespace import, `ns[key]`, reads an export of the module
// by name. When the code pins the key down where it is written (a string literal, a
// const bound to one, a parameter or variable typed as a union of string literals,
// or `keyof typeof ns`), the lookup is a use of each export the key can name, and
// is recorded like `ns.name`. Otherwise it draws no edge: crediting every export
// would hide the dead ones. Each export of the module is marked reached_dynamically
// instead, naming the files of the lookups, so dead-code findings on it stay
// reported at low confidence with that reason, and the graph is unchanged.
const (
	propNamespaceDynamic = "ns_lookup_dynamic" // file_ref, transient: module files looked up by a dynamic key
	propNamespaceAll     = "ns_lookup_all"     // file_ref, transient: module files looked up by keyof typeof

	propReachedDynamically     = "reached_dynamically"
	propReachedDynamicallyFrom = "reached_dynamically_from"
)

// maxKeyTypeDepth bounds the type aliases a key's type is followed through.
const maxKeyTypeDepth = 8

// namespaceLookup records `ns[key]` on a namespace import.
func (r *fileRefs) namespaceLookup(n *sitter.Node) {
	obj := n.ChildByFieldName("object")
	if obj == nil || kindOf(r.kinds, obj) != "identifier" {
		return
	}
	ns := nodeText(obj, r.src)
	dir, ok := r.namespaces[ns]
	if !ok {
		return
	}
	keys, all, static := r.lookupKeys(n.ChildByFieldName("index"), ns)
	file := r.nsFiles[ns]
	switch {
	case static && all:
		if file != "" && !slices.Contains(r.nsAll, file) {
			r.nsAll = append(r.nsAll, file)
		}
	case static:
		for _, k := range keys {
			r.add(dir + "." + k)
		}
	case file != "" && !slices.Contains(r.nsDynamic, file):
		r.nsDynamic = append(r.nsDynamic, file)
	}
}

// lookupKeys reads the names a lookup key can take. static is false when the code
// does not pin them down; all is true when the key spans every export (`keyof
// typeof ns`, or a for-in over ns).
func (r *fileRefs) lookupKeys(key *sitter.Node, ns string) (keys []string, all, static bool) {
	if key == nil {
		return nil, false, false
	}
	switch kindOf(r.kinds, key) {
	case "string":
		return []string{stringLiteral(nodeText(key, r.src))}, false, true
	case "parenthesized_expression":
		if key.NamedChildCount() == 1 {
			return r.lookupKeys(key.NamedChild(0), ns)
		}
	case "as_expression", "satisfies_expression":
		if key.NamedChildCount() == 2 {
			return r.typeKeys(key.NamedChild(1), ns, 0)
		}
	case "identifier":
		return r.boundKeys(key, nodeText(key, r.src), ns)
	}
	return nil, false, false
}

// boundKeys reads the keys an identifier can hold from its nearest declaration in
// an enclosing scope: a parameter or a variable with a key type, or a const bound
// to a string literal. A declaration of the name that pins nothing down ends the
// search, so an outer declaration it shadows is never read.
func (r *fileRefs) boundKeys(use *sitter.Node, name, ns string) ([]string, bool, bool) {
	for p := use.Parent(); p != nil; p = p.Parent() {
		switch kindOf(r.kinds, p) {
		case "function_declaration", "function_expression", "arrow_function", "method_definition",
			"generator_function_declaration", "generator_function":
			if param := p.ChildByFieldName("parameter"); param != nil && nodeText(param, r.src) == name {
				return nil, false, false
			}
			params := p.ChildByFieldName("parameters")
			if params == nil {
				continue
			}
			for i := range params.NamedChildCount() {
				prm := params.NamedChild(i)
				k := kindOf(r.kinds, prm)
				if k != "required_parameter" && k != "optional_parameter" {
					continue
				}
				if pat := prm.ChildByFieldName("pattern"); pat == nil || kindOf(r.kinds, pat) != "identifier" || nodeText(pat, r.src) != name {
					continue
				}
				return r.typeKeys(prm.ChildByFieldName("type"), ns, 0)
			}
		case "for_in_statement":
			left, right := p.ChildByFieldName("left"), p.ChildByFieldName("right")
			if left == nil || nodeText(left, r.src) != name {
				continue
			}
			if op := p.ChildByFieldName("operator"); op != nil && nodeText(op, r.src) == "in" &&
				right != nil && nodeText(right, r.src) == ns {
				return nil, true, true
			}
			return nil, false, false
		case "statement_block", "program", "class_body":
			if keys, all, static, found := r.declaredKeys(p, name, ns); found {
				return keys, all, static
			}
		}
	}
	return nil, false, false
}

// declaredKeys looks for a declaration of name among the statements of a block.
func (r *fileRefs) declaredKeys(block *sitter.Node, name, ns string) (keys []string, all, static, found bool) {
	for i := range block.NamedChildCount() {
		stmt := block.NamedChild(i)
		if kindOf(r.kinds, stmt) == "export_statement" {
			if d := stmt.ChildByFieldName("declaration"); d != nil {
				stmt = d
			}
		}
		k := kindOf(r.kinds, stmt)
		if k != "lexical_declaration" && k != "variable_declaration" {
			continue
		}
		isConst := strings.HasPrefix(nodeText(stmt, r.src), "const")
		for j := range stmt.NamedChildCount() {
			d := stmt.NamedChild(j)
			if kindOf(r.kinds, d) != "variable_declarator" {
				continue
			}
			if nm := d.ChildByFieldName("name"); nm == nil || nodeText(nm, r.src) != name {
				continue
			}
			if t := d.ChildByFieldName("type"); t != nil {
				keys, all, static = r.typeKeys(t, ns, 0)
				return keys, all, static, true
			}
			if v := d.ChildByFieldName("value"); isConst && v != nil && kindOf(r.kinds, v) == "string" {
				return []string{stringLiteral(nodeText(v, r.src))}, false, true, true
			}
			return nil, false, false, true
		}
	}
	return nil, false, false, false
}

// typeKeys reads the string keys a type admits: a string literal type, a union of
// them, a type alias of this file naming one, or `keyof typeof ns`, which admits
// every export.
func (r *fileRefs) typeKeys(t *sitter.Node, ns string, depth int) (keys []string, all, static bool) {
	if t == nil || depth > maxKeyTypeDepth {
		return nil, false, false
	}
	switch kindOf(r.kinds, t) {
	case "type_annotation", "parenthesized_type":
		if t.NamedChildCount() == 1 {
			return r.typeKeys(t.NamedChild(0), ns, depth+1)
		}
	case "literal_type":
		if t.NamedChildCount() == 1 && kindOf(r.kinds, t.NamedChild(0)) == "string" {
			return []string{stringLiteral(nodeText(t.NamedChild(0), r.src))}, false, true
		}
	case "union_type":
		for i := range t.NamedChildCount() {
			k, a, ok := r.typeKeys(t.NamedChild(i), ns, depth+1)
			if !ok {
				return nil, false, false
			}
			all = all || a
			keys = append(keys, k...)
		}
		return keys, all, true
	case "index_type_query":
		if strings.Join(strings.Fields(nodeText(t, r.src)), " ") == "keyof typeof "+ns {
			return nil, true, true
		}
	case "type_identifier":
		if alias := r.typeAlias(nodeText(t, r.src)); alias != nil {
			return r.typeKeys(alias, ns, depth+1)
		}
	}
	return nil, false, false
}

// typeAlias returns the value of a top-level `type name = …` of this file.
func (r *fileRefs) typeAlias(name string) *sitter.Node {
	root := r.root
	if root == nil {
		return nil
	}
	for i := range root.NamedChildCount() {
		stmt := root.NamedChild(i)
		if kindOf(r.kinds, stmt) == "export_statement" {
			if d := stmt.ChildByFieldName("declaration"); d != nil {
				stmt = d
			}
		}
		if kindOf(r.kinds, stmt) != "type_alias_declaration" {
			continue
		}
		if nm := stmt.ChildByFieldName("name"); nm != nil && nodeText(nm, r.src) == name {
			return stmt.ChildByFieldName("value")
		}
	}
	return nil
}

// stringLiteral strips a JS string literal's quotes.
func stringLiteral(s string) string {
	if len(s) >= 2 {
		return s[1 : len(s)-1]
	}
	return s
}

// bindNamespaceLookups settles the lookups the file-ref pass recorded: a lookup by
// `keyof typeof ns` uses every export of the module, and a lookup by a dynamic key
// marks every export reached_dynamically, with the files of the lookups, and draws
// no edge. The transient props are removed.
//
// An export the module re-exports (`export { A } from './a'`, `export * from './b'`)
// is the declaration it names, so the lookup reaches that declaration in its own
// file; stars reads which.
func bindNamespaceLookups(all []facts.Fact, stars *starExports) {
	allBy := map[string][]int{} // module file -> file_refs looking up every export
	dynBy := map[string][]string{}
	seen := false
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindFileRef {
			continue
		}
		if files, ok := f.PropAny(propNamespaceAll).([]string); ok {
			for _, m := range files {
				allBy[m] = append(allBy[m], i)
			}
			f.DelProp(propNamespaceAll)
			seen = true
		}
		if files, ok := f.PropAny(propNamespaceDynamic).([]string); ok {
			for _, m := range files {
				if !slices.Contains(dynBy[m], f.File) {
					dynBy[m] = append(dynBy[m], f.File)
				}
			}
			f.DelProp(propNamespaceDynamic)
			seen = true
		}
	}
	if !seen {
		return
	}
	// A re-exported declaration, keyed by its file and name, takes the lookups of
	// every module that passes it on.
	type decl struct{ file, name string }
	reAll := map[decl][]int{}
	reDyn := map[decl][]string{}
	follow := func(module string, add func(decl)) {
		if stars == nil {
			return
		}
		for _, e := range stars.exportsOf(module) {
			if e.file != module {
				add(decl{e.file, e.target})
			}
		}
	}
	for m, refs := range allBy {
		follow(m, func(d decl) {
			for _, ref := range refs {
				if !slices.Contains(reAll[d], ref) {
					reAll[d] = append(reAll[d], ref)
				}
			}
		})
	}
	for m, from := range dynBy {
		follow(m, func(d decl) {
			for _, file := range from {
				if !slices.Contains(reDyn[d], file) {
					reDyn[d] = append(reDyn[d], file)
				}
			}
		})
	}
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol || !isModuleExport(*f) {
			continue
		}
		d := decl{f.File, f.Name}
		refs := append(slices.Clone(allBy[f.File]), reAll[d]...)
		slices.Sort(refs)
		for _, ref := range slices.Compact(refs) {
			if !hasRel(all[ref], facts.RelCalls, f.Name) {
				all[ref].Relations = append(all[ref].Relations, facts.Relation{Kind: facts.RelCalls, Target: f.Name})
			}
		}
		from := append(slices.Clone(dynBy[f.File]), reDyn[d]...)
		if len(from) > 0 {
			slices.Sort(from)
			f.SetProp(propReachedDynamically, true)
			f.SetProp(propReachedDynamicallyFrom, slices.Compact(from))
		}
	}
}

// isModuleExport reports whether a symbol is a top-level export of its file, one a
// namespace object of the file holds.
func isModuleExport(f facts.Fact) bool {
	if exp, _ := f.PropAny("exported").(bool); !exp {
		return false
	}
	name, ok := strings.CutPrefix(f.Name, factpath.Dir(f.File)+".")
	return ok && name != "" && !strings.Contains(name, ".")
}

func hasRel(f facts.Fact, kind, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == kind && r.Target == target {
			return true
		}
	}
	return false
}
