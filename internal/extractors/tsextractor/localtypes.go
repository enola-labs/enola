package tsextractor

import (
	"maps"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Locals with a known class.
//
// `const creator = new LocalVideoCreator({...}); await creator.create()` calls
// LocalVideoCreator.create, and so does `svc.create()` in `function f(svc:
// VideoService)`. The call resolver named a method on a receiver only through a
// field's stated type or an import, so a local holding an instance was a receiver of
// unknown type and its calls named nothing: the flow of Peertube's upload stopped at
// the object that does the upload.
//
// The type is read where the source states it: the class a `new` constructs, or a
// type annotation that names one class. A name declared twice with different
// types is left out, since a function's locals are one map without scopes.

// tsLocalTypes returns the parameters of fn and the locals of body whose class the
// source states, each mapped to the qualified name a method call on it resolves
// through.
func tsLocalTypes(kinds *tsutil.KindTable, fn, body *sitter.Node, src []byte, dir string, importMap map[string]string, names *typeNameCache) map[string]string {
	out := map[string]string{}
	conflict := map[string]bool{}
	declared := names.of(kinds, body, src)
	set := func(name, typ string) {
		if name == "" || typ == "" || conflict[name] {
			return
		}
		q := qualifyTSType(typ, dir, importMap, declared)
		if q == "" {
			return
		}
		if prior, ok := out[name]; ok && prior != q {
			delete(out, name)
			conflict[name] = true
			return
		}
		out[name] = q
	}
	if fn != nil {
		if params := fn.ChildByFieldName("parameters"); params != nil {
			for i := range params.NamedChildCount() {
				p := params.NamedChild(i)
				pat := p.ChildByFieldName("pattern")
				if pat == nil || kindOf(kinds, pat) != "identifier" {
					continue
				}
				set(nodeText(pat, src), declaredTypeName(kinds, p, src))
			}
		}
	}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if kindOf(kinds, n) == "variable_declarator" {
			name := n.ChildByFieldName("name")
			if name != nil && kindOf(kinds, name) == "identifier" {
				typ := declaredTypeName(kinds, n, src)
				if typ == "" {
					if v := n.ChildByFieldName("value"); v != nil {
						if kindOf(kinds, v) == "await_expression" {
							v = v.NamedChild(0)
						}
						if v != nil && kindOf(kinds, v) == "new_expression" {
							if ctor := v.ChildByFieldName("constructor"); ctor != nil && kindOf(kinds, ctor) == "identifier" {
								typ = nodeText(ctor, src)
							}
						}
					}
				}
				set(nodeText(name, src), typ)
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(body)
	return out
}

// declaredTypeName returns the class a declaration or parameter is annotated with
// (see annotatedTypeName), or "".
func declaredTypeName(kinds *tsutil.KindTable, n *sitter.Node, src []byte) string {
	ann := n.ChildByFieldName("type")
	if ann == nil {
		ann = findChildByKind(kinds, n, "type_annotation")
	}
	return annotatedTypeName(kinds, ann, src)
}

// qualifyTSType names a class written in a file: its import, or a class or interface
// the file declares. Any other name is a global or ambient type, `Response` or
// `HTMLElement`, which names no fact of the repository.
func qualifyTSType(name, dir string, importMap map[string]string, declared map[string]bool) string {
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return ""
	}
	if q, ok := importMap[name]; ok {
		return q
	}
	if declared[name] {
		return dir + "." + name
	}
	return ""
}

// typeNameCache holds a file's top-level type names, found on first use.
type typeNameCache struct {
	names map[string]bool
}

func (c *typeNameCache) of(kinds *tsutil.KindTable, n *sitter.Node, src []byte) map[string]bool {
	if c.names == nil {
		c.names = fileTypeNames(kinds, n, src)
	}
	return c.names
}

// fileTypeNames returns the classes and interfaces declared at the top level of the
// file a node belongs to.
func fileTypeNames(kinds *tsutil.KindTable, n *sitter.Node, src []byte) map[string]bool {
	root := n
	for root != nil && root.Parent() != nil {
		root = root.Parent()
	}
	out := map[string]bool{}
	if root == nil {
		return out
	}
	for i := range root.ChildCount() {
		stmt := root.Child(i)
		if kindOf(kinds, stmt) == "export_statement" {
			if d := stmt.ChildByFieldName("declaration"); d != nil {
				stmt = d
			}
		}
		switch kindOf(kinds, stmt) {
		case "class_declaration", "abstract_class_declaration", "interface_declaration":
			if name := stmt.ChildByFieldName("name"); name != nil {
				out[nodeText(name, src)] = true
			}
		}
	}
	return out
}

// withLocalRoots returns the member roots a body's calls resolve through: the file's,
// with the body's typed locals over them.
func withLocalRoots(roots, locals map[string]string) map[string]string {
	if len(locals) == 0 {
		return roots
	}
	out := make(map[string]string, len(roots)+len(locals))
	maps.Copy(out, roots)
	maps.Copy(out, locals)
	return out
}
