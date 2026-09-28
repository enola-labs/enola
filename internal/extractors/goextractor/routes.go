package goextractor

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// routeInfo holds a detected route registration.
type routeInfo struct {
	method    string // "GET", "POST", etc. or "ALL"
	path      string // e.g. "/api/registration-enabled"
	handler   string // e.g. "app.Handlers.User.CreatePasswordReset"
	framework string // "gorilla/mux", "chi", "net/http"
	line      int
}

// extractRoutes walks function bodies in a Go file looking for HTTP route registrations.
func extractRoutes(fset *token.FileSet, f *ast.File, relFile, pkgDir string, index routePrefixIndex) []facts.Fact {
	var result []facts.Fact

	// Detect which router framework is imported
	framework := detectRouterFramework(f)
	if framework == "" {
		return nil
	}

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}

		// Seed this function's prefix map from the interprocedural index: when its
		// router parameter is mounted under one or more prefixes (a subrouter passed
		// in from a caller), emit its routes once per distinct prefix. A function
		// absent from the index (a root/unreached/simple function that owns its
		// router) gets a single empty seed — byte-for-byte today's behavior.
		paramName, seeds := seedsFor(index, funcKeyFor(fn, pkgDir))
		for _, seed := range seeds {
			prefixes := make(map[string]string)
			if paramName != "" && seed != "" {
				prefixes[paramName] = seed
			}
			for _, stmt := range fn.Body.List {
				extractRoutesFromStmt(fset, stmt, prefixes, framework, relFile, pkgDir, &result)
			}
		}
	}

	return result
}

// seedsFor returns the router-parameter name and the sorted seed prefixes for a
// function, defaulting to a single empty seed (today's behavior) when the index
// has no entry for it.
func seedsFor(index routePrefixIndex, key string) (string, []string) {
	if index != nil {
		if e := index[key]; e != nil && len(e.seeds) > 0 {
			return e.paramName, e.seeds
		}
	}
	return "", []string{""}
}

// detectRouterFramework checks imports to determine which router framework is used.
// Specific frameworks (gorilla/mux, chi) take priority over net/http.
func detectRouterFramework(f *ast.File) string {
	hasNetHTTP := false
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		switch {
		case strings.Contains(path, "gorilla/mux"):
			return "gorilla/mux"
		case strings.Contains(path, "go-chi/chi"):
			return "chi"
		case strings.Contains(path, "gin-gonic/gin"):
			return "gin"
		case path == "net/http":
			hasNetHTTP = true
		}
	}
	if hasNetHTTP {
		return "net/http"
	}
	return ""
}

// extractRoutesFromStmt processes a single statement looking for route registrations and subrouter assignments.
func extractRoutesFromStmt(fset *token.FileSet, stmt ast.Stmt, prefixes map[string]string, framework, relFile, pkgDir string, result *[]facts.Fact) {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		if call, ok := s.X.(*ast.CallExpr); ok {
			routes := extractRoutesFromCall(fset, call, prefixes, framework)
			for _, r := range routes {
				*result = append(*result, routeToFact(r, relFile, pkgDir))
			}
			// Recurse into function literal arguments (e.g. r.Group(func(r chi.Router) { r.Get(...) })).
			// This captures routes registered inside oapi-codegen's HandlerWithOptions pattern.
			for _, arg := range call.Args {
				if fn, ok := arg.(*ast.FuncLit); ok && fn.Body != nil {
					for _, inner := range fn.Body.List {
						extractRoutesFromStmt(fset, inner, prefixes, framework, relFile, pkgDir, result)
					}
				}
			}
		}

	case *ast.AssignStmt:
		// Track subrouter assignments: apiRouter := router.PathPrefix("/api").Subrouter()
		applySubrouterAssign(s, prefixes)
		// Also check for route registrations in assignments (e.g. _ = router.HandleFunc(...))
		for _, rhs := range s.Rhs {
			if call, ok := rhs.(*ast.CallExpr); ok {
				routes := extractRoutesFromCall(fset, call, prefixes, framework)
				for _, r := range routes {
					*result = append(*result, routeToFact(r, relFile, pkgDir))
				}
			}
		}

	case *ast.IfStmt:
		// Walk into if bodies for conditional route registration
		if s.Body != nil {
			for _, inner := range s.Body.List {
				extractRoutesFromStmt(fset, inner, prefixes, framework, relFile, pkgDir, result)
			}
		}
		if s.Else != nil {
			if block, ok := s.Else.(*ast.BlockStmt); ok {
				for _, inner := range block.List {
					extractRoutesFromStmt(fset, inner, prefixes, framework, relFile, pkgDir, result)
				}
			}
		}
	}
}

