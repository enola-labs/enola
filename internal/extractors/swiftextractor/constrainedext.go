package swiftextractor

import (
	"slices"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// Members of constrained extensions.
//
// `extension Lens where Whole == Project { var id }` and `extension Lens where
// Whole == User { var id }` declare two members Swift tells apart by the
// constraint, but both would be named Lens.id, one name-keyed fact for two
// declarations. A where-clause is no canonical name (its order, `:` against `==`,
// typealiases, the `<A>` shorthand all spell one constraint several ways), so a
// member of a constrained extension is named by its file instead, the rule the Dart
// extractor gives an unnamed extension: `Lens.id@ProjectLenses_swift`, with `@2`,
// `@3` for a further constrained extension of the type in the file declaring the
// name again, in source order. Overloads in one extension share their name, as
// they do everywhere else.
//
// The qualifier is given to every member of a constrained extension, not only to a
// name that is declared in more than one file: an id that a second file declaring
// the name would rename is no stable id, and a file sees only itself.
//
// The walk names members as before and marks them (propFileQualifier,
// propExtensionSpan); fileQualifyConstrainedMembers renames them once every
// resolver has bound its edges to the shared name, and moves those edges.

const (
	propFileQualifier = "swift_file_qualifier"
	propExtensionSpan = "swift_extension_span"
)

// isConstrainedExtension reports an extension with a where-clause or written with
// type arguments (`extension Array<Draft>`, sugar for a where-clause).
func isConstrainedExtension(node, nameNode *sitter.Node) bool {
	return findChildByKind(node, "type_constraints") != nil ||
		(kindOf(nameNode) == "user_type" && findChildByKind(nameNode, "type_arguments") != nil)
}

// fileQualifier is `@<file>` for the file's base name with its dots made `_`
// (`Project.Lenses.swift` gives `@Project_Lenses_swift`), as `.` separates a
// qualified name.
func fileQualifier(relFile string) string {
	return "@" + strings.ReplaceAll(relFile[strings.LastIndexByte(relFile, '/')+1:], ".", "_")
}

// markConstrainedMembers marks the members a constrained extension declared,
// out[bodyStart:], named memberPrefix+<name>, with the qualifier each will take.
func (w *astWalker) markConstrainedMembers(node *sitter.Node, memberPrefix string, bodyStart int) {
	suffix := map[string]string{}
	span := [2]int{int(node.StartPosition().Row) + 1, int(node.EndPosition().Row) + 1}
	for i := bodyStart; i < len(w.out); i++ {
		f := &w.out[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		short, ok := strings.CutPrefix(f.Name, memberPrefix)
		if !ok || short == "" || strings.Contains(short, ".") {
			continue
		}
		switch sk, _ := f.PropAny("symbol_kind").(string); sk {
		case facts.SymbolMethod, facts.SymbolFunc, facts.SymbolVariable, facts.SymbolConstant:
		default:
			continue // a nested type keeps its name, and so do its members
		}
		s, seen := suffix[short]
		if !seen {
			if w.constrainedMembers == nil {
				w.constrainedMembers = map[string]int{}
			}
			w.constrainedMembers[f.Name]++
			s = fileQualifier(w.relFile)
			if k := w.constrainedMembers[f.Name]; k > 1 {
				s += "@" + strconv.Itoa(k)
			}
			suffix[short] = s
		}
		f.SetProp(propFileQualifier, s)
		f.SetProp(propExtensionSpan, span)
	}
}

// constrainedDecl is one declaration a reference to a shared name may mean.
type constrainedDecl struct {
	name string
	file string
	span [2]int // the declaring constrained extension's lines
}

// fileQualifyConstrainedMembers gives the marked members their file-qualified
// names and moves each edge to a shared name to one of the declarations behind it:
// the one in the referring declaration's own extension, else the one its file
// declares, else the only one there is. A reference the name leaves ambiguous draws
// no edge: an arbitrary one would credit a declaration nobody uses.
//
// When the type or a plain extension declares the name too, the constrained ones
// are overloads of it (`func requestFirstPage()` where the params are Void, beside
// `requestFirstPage(withParams:)`), and an edge stays on the plain declaration,
// which keeps the name and applies everywhere: the convenience overload calls the
// general one, and a reference from its own extension is no call to itself.
func fileQualifyConstrainedMembers(allFacts []facts.Fact) {
	decls := map[string][]constrainedDecl{}
	for i := range allFacts {
		f := &allFacts[i]
		q, _ := f.PropAny(propFileQualifier).(string)
		if q == "" {
			continue
		}
		span, _ := f.PropAny(propExtensionSpan).([2]int)
		delete(f.Props, propFileQualifier)
		delete(f.Props, propExtensionSpan)
		decls[f.Name] = append(decls[f.Name], constrainedDecl{name: f.Name + q, file: f.File, span: span})
		f.Name += q
	}
	if len(decls) == 0 {
		return
	}
	for i := range allFacts {
		f := &allFacts[i]
		if _, ok := decls[f.Name]; ok && f.Kind == facts.KindSymbol {
			delete(decls, f.Name)
		}
	}
	for i := range allFacts {
		f := &allFacts[i]
		if len(f.Relations) > 0 {
			rels := f.Relations[:0]
			for _, r := range f.Relations {
				if ds, ok := decls[r.Target]; ok {
					r.Target = pickConstrainedDecl(ds, f)
					if r.Target == "" {
						continue
					}
				}
				rels = append(rels, r)
			}
			f.Relations = rels
		}
		renameTargetProps(f, decls)
	}
}

// renameTargetProps moves the call targets the walk listed in props to the
// declaration the edge binds to. A loop call the name leaves ambiguous stays as
// written, a call like any other the graph cannot bind; a value_refs entry mirrors
// an edge, so it goes when the edge does.
func renameTargetProps(f *facts.Fact, decls map[string][]constrainedDecl) {
	for _, key := range []string{"calls_in_loop", propValueRefs} {
		list, ok := f.PropAny(key).([]string)
		if !ok || !slices.ContainsFunc(list, func(t string) bool { _, ok := decls[t]; return ok }) {
			continue
		}
		out := make([]string, 0, len(list))
		for _, t := range list {
			if ds, ok := decls[t]; ok {
				name := pickConstrainedDecl(ds, f)
				switch {
				case name != "":
					t = name
				case key == propValueRefs:
					continue
				}
			}
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
		if len(out) == 0 {
			f.DelProp(key)
			continue
		}
		f.SetProp(key, out)
	}
}

// pickConstrainedDecl is the declaration of ds a reference made by from means, or ""
// when nothing tells them apart.
func pickConstrainedDecl(ds []constrainedDecl, from *facts.Fact) string {
	inExtension := func(d constrainedDecl) bool {
		return d.file == from.File && d.span[0] <= from.Line && from.Line <= d.span[1]
	}
	sameFile := func(d constrainedDecl) bool { return d.file == from.File }
	for _, keep := range []func(constrainedDecl) bool{inExtension, sameFile, func(constrainedDecl) bool { return true }} {
		name := ""
		for _, d := range ds {
			if !keep(d) {
				continue
			}
			if name != "" && name != d.name {
				return ""
			}
			name = d.name
		}
		if name != "" {
			return name
		}
	}
	return ""
}
