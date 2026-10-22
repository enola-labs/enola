package goextractor

import (
	"go/ast"
	"go/token"
	"path"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Go never says which types implement an interface, and an interface assertion
// (`var _ I = (*T)(nil)`, see ifaceassert.go) is the exception rather than the rule:
// a repository written the usual way hands its implementation out through a
// constructor and asserts nothing. Without an implementer a call on the interface is
// a dead end, so the I/O closure stopped one hop short of every repository behind
// an interface, and a service looping over a repository call read as doing no I/O.
//
// Two more statements in the source name an implementer:
//
//	func NewTripRepository(db *sql.DB) trip.Repository { return &TripRepository{db: db} }
//
// is STATED: the constructor's declared result is the interface and the value it
// returns is of the type. And a type whose methods cover the interface's is
// INFERRED, the way the compiler itself decides it, by method set. The extractor
// does not type-check, so the match is by method name and by the parameter and
// result types as written, each resolved through its file's imports to the same
// canonical name the graph uses. Counting them was not enough: repositories that all
// have Create, Get, List, Update and Delete with the same arity matched each other's
// service interfaces, and only the entity types tell them apart. Mocks are never
// implementers, stated or inferred.
//
// An inferred match is accepted only where it is specific: for an interface no
// non-mock type is stated to implement, with two or more methods, or that exactly one
// type matches. A one-method interface such as `Close() error` is satisfied by half a
// codebase, and carrying I/O through it would make every caller of Close do I/O.

// propImplementers and propImplementersInferred name, on an interface's symbol fact,
// the types stated and inferred to implement it, so an analysis that runs on the
// stored facts can follow a call through the interface as the extractor does.
const (
	propImplementers         = "implementers"
	propImplementersInferred = "implementers_inferred"
)

// methodSig is the shape a method is matched by: its parameter and result types,
// canonical and comma-joined.
type methodSig struct{ params, results string }

// typeDecls is what one pass over a module records about its named types.
type typeDecls struct {
	ifaceMethods  map[string]map[string]methodSig // "pkgDir.Iface" → its methods
	ifaceEmbeds   map[string][]string             // "pkgDir.Iface" → interfaces it embeds
	typeMethods   map[string]map[string]methodSig // "pkgDir.Type" → its declared methods
	mockTypes     map[string]bool                 // types declared in mock files
	constructions map[string][]string             // "pkgDir.Iface" → types a constructor returns as it
}

func newTypeDecls() typeDecls {
	return typeDecls{
		ifaceMethods:  make(map[string]map[string]methodSig),
		ifaceEmbeds:   make(map[string][]string),
		typeMethods:   make(map[string]map[string]methodSig),
		mockTypes:     make(map[string]bool),
		constructions: make(map[string][]string),
	}
}

// collectTypeDecls reads one package's interfaces, methods and constructors into d.
// Constructors are matched against interfaces only once every package is read, so
// they are recorded by their declared result here.
func collectTypeDecls(d typeDecls, pp *parsedPkg, pkgDir string, mods goModules, pkgNames, aliases map[string]string) {
	for _, relFile := range pp.relFiles {
		f, ok := pp.fileMap[relFile]
		if !ok {
			continue
		}
		ctx := resolveCtx{pkgDir: pkgDir, imports: buildFileImports(f, mods, pkgNames), aliases: aliases}
		mock := isMockFile(relFile, f)
		for _, decl := range f.Decls {
			switch dd := decl.(type) {
			case *ast.GenDecl:
				if dd.Tok != token.TYPE {
					continue
				}
				for _, spec := range dd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					name := pkgDir + "." + ts.Name.Name
					if mock {
						d.mockTypes[name] = true
					}
					it, ok := ts.Type.(*ast.InterfaceType)
					if !ok || it.Methods == nil {
						continue
					}
					methods := make(map[string]methodSig)
					for _, m := range it.Methods.List {
						ft, isFunc := m.Type.(*ast.FuncType)
						if !isFunc {
							if embedded := typeExprName(m.Type); embedded != "" {
								d.ifaceEmbeds[name] = append(d.ifaceEmbeds[name], resolveTypeName(embedded, ctx))
							}
							continue
						}
						for _, n := range m.Names {
							methods[n.Name] = sigOf(ft, ctx)
						}
					}
					d.ifaceMethods[name] = methods
				}
			case *ast.FuncDecl:
				if dd.Recv != nil && len(dd.Recv.List) == 1 {
					recv := typeExprName(dd.Recv.List[0].Type)
					if recv == "" || strings.Contains(recv, ".") {
						continue
					}
					owner := pkgDir + "." + recv
					if d.typeMethods[owner] == nil {
						d.typeMethods[owner] = make(map[string]methodSig)
					}
					d.typeMethods[owner][dd.Name.Name] = sigOf(dd.Type, ctx)
					continue
				}
				recordConstruction(d, dd, ctx)
			}
		}
	}
}