// extractRoutesFromCall extracts route info from a call expression.
func extractRoutesFromCall(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string, framework string) []routeInfo {
	switch framework {
	case "gorilla/mux":
		return extractGorillaMuxRoutes(fset, call, prefixes)
	case "chi":
		return extractChiRoutes(fset, call, prefixes)
	case "gin":
		return extractGinRoutes(fset, call, prefixes)
	case "net/http":
		return extractNetHTTPRoutes(fset, call)
	}
	return nil
}

// extractGorillaMuxRoutes handles Gorilla Mux patterns.
func extractGorillaMuxRoutes(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string) []routeInfo {
	// Pattern 1: router.HandleFunc("/path", handler).Methods("GET")
	// AST: CallExpr{Fun: SelectorExpr{X: CallExpr{HandleFunc}, Sel: Methods}, Args: ["GET"]}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Methods" {
		if innerCall, ok := sel.X.(*ast.CallExpr); ok {
			method := extractStringArg(call, 0)
			ri := extractHandleFuncCall(fset, innerCall, prefixes)
			if ri != nil {
				ri.method = strings.ToUpper(method)
				ri.framework = "gorilla/mux"
				return []routeInfo{*ri}
			}
		}
		return nil
	}

	// Pattern 2: router.HandleFunc("/path", handler) without .Methods()
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		switch sel.Sel.Name {
		case "HandleFunc", "Handle":
			ri := extractHandleFuncCall(fset, call, prefixes)
			if ri != nil {
				ri.method = "ALL"
				ri.framework = "gorilla/mux"
				return []routeInfo{*ri}
			}

		case "Use":
			// router.Use(middleware)
			if len(call.Args) > 0 {
				handler := exprToString(call.Args[0])
				if handler != "" {
					receiverVar := identName(sel.X)
					prefix := prefixes[receiverVar]
					return []routeInfo{{
						method:    "USE",
						path:      prefix,
						handler:   handler,
						framework: "gorilla/mux",
						line:      fset.Position(call.Pos()).Line,
					}}
				}
			}
		}
	}

	return nil
}

// extractChiRoutes handles Chi router patterns like r.Get("/path", handler).
func extractChiRoutes(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string) []routeInfo {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	methodName := sel.Sel.Name
	var httpMethod string

	switch methodName {
	case "Get":
		httpMethod = "GET"
	case "Post":
		httpMethod = "POST"
	case "Put":
		httpMethod = "PUT"
	case "Delete":
		httpMethod = "DELETE"
	case "Patch":
		httpMethod = "PATCH"
	case "Head":
		httpMethod = "HEAD"
	case "Options":
		httpMethod = "OPTIONS"
	case "HandleFunc", "Handle":
		httpMethod = "ALL"
	case "Use":
		if len(call.Args) > 0 {
			handler := exprToString(call.Args[0])
			if handler != "" {
				receiverVar := identName(sel.X)
				prefix := prefixes[receiverVar]
				return []routeInfo{{
					method:    "USE",
					path:      prefix,
					handler:   handler,
					framework: "chi",
					line:      fset.Position(call.Pos()).Line,
				}}
			}
		}
		return nil
	default:
		return nil
	}

	if len(call.Args) < 1 {
		return nil
	}

	path := extractStringArg(call, 0)
	if path == "" && !isStaticStringArg(call.Args[0]) {
		return nil
	}

	receiverVar := identName(sel.X)
	if prefix, ok := prefixes[receiverVar]; ok {
		path = prefix + path
	}
	if path == "" {
		path = "/"
	}

	handler := ""
	if len(call.Args) >= 2 {
		handler = exprToString(call.Args[1])
	}

	return []routeInfo{{
		method:    httpMethod,
		path:      path,
		handler:   handler,
		framework: "chi",
		line:      fset.Position(call.Pos()).Line,
	}}
}

