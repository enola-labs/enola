package goextractor

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strings"
)

// A route registered on a router parameter names no mux by itself: the mux is
// whatever the caller passed. When every call of the function in the module passes
// the same router, one the caller builds itself (`webRoutes := web.NewRouter();
// registerWebRoutes(webRoutes)`), the parameter is that router, and its routes are
// scoped to it exactly as the caller's own registrations would be. Two applications
// of one program that each build their router and hand it to their own
// registration function then no longer read as one router serving a path twice.
//
// The identity is given only when it is certain. A call the module cannot resolve
// by the function's name, a reference to the function as a value, a call from
// outside a function body, two callers passing different routers, or a router the
// caller did not build itself (a field, a call's result, a router bound twice)
// leaves the parameter unscoped, as before.

// paramMux is a registration function's router parameter and the mux it is.
type paramMux struct{ param, scope string }

// buildParamMuxScopes returns, per registration function (funcKey), its router
// parameter and the mux scope it carries, for the functions every caller hands one
// router to.
func buildParamMuxScopes(fset *token.FileSet, index map[string]routerFunc, pkgs map[string]*parsedPkg, mods goModules, pkgNames map[string]string, fieldTypes map[string]string) map[string]paramMux {
	byName := map[string][]string{} // a function's own name -> funcKeys taking a router
	for key, rf := range index {
		if rf.hasParam {
			byName[rf.fn.Name.Name] = append(byName[rf.fn.Name.Name], key)
		}
	}
	if len(byName) == 0 {
		return nil
	}

	type site struct {
		caller string // funcKey of the calling function
		local  string // the caller's local router passed, scoped; "" when it passes its parameter
	}
	sites := map[string][]site{}
	unknown := map[string]bool{} // funcKeys some call or reference leaves open
	unknownName := func(name string) {
		for _, k := range byName[name] {
			unknown[k] = true
		}
	}

	pkgDirs := make([]string, 0, len(pkgs))
	for d := range pkgs {
		pkgDirs = append(pkgDirs, d)
	}
	sort.Strings(pkgDirs)
	for _, pkgDir := range pkgDirs {
		for _, f := range pkgs[pkgDir].parsedFiles {
			var imports map[string]string
			for _, decl := range f.Decls {
				fn, isFunc := decl.(*ast.FuncDecl)
				if !isFunc || fn.Body == nil {
					// A package-level value naming the function hands it out unseen.
					ast.Inspect(decl, func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok && byName[id.Name] != nil {
							unknownName(id.Name)
						}
						return true
					})
					continue
				}
				if !mentionsAny(fn.Body, byName) {
					continue
				}
				if imports == nil {
					imports = buildFileImports(f, mods, pkgNames)
				}
				callerKey := funcKeyFor(fn, pkgDir)
				recvVar, recvType := receiverOf(fn)
				ctx := resolveCtx{pkgDir: pkgDir, mods: mods, imports: imports, recvVar: recvVar, recvType: recvType, fieldTypes: fieldTypes}
				ctx.localTypes = collectLocalTypes(fn.Body, ctx)
				locals := freshLocalMuxes(fn.Body, func(p token.Pos) int { return fset.Position(p).Line })
				callerParam := ""
				if rf, ok := index[callerKey]; ok && rf.hasParam {
					callerParam = rf.paramName
				}
				// Parts of a selector (`r.Get`, `h.Routes`) are read with it, and the
				// called function expressions with their call.
				inner := map[ast.Expr]bool{}
				visit := func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.FuncLit:
						// A call from a closure runs with whatever the closure was
						// handed; its argument is not the enclosing function's to name.
						ast.Inspect(x.Body, func(m ast.Node) bool {
							if id, ok := m.(*ast.Ident); ok && byName[id.Name] != nil {
								unknownName(id.Name)
							}
							return true
						})
						return false
					case *ast.SelectorExpr:
						inner[x.Sel] = true
						if !inner[x] {
							if byName[x.Sel.Name] != nil {
								unknownName(x.Sel.Name) // a method value
							}
						}
					case *ast.Ident:
						if !inner[x] && byName[x.Name] != nil {
							unknownName(x.Name) // a function value
						}
					case *ast.CallExpr:
						inner[x.Fun] = true
						if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
							inner[sel.Sel] = true
						}
						name := calleeName(x.Fun)
						if byName[name] == nil {
							return true
						}
						key := resolveCalleeKey(x, ctx)
						rf, ok := index[key]
						if !ok || !rf.hasParam {
							if key == "" {
								unknownName(name)
							}
							return true
						}
						if rf.paramIndex >= len(x.Args) {
							unknown[key] = true
							return true
						}
						arg, isIdent := x.Args[rf.paramIndex].(*ast.Ident)
						switch {
						case !isIdent:
							unknown[key] = true
						case locals[arg.Name] > 0:
							sites[key] = append(sites[key], site{caller: callerKey, local: fmt.Sprintf("local:%s#%s@%d", callerKey, arg.Name, locals[arg.Name])})
						case arg.Name == callerParam && !assigned(fn.Body, arg.Name):
							sites[key] = append(sites[key], site{caller: callerKey})
						default:
							unknown[key] = true
						}
					}
					return true
				}
				ast.Inspect(fn.Body, visit)
			}
		}
	}

	memo := map[string]string{}
	visiting := map[string]bool{}
	var scopeOf func(key string) string
	scopeOf = func(key string) string {
		if s, ok := memo[key]; ok {
			return s
		}
		if unknown[key] || len(sites[key]) == 0 || visiting[key] {
			return ""
		}
		visiting[key] = true
		scope := ""
		for i, s := range sites[key] {
			got := s.local
			if got == "" {
				got = scopeOf(s.caller)
			}
			if got == "" || (i > 0 && got != scope) {
				scope = ""
				break
			}
			scope = got
		}
		delete(visiting, key)
		memo[key] = scope
		return scope
	}
	out := map[string]paramMux{}
	for key := range sites {
		if s := scopeOf(key); s != "" {
			out[key] = paramMux{param: index[key].paramName, scope: s}
		}
	}
	return out
}

