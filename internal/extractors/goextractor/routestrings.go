package goextractor

import (
	"go/ast"
	"go/token"
	"regexp"
	"strings"
)

// Paths a route registration names without writing them out.
//
// A mount or a group usually takes its path as a literal, and the walks read it
// there. Some take it from a name:
//
//	const artifactRouteBase = "/_apis/pipelines/workflows/{run_id}/artifacts"
//	m.Group(artifactRouteBase, func() { ... })
//
//	prefix := "/api/actions_pipeline"
//	r.Mount(prefix, actions.ArtifactsRoutes(prefix))
//
// Left unread, the group and the mount contribute nothing, and every route under
// them is stored at the fragment its own verb names. The values ride in the same
// map as the router prefixes, under keys no identifier can have, so the walks that
// already pass that map along need no second one.

// stringKey is the key a string-valued name has in a prefix map.
func stringKey(name string) string { return "\x00str\x00" + name }

// seedKey holds the prefix the function being walked was mounted or called under,
// which is where its own router starts.
const seedKey = "\x00seed"

// stringConsts holds every package-level string constant of the module:
// package directory, then name.
type stringConsts map[string]map[string]string

func buildStringConsts(pkgs map[string]*parsedPkg) stringConsts {
	out := stringConsts{}
	for pkgDir, pp := range pkgs {
		for _, f := range pp.parsedFiles {
			for _, decl := range f.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							break
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						if out[pkgDir] == nil {
							out[pkgDir] = map[string]string{}
						}
						out[pkgDir][name.Name] = strings.Trim(lit.Value, "\"`")
					}
				}
			}
		}
	}
	return out
}

// seedStrings puts into prefixes the string constants body refers to: those of its
// own package by name, and those of a package it imports as `alias.Name`.
func seedStrings(prefixes map[string]string, body *ast.BlockStmt, pkgDir string, imports map[string]string, consts stringConsts) {
	if len(consts) == 0 || body == nil {
		return
	}
	own := consts[pkgDir]
	ast.Inspect(body, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := e.X.(*ast.Ident); ok {
				if v, ok := consts[imports[pkg.Name]][e.Sel.Name]; ok {
					prefixes[stringKey(pkg.Name+"."+e.Sel.Name)] = v
				}
			}
		case *ast.Ident:
			if v, ok := own[e.Name]; ok {
				prefixes[stringKey(e.Name)] = v
			}
		}
		return true
	})
}

// pathArg returns the path an argument names: a literal as the walks have always
// read it, or the value of a string constant or local string variable. ok is false
// when the path cannot be known.
func pathArg(expr ast.Expr, prefixes map[string]string) (string, bool) {
	if isStaticStringArg(expr) {
		return extractStringExpr(expr), true
	}
	switch e := expr.(type) {
	case *ast.Ident:
		v, ok := prefixes[stringKey(e.Name)]
		return v, ok
	case *ast.SelectorExpr:
		if pkg, isIdent := e.X.(*ast.Ident); isIdent {
			v, ok := prefixes[stringKey(pkg.Name+"."+e.Sel.Name)]
			return v, ok
		}
	}
	return "", false
}

// trackStringAssign follows a local variable that holds a path: `prefix := "/api"`
// and each later `prefix = ...`. A variable assigned anything else is forgotten, so
// a name never keeps a value it no longer has.
func trackStringAssign(s *ast.AssignStmt, prefixes map[string]string) {
	if len(s.Lhs) == 1 && len(s.Rhs) == 1 {
		if ident, ok := s.Lhs[0].(*ast.Ident); ok {
			switch s.Rhs[0].(type) {
			case *ast.BasicLit, *ast.Ident, *ast.SelectorExpr:
				if v, ok := pathArg(s.Rhs[0], prefixes); ok {
					prefixes[stringKey(ident.Name)] = v
					return
				}
			}
		}
	}
	for _, lhs := range s.Lhs {
		if ident, ok := lhs.(*ast.Ident); ok {
			delete(prefixes, stringKey(ident.Name))
		}
	}
}

var matchPathParam = regexp.MustCompile(`<([^:>]*)(?::[^>]*)?>`)

// matchPathPattern rewrites a path-group pattern into the notation the other routes
// of a chi-style router are stored in: `<name>` and `<name:rule>` are the parameter
// {name}, and `<*:*>` is the wildcard.
func matchPathPattern(pattern string) string {
	return matchPathParam.ReplaceAllStringFunc(pattern, func(m string) string {
		name := matchPathParam.FindStringSubmatch(m)[1]
		if name == "" || name == "*" {
			return "*"
		}
		return "{" + name + "}"
	})
}

// extractMatchPathRoutes reads a route registered on a path group:
//
//	g.MatchPath("HEAD,GET", "/<group:*>/repodata/<filename>", mw, handler)
//
// One route per method, handled by the last argument.
func extractMatchPathRoutes(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string) []routeInfo {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "MatchPath" || len(call.Args) < 3 {
		return nil
	}
	if !isStaticStringArg(call.Args[0]) || !isStaticStringArg(call.Args[1]) {
		return nil
	}
	path := prefixes[identName(sel.X)] + matchPathPattern(extractStringArg(call, 1))
	handler := exprToString(call.Args[len(call.Args)-1])
	var out []routeInfo
	for _, method := range strings.Split(extractStringArg(call, 0), ",") {
		method = strings.ToUpper(strings.TrimSpace(method))
		if method == "" {
			continue
		}
		out = append(out, routeInfo{
			method:    method,
			path:      path,
			handler:   handler,
			framework: "chi",
			line:      fset.Position(call.Pos()).Line,
		})
	}
	return out
}