// ginVerbs maps gin's registration methods to their HTTP method.
//
// gin spells its verbs in UPPERCASE (`r.GET`), where chi uses Go-cased names
// (`r.Get`). That difference is load-bearing rather than cosmetic: the two vocabularies
// do not overlap, so a file cannot be ambiguous between the two routers even before the
// per-file import gate decides which extractor runs.
var ginVerbs = map[string]string{
	"GET": "GET", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE",
	"PATCH": "PATCH", "HEAD": "HEAD", "OPTIONS": "OPTIONS",
	"Any": "ALL",
}

// extractGinRoutes handles gin patterns: r.GET("/path", handler), a group's
// g.POST("/path", handler), and the generic r.Handle("GET", "/path", handler).
//
// The path is joined against the group prefix rather than concatenated, because
// `server.Group("/")` is gin's idiomatic way to spell a group that adds no prefix and
// is pervasive in real code — ente's server opens seven of them. Concatenating would
// turn every route under one into `//ping`, a path the server does not serve and which
// no client route would ever match.
func extractGinRoutes(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string) []routeInfo {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	methodName := sel.Sel.Name
	httpMethod, isVerb := ginVerbs[methodName]
	pathArg, handlerArg := 0, 1

	switch {
	case isVerb:
	case methodName == "Handle":
		// r.Handle("GET", "/path", handler) — the method is the first argument, so
		// everything shifts by one. A non-literal method is left as ALL rather than
		// guessed.
		httpMethod = strings.ToUpper(extractStringArg(call, 0))
		if httpMethod == "" {
			httpMethod = "ALL"
		}
		pathArg, handlerArg = 1, 2
	case methodName == "Use":
		if len(call.Args) == 0 {
			return nil
		}
		handler := exprToString(call.Args[0])
		if handler == "" {
			return nil
		}
		return []routeInfo{{
			method:    "USE",
			path:      prefixes[identName(sel.X)],
			handler:   handler,
			framework: "gin",
			line:      fset.Position(call.Pos()).Line,
		}}
	default:
		return nil
	}

	if len(call.Args) <= pathArg {
		return nil
	}
	path := extractStringArg(call, pathArg)
	if path == "" && !isStaticStringArg(call.Args[pathArg]) {
		// A computed path is skipped rather than published bare: gin groups mount at
		// "/" so often that a bare path is frequently the root itself, the same reason
		// the C# minimal-API pass is stricter here than the Go mux one.
		return nil
	}

	path = joinRoutePath(prefixes[identName(sel.X)], path)

	handler := ""
	if len(call.Args) > handlerArg {
		// gin takes variadic middleware before the final handler; the LAST argument is
		// the one that serves the request.
		handler = exprToString(call.Args[len(call.Args)-1])
	}

	return []routeInfo{{
		method:    httpMethod,
		path:      path,
		handler:   handler,
		framework: "gin",
		line:      fset.Position(call.Pos()).Line,
	}}
}

// joinRoutePath joins a group prefix and a route path into one clean path.
//
// Deliberately not plain concatenation, which is what the mux and chi paths do: those
// routers spell an empty mount as "" while gin spells it "/", so gin is the one router
// here where the naive join produces a doubled separator on the common case.
func joinRoutePath(prefix, path string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	switch {
	case path == "":
		if prefix == "" {
			return "/"
		}
		return prefix
	case !strings.HasPrefix(path, "/"):
		return prefix + "/" + path
	default:
		return prefix + path
	}
}

