package goextractor

import (
	"go/ast"
	"sort"
	"strings"
)

// A router the module wrote itself.
//
// routerWrapperPackages finds a package that wraps chi or gorilla/mux. Some
// applications carry a whole router of their own: Grafana's pkg/web descends from
// Macaron and imports no router at all, and pkg/api/routing registers on it. Every
// route of the application goes through types these two packages declare, from files
// that import nothing a framework list could name.
//
// Such a package is recognized by its verbs: a type with at least three of Get, Post,
// Put, Delete and Patch, each taking a pattern and then handlers. Taking handlers is
// what separates a router from an HTTP client, whose Get takes a URL and returns a
// response.
//
// Files that import one are read as chi-style files, with one restriction a real
// framework import does not need. chi's package is imported by files that register
// routes; an application's own web package is imported everywhere, for its context
// and its helpers. So a verb counts only on a receiver known to be a router: a
// parameter or a struct field of one of the package's router types, the parameter of
// a group's function literal, or a variable assigned from one of those.

// ownRouterMark prefixes the value routerWrapperPackages gives a self-contained
// router package: the mark, then its router type names, comma-separated.
const ownRouterMark = "own:"

// strictKey is set in a prefix map while walking a file that reaches its router only
// through a self-contained router package.
const strictKey = "\x00strict"

var ownRouterVerbs = map[string]bool{"Get": true, "Post": true, "Put": true, "Delete": true, "Patch": true}

// takesHandlers reports whether a verb's parameters are a pattern followed by
// handlers: a first parameter of type string and a last one whose type is a function
// or is named for a handler.
func takesHandlers(params *ast.FieldList) bool {
	if params == nil || len(params.List) < 2 {
		return false
	}
	if first, ok := params.List[0].Type.(*ast.Ident); !ok || first.Name != "string" {
		return false
	}
	last := params.List[len(params.List)-1].Type
	if ell, ok := last.(*ast.Ellipsis); ok {
		last = ell.Elt
	}
	if _, isFunc := last.(*ast.FuncType); isFunc {
		return true
	}
	return strings.Contains(typeExprToString(last), "Handler")
}

// ownRouterTypes returns the types of a package that declare a router's verbs, as
// methods or as an interface, sorted.
func ownRouterTypes(pp *parsedPkg) []string {
	verbs := map[string]map[string]bool{}
	add := func(typ, verb string) {
		if verbs[typ] == nil {
			verbs[typ] = map[string]bool{}
		}
		verbs[typ][verb] = true
	}
	for _, f := range pp.parsedFiles {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil || len(d.Recv.List) == 0 || !ownRouterVerbs[d.Name.Name] || !takesHandlers(d.Type.Params) {
					continue
				}
				if d.Type.Results != nil && len(d.Type.Results.List) > 0 {
					continue // a router's verb registers; it returns no response
				}
				if recv := typeExprToString(d.Recv.List[0].Type); recv != "" {
					add(recv, d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					iface, ok := ts.Type.(*ast.InterfaceType)
					if !ok || iface.Methods == nil {
						continue
					}
					for _, m := range iface.Methods.List {
						ft, ok := m.Type.(*ast.FuncType)
						if !ok || len(m.Names) != 1 || !ownRouterVerbs[m.Names[0].Name] {
							continue
						}
						if ft.Results == nil && takesHandlers(ft.Params) {
							add(ts.Name.Name, m.Names[0].Name)
						}
					}
				}
			}
		}
	}
	var out []string
	for typ, set := range verbs {
		if len(set) >= 3 {
			out = append(out, typ)
		}
	}
	sort.Strings(out)
	return out
}

// ownRouterTypeNames returns the router type names a wrapper value carries, or nil
// for a wrapper around a framework.
func ownRouterTypeNames(wrapper string) []string {
	names, ok := strings.CutPrefix(wrapper, ownRouterMark)
	if !ok || names == "" {
		return nil
	}
	return strings.Split(names, ",")
}

