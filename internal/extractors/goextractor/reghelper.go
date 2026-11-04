package goextractor

import (
	"go/ast"
	"go/token"
	"strings"
)

// Route registration through a helper of the module's own.
//
// A service may register every route through a function of its own instead of the
// router's verbs:
//
//	func RegisterEndpoint(r chi.Router, method, pattern, operationID string, h Handler) {
//		r.Method(method, pattern, wrap(h))
//	}
//	RegisterEndpoint(r, http.MethodGet, OrdersURL, "listOrders", ordersHandler.Handle)
//
// The verb call inside the helper has neither a method nor a path to read, and the
// helper often forwards to a library's registration function that is not in the
// module at all. What the module does hold is the helper's signature and its calls.
// A function is a registration helper when its parameters are a router, a string
// named for an HTTP method, and a string named for a path; each call to it is a route
// at the method and path its arguments give, served by its handler argument, under
// the prefix its router argument is mounted at. A string parameter named for an
// operation id carries the operationId the route serves.

// regHelper is where a registration helper takes each of its arguments; -1 for one
// it does not take.
type regHelper struct {
	router, method, path, opID, handler int
}

// routerTypeSuffixes end the type of a router parameter: chi.Router, *chi.Mux,
// *mux.Router, *http.ServeMux, gin.IRouter, *gin.RouterGroup, *echo.Group.
var routerTypeSuffixes = []string{"Router", "Mux", "ServeMux", "IRouter", "IRoutes", "RouterGroup", "Group", "Engine", "Echo"}

var (
	methodParamNames = map[string]bool{"method": true, "verb": true, "httpmethod": true}
	pathParamNames   = map[string]bool{"pattern": true, "path": true, "route": true, "url": true, "endpoint": true, "uri": true}
	opIDParamNames   = map[string]bool{"operationid": true, "opid": true, "operation": true}
)

// registrationHelpers finds the module's registration helpers, keyed as calls name
// them: the package directory, a dot, the function name.
func registrationHelpers(parsedPkgs map[string]*parsedPkg) map[string]regHelper {
	out := map[string]regHelper{}
	for pkgDir, pp := range parsedPkgs {
		for _, f := range pp.parsedFiles {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Type.Params == nil {
					continue
				}
				if h, ok := helperSignature(fn.Type.Params); ok {
					out[pkgDir+"."+fn.Name.Name] = h
				}
			}
		}
	}
	return out
}

func helperSignature(params *ast.FieldList) (regHelper, bool) {
	h := regHelper{router: -1, method: -1, path: -1, opID: -1, handler: -1}
	i := 0
	for _, field := range params.List {
		typ := typeExprToString(field.Type)
		names := field.Names
		if len(names) == 0 {
			names = []*ast.Ident{nil}
		}
		for _, name := range names {
			n := ""
			if name != nil {
				n = strings.ToLower(name.Name)
			}
			switch {
			case h.router < 0 && isRouterTypeName(typ):
				h.router = i
			case typ == "string" && methodParamNames[n] && h.method < 0:
				h.method = i
			case typ == "string" && pathParamNames[n] && h.path < 0:
				h.path = i
			case typ == "string" && opIDParamNames[n] && h.opID < 0:
				h.opID = i
			case typ != "string" && h.path >= 0 && h.handler < 0:
				h.handler = i
			}
			i++
		}
	}
	return h, h.router >= 0 && h.method >= 0 && h.path >= 0
}

func isRouterTypeName(typ string) bool {
	typ = strings.TrimPrefix(typ, "*")
	if !strings.Contains(typ, ".") {
		return false
	}
	for _, s := range routerTypeSuffixes {
		if strings.HasSuffix(typ, "."+s) {
			return true
		}
	}
	return false
}

// helperRoute reads a call to a registration helper as a route; ok is false for any
// other call, or one whose method or path cannot be read.
func (w *routeWalk) helperRoute(call *ast.CallExpr) (routeInfo, string, bool) {
	if len(w.helpers) == 0 {
		return routeInfo{}, "", false
	}
	key := ""
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		key = w.pkgDir + "." + fn.Name
	case *ast.SelectorExpr:
		if pkg, ok := fn.X.(*ast.Ident); ok && w.imports[pkg.Name] != "" {
			key = w.imports[pkg.Name] + "." + fn.Sel.Name
		}
	case *ast.IndexExpr: // a generic helper instantiated explicitly
		if sel, ok := fn.X.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && w.imports[pkg.Name] != "" {
				key = w.imports[pkg.Name] + "." + sel.Sel.Name
			}
		} else if id, ok := fn.X.(*ast.Ident); ok {
			key = w.pkgDir + "." + id.Name
		}
	}
	h, ok := w.helpers[key]
	if !ok || h.method >= len(call.Args) || h.path >= len(call.Args) || h.router >= len(call.Args) {
		return routeInfo{}, "", false
	}
	method := httpMethodArg(call.Args[h.method])
	path, ok := pathArg(call.Args[h.path], w.prefixes)
	if method == "" || !ok || path == "" {
		return routeInfo{}, "", false
	}
	path = joinRoutePath(w.prefixes[routerVarName(call.Args[h.router])], path)
	r := routeInfo{method: method, path: path, framework: w.framework, line: w.fset.Position(call.Pos()).Line}
	if h.handler >= 0 && h.handler < len(call.Args) {
		r.handler = exprToString(call.Args[h.handler])
		r.target = w.constructedMethod(call.Args[h.handler])
	}
	opID := ""
	if h.opID >= 0 && h.opID < len(call.Args) && isStaticStringArg(call.Args[h.opID]) {
		opID = extractStringExpr(call.Args[h.opID])
	}
	return r, opID, true
}

// httpMethodArg reads a method argument: net/http's http.MethodGet, or a literal.
func httpMethodArg(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		if strings.HasPrefix(e.Sel.Name, "Method") {
			return strings.ToUpper(strings.TrimPrefix(e.Sel.Name, "Method"))
		}
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return strings.ToUpper(strings.Trim(e.Value, "\"`"))
		}
	}
	return ""
}

// constructedMethod names the method a handler expression takes off a value built in
// place, `handlers.NewRefund(svc).Handle`: by Go's constructor convention that is
// handlers.Refund's Handle. "" for any other expression.
func (w *routeWalk) constructedMethod(expr ast.Expr) string {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	call, ok := sel.X.(*ast.CallExpr)
	if !ok {
		return ""
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		if t, ok := strings.CutPrefix(fn.Name, "New"); ok && token.IsExported(t) {
			return w.pkgDir + "." + t + "." + sel.Sel.Name
		}
	case *ast.SelectorExpr:
		if pkg, ok := fn.X.(*ast.Ident); ok && w.imports[pkg.Name] != "" {
			if t, ok := strings.CutPrefix(fn.Sel.Name, "New"); ok && token.IsExported(t) {
				return w.imports[pkg.Name] + "." + t + "." + sel.Sel.Name
			}
		}
	}
	return ""
}
