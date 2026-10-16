package goextractor

import (
	"go/ast"
	"go/token"
	"sort"
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
func extractRoutes(fset *token.FileSet, f *ast.File, relFile, pkgDir string, routes routeIndex, fileImports map[string]string) []facts.Fact {
	var result []facts.Fact
	index := routes.prefixes

	// Detect which router framework is imported
	framework := detectRouterFramework(f, fileImports, routes.wrappers)
	if framework == "" {
		return nil
	}
	// The package that implements a router registers none of the application's
	// routes: its `r.Handle("GET", pattern, h)` is the verb's own body.
	if strings.HasPrefix(routes.wrappers[pkgDir], ownRouterMark) {
		return nil
	}
	strict := importsOwnRouterOnly(f, fileImports, routes.wrappers)
	var aliases map[string]string
	if strict {
		aliases = routerAliases(f, fileImports, routes.wrappers)
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
			seedStrings(prefixes, fn.Body, pkgDir, fileImports, routes.consts)
			if paramName != "" && seed != "" {
				prefixes[paramName] = seed
				prefixes[seedKey] = seed
			}
			recvVar, recvType := receiverOf(fn)
			if strict {
				prefixes[strictKey] = "1"
				if name, _, ok := routerParam(fn, aliases); ok {
					if _, set := prefixes[name]; !set {
						prefixes[name] = ""
					}
				}
				seedReceiverRouters(prefixes, pkgDir, recvVar, recvType, routes.routerFields)
			}
			w := &routeWalk{fset: fset, prefixes: prefixes, closures: map[string]*ast.FuncLit{}, framework: framework, relFile: relFile, pkgDir: pkgDir, result: &result, recvVar: recvVar, recvType: recvType}
			for _, stmt := range fn.Body.List {
				w.stmt(stmt)
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
//
// A file that imports none of them but imports one of the module's own router
// packages (see routerWrapperPackages) registers routes in that package's
// framework: the wrapper keeps the framework's verbs, and a file that only ever
// touches the wrapper has no import that names the framework.
func detectRouterFramework(f *ast.File, fileImports, wrappers map[string]string) string {
	if fw := detectImportedRouterFramework(f); fw != "" && fw != "net/http" {
		return fw
	}
	aliases := make([]string, 0, len(fileImports))
	for alias := range fileImports {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		switch w := wrappers[fileImports[alias]]; {
		case w == "chi", strings.HasPrefix(w, ownRouterMark):
			return "chi"
		case w == "mux":
			return "gorilla/mux"
		}
	}
	return detectImportedRouterFramework(f)
}

// detectImportedRouterFramework is the framework a file's own imports name.
func detectImportedRouterFramework(f *ast.File) string {
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

// routeWalk is the state of one walk over a registration function's body: the
// prefix each router variable carries at the statement being read, and the local
// closures that register routes when called.
type routeWalk struct {
	fset      *token.FileSet
	prefixes  map[string]string
	closures  map[string]*ast.FuncLit
	framework string
	relFile   string
	pkgDir    string
	result    *[]facts.Fact
	// recvVar and recvType are the receiver of the method being walked, "" in a
	// plain function.
	recvVar  string
	recvType string
}

func (w *routeWalk) emit(call *ast.CallExpr) {
	for _, r := range extractRoutesFromCall(w.fset, call, w.prefixes, w.framework) {
		fact := routeToFact(r, w.relFile, w.pkgDir)
		// A method value on the receiver, `hs.LoginPost` inside a method of
		// HTTPServer, names that type's method.
		if recv, method, ok := strings.Cut(r.handler, "."); ok && recv == w.recvVar && w.recvType != "" &&
			token.IsIdentifier(method) && r.method != "USE" {
			fact.SetProp(facts.PropHandlerTarget, w.pkgDir+"."+w.recvType+"."+method)
		}
		*w.result = append(*w.result, fact)
	}
}

func (w *routeWalk) block(body *ast.BlockStmt) {
	for _, inner := range body.List {
		w.stmt(inner)
	}
}

// stmt processes a single statement looking for route registrations and subrouter assignments.
func (w *routeWalk) stmt(stmt ast.Stmt) {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		call, ok := s.X.(*ast.CallExpr)
		if !ok {
			return
		}
		// A local closure that registers routes, called here: its routes are
		// registered now, under whatever this statement sits under. Removed while
		// it is walked so a closure that calls itself ends.
		if name := identName(call.Fun); name != "" && len(call.Args) == 0 {
			if lit := w.closures[name]; lit != nil {
				delete(w.closures, name)
				w.block(lit.Body)
				w.closures[name] = lit
				return
			}
		}
		w.emit(call)
		mounted, scoped := funcLitMount(call, w.prefixes)
		// Recurse into function literal arguments (e.g. r.Group(func(r chi.Router) { r.Get(...) })).
		// This captures routes registered inside oapi-codegen's HandlerWithOptions pattern.
		for _, arg := range call.Args {
			fn, ok := arg.(*ast.FuncLit)
			if !ok || fn.Body == nil {
				continue
			}
			if fn == mounted {
				restore := scoped.apply(w.prefixes)
				w.block(fn.Body)
				restore()
				continue
			}
			w.block(fn.Body)
		}

	case *ast.AssignStmt:
		// Track subrouter assignments: apiRouter := router.PathPrefix("/api").Subrouter()
		applySubrouterAssign(s, w.prefixes)
		trackStringAssign(s, w.prefixes)
		trackRouterAlias(s, w.prefixes)
		// `addRoutes := func() { m.Get(...) }` registers nothing until it is called.
		if len(s.Lhs) == 1 && len(s.Rhs) == 1 {
			if lit, ok := s.Rhs[0].(*ast.FuncLit); ok && lit.Body != nil && lit.Type.Params.NumFields() == 0 {
				if name := identName(s.Lhs[0]); name != "" {
					w.closures[name] = lit
				}
			}
		}
		// Also check for route registrations in assignments (e.g. _ = router.HandleFunc(...))
		for _, rhs := range s.Rhs {
			if call, ok := rhs.(*ast.CallExpr); ok {
				w.emit(call)
			}
		}

	case *ast.IfStmt:
		// Walk into if bodies for conditional route registration
		if s.Body != nil {
			w.block(s.Body)
		}
		if s.Else != nil {
			if block, ok := s.Else.(*ast.BlockStmt); ok {
				w.block(block)
			}
		}
	}
}

// mountScope is the prefix a router variable carries inside a mount's function
// literal, and nowhere else.
type mountScope struct {
	name   string
	prefix string
}

// apply sets the scoped prefix and returns the function that undoes it.
func (m mountScope) apply(prefixes map[string]string) func() {
	old, had := prefixes[m.name]
	prefixes[m.name] = m.prefix
	return func() {
		if had {
			prefixes[m.name] = old
		} else {
			delete(prefixes, m.name)
		}
	}
}

// funcLitMount recognizes a mount that takes the routes under it as a function
// literal, and returns the literal with the prefix its routes are registered
// under:
//
//	r.Route("/api", func(r chi.Router) { r.Get("/users", list) })   // /api/users
//	m.Group("/user", func() { m.Post("/login", signIn) }, mw...)    // /user/login
//	r.Group(func(r chi.Router) { r.Get("/users", list) })           // no new segment
//	m.PathGroup("/compare/*", func(g *web.RouterPathGroup) { ... }) // /compare
//
// The literal's router parameter carries the prefix inside it. A literal that
// takes no parameter registers on the receiver itself, so the receiver carries
// it for the length of the literal. A Route whose path is not a literal mounts
// under something this cannot read, and is left to the caller as before.
func funcLitMount(call *ast.CallExpr, prefixes map[string]string) (*ast.FuncLit, mountScope) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Route" && sel.Sel.Name != "Group" && sel.Sel.Name != "PathGroup") {
		return nil, mountScope{}
	}
	recv := routerVarName(sel.X)
	if recv == "" {
		return nil, mountScope{}
	}
	if _, known := prefixes[recv]; !known && prefixes[strictKey] != "" {
		return nil, mountScope{}
	}
	var lit *ast.FuncLit
	for _, arg := range call.Args {
		if fn, ok := arg.(*ast.FuncLit); ok && fn.Body != nil {
			lit = fn
			break
		}
	}
	if lit == nil {
		return nil, mountScope{}
	}
	prefix := prefixes[recv]
	path, known := pathArg(call.Args[0], prefixes)
	switch {
	case known:
		if sel.Sel.Name == "PathGroup" {
			// The group's own pattern ends in the wildcard its routes match under.
			path = strings.TrimSuffix(strings.TrimSuffix(path, "*"), "/")
		}
		if path != "" {
			prefix = joinRoutePath(prefix, path)
		}
	case sel.Sel.Name != "Group" || call.Args[0] != ast.Expr(lit):
		return nil, mountScope{}
	}
	name := recv
	if params := lit.Type.Params; params != nil && len(params.List) > 0 && len(params.List[0].Names) > 0 {
		name = params.List[0].Names[0].Name
	}
	return lit, mountScope{name: name, prefix: prefix}
}

// extractRoutesFromCall extracts route info from a call expression.
func extractRoutesFromCall(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string, framework string) []routeInfo {
	switch framework {
	case "gorilla/mux":
		return extractGorillaMuxRoutes(fset, call, prefixes)
	case "chi":
		routes := extractChiRoutes(fset, call, prefixes)
		// A file that imports the application's own router package for something
		// else may still register on net/http.
		if routes == nil && prefixes[strictKey] != "" {
			return extractNetHTTPRoutes(fset, call, prefixes)
		}
		return routes
	case "gin":
		return extractGinRoutes(fset, call, prefixes)
	case "net/http":
		return extractNetHTTPRoutes(fset, call, prefixes)
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
	// Through a self-contained router package a verb counts only on a known router.
	if prefixes[strictKey] != "" {
		if _, known := prefixes[routerVarName(sel.X)]; !known {
			return nil
		}
	}
	if routes := extractComboRoutes(fset, call, prefixes); routes != nil {
		return routes
	}
	if routes := extractMatchPathRoutes(fset, call, prefixes); routes != nil {
		return routes
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
				receiverVar := routerVarName(sel.X)
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

	// A registration names a path and what serves it. One argument is a lookup
	// that happens to share the verb's name: `header.Get("Authorization")`,
	// `query.Get("expires")`.
	if len(call.Args) < 2 {
		return nil
	}

	path, known := pathArg(call.Args[0], prefixes)
	if !known {
		return nil
	}
	receiverVar := routerVarName(sel.X)
	prefix := prefixes[receiverVar]
	// A pattern with no leading slash continues the group it is written in:
	// `m.Get(".diff", h)` under "/{index}" is "/{index}.diff". Written on the router
	// itself, with no group around it, it is served with the slash.
	if path != "" && !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "*") &&
		prefix == prefixes[seedKey] {
		path = "/" + path
	}
	path = prefix + path
	if path == "" {
		path = "/"
	}

	handler := ""
	if len(call.Args) >= 2 {
		// chi's verbs take one handler, so it is both the second argument and the
		// last. A router with chi's verb names that takes middleware before the
		// handler (`m.Post("/login", bind(form), SignInPost)`) puts it last, and
		// reading the second argument there names a middleware as the handler.
		handler = routeHandlerExpr(call.Args[len(call.Args)-1])
	}

	return []routeInfo{{
		method:    httpMethod,
		path:      path,
		handler:   handler,
		framework: "chi",
		line:      fset.Position(call.Pos()).Line,
	}}
}

// chiVerbs maps the verb methods of a chi-style router to their HTTP method.
var chiVerbs = map[string]string{
	"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE",
	"Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS",
}

// extractComboRoutes reads a path named once with its verbs chained after it:
//
//	m.Combo("/new").Get(NewIssue).Post(bind(form), NewIssuePost)
//
// One route per verb, all at the Combo's path, each handled by its own last
// argument. Anything in the chain that is not a verb means this is some other
// chain, and nothing is read from it.
func extractComboRoutes(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string) []routeInfo {
	var verbs []*ast.CallExpr
	for cur := call; ; {
		sel, ok := cur.Fun.(*ast.SelectorExpr)
		if !ok {
			return nil
		}
		if sel.Sel.Name == "Combo" {
			if len(verbs) == 0 || len(cur.Args) == 0 {
				return nil
			}
			path, known := pathArg(cur.Args[0], prefixes)
			if !known {
				return nil
			}
			if prefix, ok := prefixes[identName(sel.X)]; ok {
				path = prefix + path
			}
			if path == "" {
				path = "/"
			}
			routes := make([]routeInfo, 0, len(verbs))
			// Collected outermost first; emitted in the order they were written.
			for i := len(verbs) - 1; i >= 0; i-- {
				verb := verbs[i]
				handler := ""
				if len(verb.Args) > 0 {
					handler = exprToString(verb.Args[len(verb.Args)-1])
				}
				name := verb.Fun.(*ast.SelectorExpr).Sel
				routes = append(routes, routeInfo{
					method:    chiVerbs[name.Name],
					path:      path,
					handler:   handler,
					framework: "chi",
					line:      fset.Position(name.Pos()).Line,
				})
			}
			return routes
		}
		if _, isVerb := chiVerbs[sel.Sel.Name]; !isVerb {
			return nil
		}
		inner, ok := sel.X.(*ast.CallExpr)
		if !ok {
			return nil
		}
		verbs = append(verbs, cur)
		cur = inner
	}
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
	pathIdx, handlerArg := 0, 1

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
		pathIdx, handlerArg = 1, 2
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

	if len(call.Args) <= pathIdx {
		return nil
	}
	path, known := pathArg(call.Args[pathIdx], prefixes)
	if !known {
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
		handler = routeHandlerExpr(call.Args[len(call.Args)-1])
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
func extractNetHTTPRoutes(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string) []routeInfo {
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

	path, known := pathArg(call.Args[0], prefixes)
	if !known {
		return nil
	}
	if path == "" {
		path = "/"
	}

	handler := ""
	if len(call.Args) >= 2 {
		handler = routeHandlerExpr(call.Args[1])
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

	// An empty path from a static string literal ("") is an intentional
	// collection-root registration on a subrouter (e.g. coursesRouter.HandleFunc("",
	// list)) whose real path is the subrouter's prefix. Only drop when the path is
	// unknown because some part of the argument is dynamic.
	path, known := pathArg(call.Args[0], prefixes)
	if !known {
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
		handler = routeHandlerExpr(call.Args[1])
	}

	return &routeInfo{
		path:    path,
		handler: handler,
		line:    fset.Position(call.Pos()).Line,
	}
}

// isStaticStringArg reports whether expr is a statically-known string path — a
// string literal (possibly empty) or a concatenation of nothing but them — versus a
// dynamic argument whose path can't be determined. An empty path from a static literal is
// an intentional collection-root registration; an empty path from a non-static
// arg means the path is unknown and the route must be dropped.
func isStaticStringArg(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.BinaryExpr:
		return e.Op == token.ADD && isStaticStringArg(e.X) && isStaticStringArg(e.Y)
	case *ast.ParenExpr:
		return isStaticStringArg(e.X)
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
			return strings.Trim(e.Value, "\"`")
		}
	case *ast.ParenExpr:
		return extractStringExpr(e.X)
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			right := extractStringExpr(e.Y)
			// When the right side is an absolute path ("/...") after a part that is
			// not a literal, use it directly. This handles a client's
			// `options.BaseURL + "/api/v1/..."` where BaseURL is a runtime variable
			// that is typically empty or a host prefix. A literal left side is part
			// of the path: "/v1" + "/items" is /v1/items.
			if strings.HasPrefix(right, "/") && !isStaticStringArg(e.X) {
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
