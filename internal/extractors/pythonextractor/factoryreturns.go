package pythonextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// What an imported factory returns.
//
//	from cognee.infrastructure.databases.graph import get_graph_engine
//
//	async def add_data_points(...):
//	    graph_engine = await get_graph_engine()     # -> GraphDBInterface
//	    await graph_engine.add_nodes(nodes)
//
// returntypes.go types a local by the return annotation of a function of the same
// file. A factory is nearly always imported, and its annotation is in a file this
// one's walk does not see, so `graph_engine.add_nodes` named nothing and a flow
// through the function ended at the factory.
//
// The walk cannot answer it, so it says what it knows: the local holds what
// `cognee.….get_graph_engine` returns, written as that path with pyCallMark after
// it. A call on the local is recorded as `<path>().add_nodes`. Once every file is
// merged, each file has contributed the return annotations of its functions and
// of its classes' methods (returns, keyed by the symbol), and resolveCallTargetsWith
// replaces the mark by the declared type, resolved from the factory's own file:
// `GraphDBInterface.add_nodes`, and the implementor calls a typed receiver gets.
// A mark that names no function with a declared type is dropped with its edge,
// which is what the walk recorded before.
//
// Chains read the same way: `make_client().session().get()` is
// `<make_client>().session().get`, each step typed by the previous one's return.

// pyCallMark follows the path of a call whose result a receiver is: `pkg.mod.f()`.
// No identifier or module path contains it.
const pyCallMark = "()"

// pyReturn is a declared return type and the directory of the file that declares
// it, which a relative or same-package name in it is resolved from.
type pyReturn struct {
	typ string
	dir string
}

// isCallMarked reports whether a call target goes through a call's result.
func isCallMarked(target string) bool {
	return strings.Contains(target, pyCallMark+".")
}

// collectReturnTypes maps the top-level functions of a module and the methods of
// its top-level classes to the types their return annotations name, keyed as
// their symbols are (`pkg/mod.f`, `pkg/mod.Cls.m`). importMap must be complete.
func collectReturnTypes(root *sitter.Node, src []byte, module, dir string, importMap map[string]string) map[string]pyReturn {
	out := make(map[string]pyReturn)
	for name, ann := range collectFuncReturnAnnotations(root, src) {
		if t := resolveTypeNamePy(ann, module, importMap); t != "" {
			out[module+"."+name] = pyReturn{typ: t, dir: dir}
		}
	}
	for i := uint(0); i < root.NamedChildCount(); i++ {
		def := pyDefinition(root.NamedChild(i))
		if def == nil || kindOf(def) != "class_definition" {
			continue
		}
		nameNode := def.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		qual := module + "." + pyText(nameNode, src)
		for m, t := range collectMethodReturnTypes(def.ChildByFieldName("body"), src, importMap, module, qual) {
			out[qual+"."+m] = pyReturn{typ: t, dir: dir}
		}
	}
	return out
}

// returnResolver answers marked targets against the merged return types.
type returnResolver struct {
	returns   map[string]pyReturn
	symbols   map[string]bool
	fileIdx   suffixIndex
	topPkgs   map[string]bool
	reexports reexportIndex
}

// symbol resolves a name written from a file in importerDir to a symbol of the
// repository, or "".
func (r *returnResolver) symbol(name, importerDir string) string {
	if !isDottedCallTarget(name) {
		if r.symbols[name] {
			return name
		}
		return ""
	}
	resolved, keep := resolveDottedTarget(name, r.fileIdx, r.topPkgs, importerDir, r.reexports, r.symbols)
	if !keep || !r.symbols[resolved] {
		return ""
	}
	return resolved
}

// returnOf is the declared type of what the named function or method returns, as
// a symbol, or "".
func (r *returnResolver) returnOf(fn, importerDir string) string {
	sym := r.symbol(fn, importerDir)
	if sym == "" {
		return ""
	}
	ret, ok := r.returns[sym]
	if !ok {
		return ""
	}
	return r.symbol(ret.typ, ret.dir)
}

// resolve rewrites a marked target to the method of the type the chain ends in,
// returning that type and the method, or ok=false.
func (r *returnResolver) resolve(target, importerDir string) (typ, method string, ok bool) {
	if r == nil {
		return "", "", false
	}
	parts := strings.Split(target, pyCallMark+".")
	last := parts[len(parts)-1]
	if last == "" || strings.Contains(last, pyCallMark) {
		return "", "", false
	}
	typ = r.returnOf(parts[0], importerDir)
	for _, step := range parts[1 : len(parts)-1] {
		if typ == "" {
			return "", "", false
		}
		typ = r.returnOf(typ+"."+step, importerDir)
	}
	if typ == "" {
		return "", "", false
	}
	return typ, last, true
}

// resolveMarkedRelations replaces the marked call targets of a fact by the
// method of the declared type, with the implementor calls a typed receiver gets,
// and drops the ones that name no declared type.
func resolveMarkedRelations(f *facts.Fact, r *returnResolver, idx *pySymbolIndex) {
	var marked bool
	for _, rel := range f.Relations {
		if isCallMarked(rel.Target) {
			marked = true
			break
		}
	}
	if !marked {
		return
	}
	importerDir := fileDir(f.File)
	out := f.Relations[:0]
	type implCall struct{ method, typ string }
	var impls []implCall
	for _, rel := range f.Relations {
		if !isCallMarked(rel.Target) {
			out = append(out, rel)
			continue
		}
		typ, method, ok := r.resolve(rel.Target, importerDir)
		if !ok {
			continue
		}
		rel.Target = typ + "." + method
		out = append(out, rel)
		if rel.Kind == facts.RelCalls {
			impls = append(impls, implCall{method, typ})
		}
	}
	f.Relations = out
	if idx == nil || idx.implMap == nil {
		return
	}
	for _, c := range impls {
		addImplementorCalls(f, c.method, c.typ, idx)
	}
}