// extractNetHTTPRoutes handles net/http patterns like http.HandleFunc("/path", handler).
func extractNetHTTPRoutes(fset *token.FileSet, call *ast.CallExpr) []routeInfo {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	if sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle" {
		return nil
	}

	// Check it's called on http or a mux variable
	if len(call.Args) < 1 {
		return nil
	}

	path := extractStringArg(call, 0)
	if path == "" && !isStaticStringArg(call.Args[0]) {
		return nil
	}
	if path == "" {
		path = "/"
	}

	handler := ""
	if len(call.Args) >= 2 {
		handler = exprToString(call.Args[1])
	}

	return []routeInfo{{
		method:    "ALL",
		path:      path,
		handler:   handler,
		framework: "net/http",
		line:      fset.Position(call.Pos()).Line,
	}}
}

// extractHandleFuncCall extracts path and handler from a HandleFunc/Handle call.
func extractHandleFuncCall(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string) *routeInfo {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	if sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle" {
		return nil
	}

	if len(call.Args) < 1 {
		return nil
	}

	path := extractStringArg(call, 0)
	// An empty path from a static string literal ("") is an intentional
	// collection-root registration on a subrouter (e.g. coursesRouter.HandleFunc("",
	// list)) whose real path is the subrouter's prefix. Only drop when the path is
	// empty because the argument is dynamic/unresolvable, not a literal.
	if path == "" && !isStaticStringArg(call.Args[0]) {
		return nil
	}

	// Resolve prefix from receiver variable
	receiverVar := identName(sel.X)
	if prefix, ok := prefixes[receiverVar]; ok {
		path = prefix + path
	}
	// A composed-empty path means an empty registration on a prefix-less router —
	// the runtime root.
	if path == "" {
		path = "/"
	}

	handler := ""
	if len(call.Args) >= 2 {
		handler = exprToString(call.Args[1])
	}

	return &routeInfo{
		path:    path,
		handler: handler,
		line:    fset.Position(call.Pos()).Line,
	}
}

// isStaticStringArg reports whether expr is a statically-known string path — a
// string literal (possibly empty) or a concatenation of them — versus a dynamic
// argument whose path can't be determined. An empty path from a static literal is
// an intentional collection-root registration; an empty path from a non-static
// arg means the path is unknown and the route must be dropped.
func isStaticStringArg(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.BinaryExpr:
		return e.Op == token.ADD && (isStaticStringArg(e.X) || isStaticStringArg(e.Y))
	}
	return false
}

// applySubrouterAssign records the prefix of a subrouter variable assigned by
// `x := parent.PathPrefix("/p").Subrouter()`, composing the parent router's known
// prefix. It is the single source of truth for subrouter-prefix composition,
// shared by route-fact emission (extractRoutesFromStmt) and the interprocedural
// pre-pass (analyzeRouterFunc) so the two can never drift.
func applySubrouterAssign(s *ast.AssignStmt, prefixes map[string]string) {
	if len(s.Lhs) != 1 || len(s.Rhs) != 1 {
		return
	}
	ident, ok := s.Lhs[0].(*ast.Ident)
	if !ok {
		return
	}
	prefix := extractSubrouterPrefix(s.Rhs[0])
	if prefix == "" {
		return
	}
	if parentPrefix, ok := prefixes[extractReceiverVar(s.Rhs[0])]; ok {
		// Joined rather than concatenated so a nested gin group under a "/" parent
		// (`storageAPI := privateAPI.Group("/")`) does not become "//". The two forms
		// agree on every other input, including every mux PathPrefix chain.
		prefix = joinRoutePath(parentPrefix, prefix)
	}
	prefixes[ident.Name] = prefix
}

// extractSubrouterPrefix extracts the path prefix from a PathPrefix(...).Subrouter() chain.
func extractSubrouterPrefix(expr ast.Expr) string {
	// Pattern: router.PathPrefix("/api").Subrouter()
	// AST: CallExpr{Fun: SelectorExpr{X: CallExpr{PathPrefix}, Sel: Subrouter}}
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return ""
	}

	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}

	if sel.Sel.Name == "Subrouter" {
		// The receiver should be PathPrefix(...)
		if innerCall, ok := sel.X.(*ast.CallExpr); ok {
			if innerSel, ok := innerCall.Fun.(*ast.SelectorExpr); ok {
				if innerSel.Sel.Name == "PathPrefix" && len(innerCall.Args) >= 1 {
					return extractStringArg(innerCall, 0)
				}
			}
		}
	}

	// gin: `admin := server.Group("/admin")` binds a prefix to a variable.
	//
	// Gated on the argument being a STRING LITERAL, not on the framework, because chi
	// declares a `Group` too — with a completely different meaning: `r.Group(func(r
	// chi.Router){...})` takes a function and mounts nothing. The argument's shape
	// discriminates the two structurally, so neither router needs to be named here and
	// a chi Group cannot be mistaken for a mount.
	if sel.Sel.Name == "Group" && len(call.Args) >= 1 && isStaticStringArg(call.Args[0]) {
		return extractStringArg(call, 0)
	}

	return ""
}