// ownRouterFields maps each struct ("pkgDir.Type") to its fields whose type is a
// router type of a self-contained router package, sorted.
func ownRouterFields(fieldTypes, wrappers map[string]string) map[string][]string {
	routerTypes := map[string]bool{}
	for pkgDir, w := range wrappers {
		for _, name := range ownRouterTypeNames(w) {
			routerTypes[pkgDir+"."+name] = true
		}
	}
	if len(routerTypes) == 0 {
		return nil
	}
	out := map[string][]string{}
	for key, typ := range fieldTypes {
		if !routerTypes[strings.TrimLeft(typ, "*")] {
			continue
		}
		if dot := strings.LastIndexByte(key, '.'); dot > 0 {
			out[key[:dot]] = append(out[key[:dot]], key[dot+1:])
		}
	}
	for owner := range out {
		sort.Strings(out[owner])
	}
	return out
}

// importsOwnRouterOnly reports whether a file reaches a router only through a
// self-contained router package: it imports one, and no framework by name.
func importsOwnRouterOnly(f *ast.File, fileImports, wrappers map[string]string) bool {
	if fw := detectImportedRouterFramework(f); fw != "" && fw != "net/http" {
		return false
	}
	for _, pkgDir := range fileImports {
		if strings.HasPrefix(wrappers[pkgDir], ownRouterMark) {
			return true
		}
	}
	return false
}

// seedReceiverRouters records the router fields of a method's receiver, each at the
// root: `hs.RouteRegister` for `func (hs *HTTPServer) registerRoutes()`.
func seedReceiverRouters(prefixes map[string]string, pkgDir, recvVar, recvType string, routerFields map[string][]string) {
	if recvVar == "" || recvType == "" {
		return
	}
	for _, field := range routerFields[pkgDir+"."+recvType] {
		if _, set := prefixes[recvVar+"."+field]; !set {
			prefixes[recvVar+"."+field] = ""
		}
	}
}

// routerVarName is the name a router is known by in a prefix map: a variable, or a
// field of one (`hs.RouteRegister`). "" for any other expression.
func routerVarName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok {
			return x.Name + "." + e.Sel.Name
		}
	}
	return ""
}

// trackRouterAlias follows `r := hs.RouteRegister`: a variable assigned a known
// router is that router, at the prefix it has now.
func trackRouterAlias(s *ast.AssignStmt, prefixes map[string]string) {
	if len(s.Lhs) != 1 || len(s.Rhs) != 1 {
		return
	}
	lhs, ok := s.Lhs[0].(*ast.Ident)
	if !ok {
		return
	}
	if _, isSel := s.Rhs[0].(*ast.SelectorExpr); !isSel {
		return
	}
	if prefix, known := prefixes[routerVarName(s.Rhs[0])]; known {
		prefixes[lhs.Name] = prefix
	}
}

// routeHandlerExpr names the function a registration's last argument registers.
//
//	hs.LoginView                      the function itself
//	routing.Wrap(hs.LoginPost)        the one function a wrapper is given
//	hs.getDSQueryEndpoint()           the function that builds the handler
//	misc.LocationRedirect("./x")      the same, built from a value
//	wrap(mw(h.create, "message"))     nothing: which argument serves is not written
//
// Without the second form the route names the wrapper, and every wrapped route of
// the application is served by one function.
func routeHandlerExpr(arg ast.Expr) string {
	for range 3 {
		call, ok := arg.(*ast.CallExpr)
		if !ok {
			break
		}
		switch len(call.Args) {
		case 0:
			return exprToString(call.Fun)
		case 1:
			switch call.Args[0].(type) {
			case *ast.Ident, *ast.SelectorExpr, *ast.CallExpr:
				arg = call.Args[0]
			default:
				// Given a value and not a function: `redirect("./general")` builds
				// the handler, as a call with no argument does.
				return exprToString(call.Fun)
			}
		default:
			return ""
		}
	}
	switch arg.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return exprToString(arg)
	}
	return ""
}