// mentionsAny reports whether body names any of the functions.
func mentionsAny(body *ast.BlockStmt, names map[string][]string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && names[id.Name] != nil {
			found = true
		}
		return !found
	})
	return found
}

// calleeName is the last name of a call's function expression: `f` in `f()`,
// `pkg.f()` and `h.f()`.
func calleeName(fun ast.Expr) string {
	switch x := fun.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// freshLocalMuxes returns the locals body binds exactly once, to a router it builds
// (see isFreshMux), each with the line of its binding; a name bound twice maps to 0.
// It is the rule routeWalk.noteLocalMux applies, read over the whole body.
func freshLocalMuxes(body *ast.BlockStmt, line func(token.Pos) int) map[string]int {
	out := map[string]int{}
	note := func(id *ast.Ident, value ast.Expr) {
		if id.Name == "_" {
			return
		}
		if _, seen := out[id.Name]; seen || value == nil || !isFreshMux(value) {
			out[id.Name] = 0
			return
		}
		out[id.Name] = line(id.Pos())
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				var v ast.Expr
				if len(s.Rhs) == len(s.Lhs) {
					v = s.Rhs[i]
				}
				note(id, v)
			}
		case *ast.ValueSpec:
			for i, id := range s.Names {
				var v ast.Expr
				if i < len(s.Values) {
					v = s.Values[i]
				}
				note(id, v)
			}
		}
		return true
	})
	return out
}

// assigned reports whether body assigns to name anywhere.
func assigned(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for _, lhs := range as.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// isFreshMux reports whether value builds a new router: a constructor named New...
// (`http.NewServeMux()`, `web.NewRouter()`) or a struct literal, maybe addressed.
func isFreshMux(value ast.Expr) bool {
	if u, ok := value.(*ast.UnaryExpr); ok && u.Op == token.AND {
		value = u.X
	}
	switch v := value.(type) {
	case *ast.CompositeLit:
		return true
	case *ast.CallExpr:
		switch fn := v.Fun.(type) {
		case *ast.Ident:
			return strings.HasPrefix(fn.Name, "New")
		case *ast.SelectorExpr:
			_, pkg := fn.X.(*ast.Ident)
			return pkg && strings.HasPrefix(fn.Sel.Name, "New")
		}
	}
	return false
}