// recordConstruction notes the types a top-level function returns as its declared
// result: `func New(...) Iface` (or `(Iface, error)`) returning `&T{…}`, `T{…}` or
// `new(T)`. Whether Iface is an interface is settled later, once all are known.
func recordConstruction(d typeDecls, fn *ast.FuncDecl, ctx resolveCtx) {
	res := fn.Type.Results
	if fn.Body == nil || res == nil || len(res.List) == 0 || len(res.List) > 2 || len(res.List[0].Names) > 1 {
		return
	}
	declared := typeExprName(res.List[0].Type)
	if declared == "" {
		return
	}
	iface := resolveTypeName(declared, ctx)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if _, nested := n.(*ast.FuncLit); nested {
			return false // a closure's return is not the constructor's
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return true
		}
		if impl := assertedTypeName(ret.Results[0]); impl != "" {
			if t := resolveTypeName(impl, ctx); t != iface {
				d.constructions[iface] = append(d.constructions[iface], t)
			}
		}
		return true
	})
}

// sigOf writes a function type's parameter and result types, one per name in a
// named group.
func sigOf(ft *ast.FuncType, ctx resolveCtx) methodSig {
	list := func(fl *ast.FieldList) string {
		if fl == nil {
			return ""
		}
		var types []string
		for _, f := range fl.List {
			key := typeKey(f.Type, ctx)
			for n := max(1, len(f.Names)); n > 0; n-- {
				types = append(types, key)
			}
		}
		return strings.Join(types, ",")
	}
	return methodSig{params: list(ft.Params), results: list(ft.Results)}
}

// predeclaredTypes are Go's built-in type names, which no import qualifies.
var predeclaredTypes = map[string]bool{
	"bool": true, "byte": true, "complex64": true, "complex128": true, "error": true, "float32": true,
	"float64": true, "int": true, "int8": true, "int16": true, "int32": true, "int64": true, "rune": true,
	"string": true, "uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true, "any": true, "comparable": true,
}

// typeKey names a type expression canonically, so the same type written in two files
// under different import aliases compares equal.
func typeKey(e ast.Expr, ctx resolveCtx) string {
	switch t := e.(type) {
	case *ast.Ident:
		if predeclaredTypes[t.Name] {
			return t.Name
		}
		return resolveTypeName(t.Name, ctx)
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok {
			return resolveTypeName(pkg.Name+"."+t.Sel.Name, ctx)
		}
	case *ast.StarExpr:
		return "*" + typeKey(t.X, ctx)
	case *ast.ArrayType:
		return "[]" + typeKey(t.Elt, ctx)
	case *ast.MapType:
		return "map[" + typeKey(t.Key, ctx) + "]" + typeKey(t.Value, ctx)
	case *ast.Ellipsis:
		return "..." + typeKey(t.Elt, ctx)
	case *ast.ChanType:
		return "chan " + typeKey(t.Value, ctx)
	case *ast.ParenExpr:
		return typeKey(t.X, ctx)
	case *ast.IndexExpr:
		return typeKey(t.X, ctx)
	case *ast.IndexListExpr:
		return typeKey(t.X, ctx)
	case *ast.FuncType:
		return "func"
	case *ast.InterfaceType:
		return "interface"
	case *ast.StructType:
		return "struct"
	}
	return "?"
}

