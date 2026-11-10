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

// A constant built from others of its package, `ItemURL = BaseURL + "/item"`, is
// read too: the package's constants are evaluated until none changes, so the order
// they are declared in does not matter.
func buildStringConsts(pkgs map[string]*parsedPkg) stringConsts {
	out := stringConsts{}
	for pkgDir, pp := range pkgs {
		pending := map[string]ast.Expr{}
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
						pending[name.Name] = vs.Values[i]
					}
				}
			}
		}
		known := map[string]string{}
		for changed := true; changed; {
			changed = false
			for name, expr := range pending {
				if v, ok := constString(expr, known); ok {
					known[name] = v
					delete(pending, name)
					changed = true
				}
			}
		}
		if len(known) > 0 {
			out[pkgDir] = known
		}
	}
	return out
}

// constString evaluates a string constant expression against the constants already
// known: a literal, a known name, or a + of those.
func constString(expr ast.Expr, known map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return strings.Trim(e.Value, "\"`"), true
		}
	case *ast.Ident:
		v, ok := known[e.Name]
		return v, ok
	case *ast.ParenExpr:
		return constString(e.X, known)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, ok := constString(e.X, known)
		if !ok {
			return "", false
		}
		r, ok := constString(e.Y, known)
		return l + r, ok
	}
	return "", false
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
// read it, the value of a string constant or local string variable, or a
// concatenation of those. ok is false when any part of the path cannot be known:
// `path + "*"` with path computed at run time is no route at "*".
func pathArg(expr ast.Expr, prefixes map[string]string) (string, bool) {
	if isStaticStringArg(expr) {
		return extractStringExpr(expr), true
	}
	switch e := expr.(type) {
	case *ast.ParenExpr:
		return pathArg(e.X, prefixes)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := pathArg(e.X, prefixes)
		if !ok {
			return "", false
		}
		right, ok := pathArg(e.Y, prefixes)
		return left + right, ok
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

// splitMethodPattern reads a Go 1.22 ServeMux pattern, "[METHOD ][HOST]/[PATH]",
// which chi's Handle and HandleFunc accept too: `mux.HandleFunc("POST /items", h)`
// serves POST on /items. It returns the method ("" when the pattern names none) and
// the path, with the host and the exact-match marker "{$}" dropped. A pattern that is
// not of that form is returned as it is.
func splitMethodPattern(pattern string) (method, path string) {
	path = pattern
	if i := strings.IndexAny(pattern, " \t"); i > 0 && isMethodToken(pattern[:i]) {
		method, path = pattern[:i], strings.TrimLeft(pattern[i:], " \t")
	}
	// A host comes before the first slash: "example.com/x", "api.local:8080/x".
	// A relative chi pattern (".diff", "{id}/x") is no host.
	if i := strings.Index(path, "/"); i > 0 {
		host := path[:i]
		if strings.ContainsAny(host, ".:") && !strings.HasPrefix(host, ".") && !strings.HasPrefix(host, ":") && !strings.ContainsAny(host, "{}*") {
			path = path[i:]
		}
	}
	path = strings.TrimSuffix(path, "{$}")
	return method, path
}

// isMethodToken reports an HTTP method as a ServeMux pattern spells it: upper-case
// letters only.
func isMethodToken(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return s != ""
}