// extractReceiverVar returns the variable name that the Subrouter() chain is called on.
func extractReceiverVar(expr ast.Expr) string {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if sel.Sel.Name == "Subrouter" {
		if innerCall, ok := sel.X.(*ast.CallExpr); ok {
			if innerSel, ok := innerCall.Fun.(*ast.SelectorExpr); ok {
				return identName(innerSel.X)
			}
		}
	}
	return ""
}

// extractStringArg returns the string value of the argument at the given index, or "".
func extractStringArg(call *ast.CallExpr, index int) string {
	if index >= len(call.Args) {
		return ""
	}
	return extractStringExpr(call.Args[index])
}

// extractStringExpr extracts a string value from an expression.
// Handles plain string literals and binary concatenation (e.g. options.BaseURL+"/path").
// For concatenations where only the right side is a path literal (starts with "/"),
// the path portion is returned so oapi-codegen patterns resolve correctly.
func extractStringExpr(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return strings.Trim(e.Value, `"`)
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			right := extractStringExpr(e.Y)
			// When the right side is an absolute path ("/..."), use it directly.
			// This correctly handles `options.BaseURL + "/api/v1/..."` where BaseURL
			// is a runtime variable that is typically empty or a host prefix.
			if strings.HasPrefix(right, "/") {
				return right
			}
			left := extractStringExpr(e.X)
			return left + right
		}
	}
	return ""
}

// exprToString converts an expression to a human-readable string.
// Handles selector chains like app.Handlers.User.CreatePasswordReset.
func exprToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		x := exprToString(e.X)
		if x != "" {
			return x + "." + e.Sel.Name
		}
		return e.Sel.Name
	case *ast.CallExpr:
		// For function call expressions like middleware.RateLimitMiddleware(...)
		return exprToString(e.Fun)
	}
	return ""
}

// identName returns the name of an identifier expression, or "".
func identName(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// routeToFact converts a routeInfo to a facts.Fact.
func routeToFact(r routeInfo, relFile, pkgDir string) facts.Fact {
	props := map[string]any{
		"method":    r.method,
		"framework": r.framework,
		"language":  "go",
	}
	if r.handler != "" {
		props["handler"] = r.handler
	}
	if r.method == "USE" {
		props["type"] = "middleware"
	}

	return facts.Fact{
		Kind:  facts.KindRoute,
		Name:  r.path,
		File:  relFile,
		Line:  r.line,
		Props: props,
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: pkgDir},
		},
	}
}

// handlerTarget names the function a route's handler expression registers, in the
// form Go call targets take, when the expression alone settles it: `Home` is the
// file's own package's function, `repo.Home` the function of the package the file
// imports as repo. A method value (`h.Home`, a receiver variable) or a call
// (`routing.Wrap(h.Home)`) names no function by itself and gets "".
func handlerTarget(handler, pkgDir string, fileImports map[string]string) string {
	if handler == "" || strings.ContainsAny(handler, "()[]{} *&") {
		return ""
	}
	parts := strings.Split(handler, ".")
	switch len(parts) {
	case 1:
		if !token.IsIdentifier(parts[0]) {
			return ""
		}
		return pkgDir + "." + parts[0]
	case 2:
		pkg, ok := fileImports[parts[0]]
		if !ok || !token.IsIdentifier(parts[1]) {
			return ""
		}
		return pkg + "." + parts[1]
	}
	return ""
}