// isMockFile reports a file of generated or hand-written test doubles: under a mocks
// directory, named for a mock, or generated by a mocking tool.
func isMockFile(relFile string, f *ast.File) bool {
	dir, base := path.Split(relFile)
	if strings.Contains("/"+dir, "/mocks/") || strings.Contains("/"+dir, "/mock/") ||
		strings.HasPrefix(base, "mock_") || strings.HasSuffix(base, "_mock.go") || strings.HasSuffix(base, "_mocks.go") {
		return true
	}
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		if t := cg.Text(); strings.Contains(t, "Code generated by MockGen") || strings.Contains(t, "Code generated by mockery") {
			return true
		}
	}
	return false
}

// implementersFrom settles, per interface of the module, its stated and its inferred
// implementers: stated by an interface assertion (asserted) or a constructor, and
// inferred from method sets where nothing but a mock is stated. An interface's
// embedded interfaces contribute their methods first. No mock is in either.
func implementersFrom(d typeDecls, embeds, asserted map[string][]string) (stated, inferred map[string][]string) {
	stated = make(map[string][]string)
	inferred = make(map[string][]string)
	add := func(iface, t string) {
		if !d.mockTypes[t] && !contains(stated[iface], t) {
			stated[iface] = append(stated[iface], t)
		}
	}
	for iface, types := range asserted {
		for _, t := range types {
			add(iface, t)
		}
	}
	for iface, types := range d.constructions {
		if _, ok := d.ifaceMethods[iface]; !ok {
			continue // a constructor of a concrete type
		}
		for _, t := range types {
			add(iface, t)
		}
	}

	// Methods of each type, its own and those it promotes from an embedded type of
	// the module.
	methodsOf := func(t string) map[string]methodSig {
		out := make(map[string]methodSig)
		for _, e := range embeds[t] {
			for m, s := range d.typeMethods[e] {
				out[m] = s
			}
		}
		for m, s := range d.typeMethods[t] {
			out[m] = s
		}
		return out
	}
	// Types by method name, to look an interface up by its rarest method.
	byMethod := make(map[string][]string)
	for t, ms := range d.typeMethods {
		if d.mockTypes[t] {
			continue
		}
		for m := range ms {
			byMethod[m] = append(byMethod[m], t)
		}
	}
	for iface := range d.ifaceMethods {
		if len(stated[iface]) > 0 {
			continue // the source says what implements it; a name match adds only noise
		}
		want := ifaceMethodSet(iface, d, 0)
		if len(want) == 0 {
			continue
		}
		rarest := ""
		for m := range want {
			if rarest == "" || len(byMethod[m]) < len(byMethod[rarest]) || (len(byMethod[m]) == len(byMethod[rarest]) && m < rarest) {
				rarest = m
			}
		}
		var matches []string
		for _, t := range byMethod[rarest] {
			have := methodsOf(t)
			ok := true
			for m, s := range want {
				if got, present := have[m]; !present || got != s {
					ok = false
					break
				}
			}
			if ok {
				matches = append(matches, t)
			}
		}
		if len(matches) == 0 || (len(want) < 2 && len(matches) != 1) {
			continue
		}
		sort.Strings(matches)
		for _, t := range matches {
			if !contains(stated[iface], t) {
				inferred[iface] = append(inferred[iface], t)
			}
		}
	}
	for _, m := range []map[string][]string{stated, inferred} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
	return stated, inferred
}

// ifaceMethodSet is an interface's methods with those of the module interfaces it
// embeds, to a small depth.
func ifaceMethodSet(iface string, d typeDecls, depth int) map[string]methodSig {
	out := make(map[string]methodSig)
	if depth > 4 {
		return out
	}
	for _, e := range d.ifaceEmbeds[iface] {
		for m, s := range ifaceMethodSet(e, d, depth+1) {
			out[m] = s
		}
	}
	for m, s := range d.ifaceMethods[iface] {
		out[m] = s
	}
	return out
}

// annotateImplementers records the implementers on each interface's symbol fact.
func annotateImplementers(all []facts.Fact, stated, inferred map[string][]string) {
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol || f.PropString("symbol_kind") != facts.SymbolInterface {
			continue
		}
		if ts := stated[f.Name]; len(ts) > 0 {
			f.SetProp(propImplementers, ts)
		}
		if ts := inferred[f.Name]; len(ts) > 0 {
			f.SetProp(propImplementersInferred, ts)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
