package goextractor

import (
	"go/ast"
	"sort"
	"strings"
)

// This file computes, for every route-registration function in a Go module, the
// set of mount prefixes its router parameter carries at its call sites — so that
// routes registered on a router PASSED INTO a function are stored at their true
// runtime path.
//
// The base route extractor (routes.go) already composes gorilla/mux subrouter
// PathPrefix mounts, but only WITHIN a single function: it tracks
// `apiRouter := router.PathPrefix("/api").Subrouter()` in a per-function map. Real
// backends split registration across functions and packages —
// `settingsRouter := apiRouter.PathPrefix("/settings").Subrouter()` in one file,
// `RegisterCourseRoutes(settingsRouter)` calling a `func(r *mux.Router)` in
// another — so the callee's map starts empty and the route is stored as the bare
// leaf ("/courses") instead of "/api/settings/courses". That breaks client↔route
// matching and misrepresents impact_analysis/traverse.
//
// buildRoutePrefixIndex closes the gap with a repo-wide fixpoint: starting from
// root routers (mux.NewRouter() → prefix ""), it propagates the prefix a router
// argument carries at each call site to the callee's router parameter, to a
// fixpoint. extractRoutes then seeds each function's per-function prefix map from
// this index, and the existing intra-function composition does the rest.
//
// The analysis is name-based (the extractor is not type-checked, mirroring the
// rest of goextractor): a call resolves to a callee only by (package, function)
// name, and an edge is recorded only when that name is a real router-parameter
// function — so a wrong resolution can never fabricate a prefix, at most it misses
// one (leaving today's bare-path behavior).

// routePrefixEntry is the emission-facing result for one registration function:
// the name of its router parameter and the sorted set of distinct prefixes it is
// mounted under across the whole module (a function mounted at several points
// carries several prefixes, and its routes are emitted once per prefix).
type routePrefixEntry struct {
	paramName string
	seeds     []string // sorted, non-empty
}

// routePrefixIndex maps a funcKey (pkgDir + "." + [receiver + "."] + name, keyed
// exactly as extractFunc names symbols) to its mount info. A nil index or an
// absent/empty entry means "no interprocedural data" — extractRoutes then falls
// back to today's behavior (empty seed), which is the graceful-degradation path.
type routePrefixIndex map[string]*routePrefixEntry

// routeIndex is what the module-wide passes hand to per-file route extraction:
// the mount prefixes, and the module's own router packages.
type routeIndex struct {
	prefixes routePrefixIndex
	wrappers map[string]string
}

// funcKeyFor returns the package-qualified identity of a FuncDecl, matching how
// extractFunc names symbols (go.go): pkgDir + "." + [receiverType + "."] + name.
func funcKeyFor(fn *ast.FuncDecl, pkgDir string) string {
	name := fn.Name.Name
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		if recv := typeExprToString(fn.Recv.List[0].Type); recv != "" {
			name = recv + "." + name
		}
	}
	return pkgDir + "." + name
}

// routerAliases returns the import alias → framework ("mux" | "chi") for every
// router package imported by f, so router-typed parameters and NewRouter() calls
// can be recognized regardless of import aliasing. Matching mirrors
// detectRouterFramework.
func routerAliases(f *ast.File, fileImports, wrappers map[string]string) map[string]string {
	aliases := importedRouterAliases(f)
	// The module's own router packages are router packages to the files that
	// import them.
	for alias, pkgDir := range fileImports {
		if fw := wrappers[pkgDir]; fw != "" {
			if _, taken := aliases[alias]; !taken {
				aliases[alias] = fw
			}
		}
	}
	return aliases
}

// importedRouterAliases is routerAliases for the frameworks a file imports by name.
func importedRouterAliases(f *ast.File) map[string]string {
	aliases := map[string]string{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		var fw string
		switch {
		case strings.Contains(path, "gorilla/mux"):
			fw = "mux"
		case strings.Contains(path, "go-chi/chi"):
			fw = "chi"
		default:
			continue
		}
		alias := importAlias(imp, path)
		if alias == "" || alias == "_" || alias == "." {
			continue
		}
		aliases[alias] = fw
	}
	return aliases
}

// routerWrapperPackages finds the module's own router packages: a package that
// imports a router framework and declares a type with that framework's
// registration verbs. An application that wraps its router (to bind forms, to
// take middleware before the handler) registers every route through the wrapper,
// from files that never import the framework. The result maps the wrapper's
// package directory to the framework it wraps.
//
// The verbs are the evidence. A package that merely imports chi to build a
// handler declares no `Get(pattern string, ...)` method and is not one.
func routerWrapperPackages(pkgs map[string]*parsedPkg) map[string]string {
	wrappers := map[string]string{}
	for pkgDir, pp := range pkgs {
		framework := ""
		verbs := false
		for _, f := range pp.parsedFiles {
			for _, fw := range importedRouterAliases(f) {
				// Sorted so two frameworks in one package always resolve the same way.
				if framework == "" || fw < framework {
					framework = fw
				}
			}
			for _, decl := range f.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && isRegistrationVerb(fn) {
					verbs = true
				}
			}
		}
		if framework != "" && verbs {
			wrappers[pkgDir] = framework
		}
	}
	return wrappers
}

// isRegistrationVerb reports whether fn is a method that registers a route by
// verb: `func (r *Router) Get(pattern string, h ...any)`.
func isRegistrationVerb(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
		return false
	}
	switch fn.Name.Name {
	case "Get", "Post", "HandleFunc":
	default:
		return false
	}
	first, ok := fn.Type.Params.List[0].Type.(*ast.Ident)
	return ok && first.Name == "string"
}

// importAlias returns the local name an import is referenced by: its explicit
// alias, or the package's default name derived from the path (skipping a trailing
// version segment like "/v5", so "go-chi/chi/v5" → "chi").
func importAlias(imp *ast.ImportSpec, path string) string {
	if imp.Name != nil {
		return imp.Name.Name
	}
	seg := path[strings.LastIndex(path, "/")+1:]
	if isVersionSegment(seg) {
		rest := path[:strings.LastIndex(path, "/")]
		seg = rest[strings.LastIndex(rest, "/")+1:]
	}
	return seg
}

// isVersionSegment reports whether s is a semantic-import version segment (vN).
func isVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isRouterType reports whether a parameter type expression is a router:
// *mux.Router, chi.Router (interface), or *chi.Mux, under any of the given aliases.
func isRouterType(expr ast.Expr, aliases map[string]string) bool {
	for alias, fw := range aliases {
		switch fw {
		case "mux":
			if isPointerToQualifiedType(expr, alias, "Router") {
				return true
			}
		case "chi":
			// *alias.Router is how a wrapper package's own router type is taken.
			if isQualifiedType(expr, alias, "Router") || isPointerToQualifiedType(expr, alias, "Mux") || isPointerToQualifiedType(expr, alias, "Router") {
				return true
			}
		}
	}
	return false
}

// routerParam returns the name and flattened parameter index of fn's first
// named router-typed parameter. ok is false when fn takes no such parameter (or
// it is unnamed and thus unreferenceable in the body).
func routerParam(fn *ast.FuncDecl, aliases map[string]string) (name string, index int, ok bool) {
	if fn.Type == nil || fn.Type.Params == nil {
		return "", 0, false
	}
	pos := 0
	for _, field := range fn.Type.Params.List {
		n := len(field.Names)
		if n == 0 {
			n = 1 // an unnamed param still occupies a positional slot
		}
		if len(field.Names) > 0 && isRouterType(field.Type, aliases) {
			return field.Names[0].Name, pos, true
		}
		pos += n
	}
	return "", 0, false
}

// newRouterLocalVar returns the LHS variable name when s assigns a freshly-created
// root router (`x := mux.NewRouter()` / `x := chi.NewRouter()` / `chi.NewMux()`),
// or "" otherwise. Such a var anchors the propagation at prefix "".
func newRouterLocalVar(s *ast.AssignStmt, aliases map[string]string) string {
	if len(s.Lhs) != 1 || len(s.Rhs) != 1 {
		return ""
	}
	ident, ok := s.Lhs[0].(*ast.Ident)
	if !ok {
		return ""
	}
	call, ok := s.Rhs[0].(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	if _, isRouter := aliases[pkg.Name]; !isRouter {
		return ""
	}
	if sel.Sel.Name == "NewRouter" || sel.Sel.Name == "NewMux" {
		return ident.Name
	}
	return ""
}

// receiverOf returns the receiver variable name and type (star stripped) of a
// method declaration, or ("", "") for a plain function.
func receiverOf(fn *ast.FuncDecl) (string, string) {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return "", ""
	}
	field := fn.Recv.List[0]
	name := ""
	if len(field.Names) > 0 {
		name = field.Names[0].Name
	}
	return name, typeExprToString(field.Type)
}

// resolveCalleeKey resolves a call expression to the funcKey of its target, or ""
// when it cannot be resolved. It reuses the extractor's shared call resolver
// (flattenSelector + resolveChain), so it handles same-package calls (`Fn(...)`),
// package-qualified calls (`pkg.Fn(...)`), and — crucially for real backends —
// receiver-method calls whose receiver is a variable or field chain
// (`h.RegisterRoutes(r)`, `app.SettingsHandler.RegisterRoutes(r)`), resolving the
// receiver's type through localTypes/fieldTypes to the method's funcKey.
func resolveCalleeKey(call *ast.CallExpr, ctx resolveCtx) string {
	chain := flattenSelector(call.Fun)
	if chain == nil {
		return ""
	}
	return resolveChain(chain, ctx)
}

// routerArgPrefix returns the prefix a router argument carries and whether the
// argument is a recognizable router. It accepts a known router variable (present
// in prefixes) or an inline `x.PathPrefix("/p").Subrouter()` chain.
func routerArgPrefix(arg ast.Expr, prefixes map[string]string) (string, bool) {
	if ident, ok := arg.(*ast.Ident); ok {
		if p, ok := prefixes[ident.Name]; ok {
			return p, true
		}
		return "", false
	}
	if sub := extractSubrouterPrefix(arg); sub != "" {
		parent := extractReceiverVar(arg)
		return prefixes[parent] + sub, true
	}
	return "", false
}

// routerFunc is one function considered by the propagation: its declaration, its
// router parameter (if any), whether it creates a root router, and the file/
// receiver context needed to resolve its calls (including method calls, via the
// shared resolveChain type resolver).
type routerFunc struct {
	fn         *ast.FuncDecl
	pkgDir     string
	paramName  string
	paramIndex int
	hasParam   bool
	hasRoot    bool
	rootVar    string            // the first root router the body creates, else ""
	recvVar    string            // receiver var name if fn is a method, else ""
	recvType   string            // receiver type (star stripped), else ""
	imports    map[string]string // alias → target pkgDir
	aliases    map[string]string // alias → framework
}

// routerEdge records that a call in some function passes a router carrying prefix
// to the callee identified by key.
type routerEdge struct {
	callee string
	prefix string
	// mounted marks a prefix carried by the router the callee BUILDS and returns,
	// attached by the caller with Mount, rather than by a router it is passed.
	mounted bool
}

// buildRouterFuncIndex indexes every function in the module by funcKey, recording
// its router parameter, whether it roots a router, and its file's import/alias maps.
func buildRouterFuncIndex(pkgs map[string]*parsedPkg, modulePath string, pkgNames map[string]string, wrappers map[string]string) map[string]routerFunc {
	index := map[string]routerFunc{}
	pkgDirs := make([]string, 0, len(pkgs))
	for pkgDir := range pkgs {
		pkgDirs = append(pkgDirs, pkgDir)
	}
	sort.Strings(pkgDirs)

	for _, pkgDir := range pkgDirs {
		for _, f := range pkgs[pkgDir].parsedFiles {
			imports := buildFileImports(f, modulePath, pkgNames)
			aliases := routerAliases(f, imports, wrappers)
			if len(aliases) == 0 {
				continue // no router package imported → no registration functions here
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				name, idx, hasParam := routerParam(fn, aliases)
				recvVar, recvType := receiverOf(fn)
				index[funcKeyFor(fn, pkgDir)] = routerFunc{
					fn:         fn,
					pkgDir:     pkgDir,
					paramName:  name,
					paramIndex: idx,
					hasParam:   hasParam,
					hasRoot:    bodyRootVar(fn.Body, aliases) != "",
					rootVar:    bodyRootVar(fn.Body, aliases),
					recvVar:    recvVar,
					recvType:   recvType,
					imports:    imports,
					aliases:    aliases,
				}
			}
		}
	}
	return index
}

// bodyRootVar returns the variable body assigns its first freshly-created root
// router to, or "" when it creates none.
func bodyRootVar(body *ast.BlockStmt, aliases map[string]string) string {
	found := ""
	ast.Inspect(body, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		if as, ok := n.(*ast.AssignStmt); ok {
			found = newRouterLocalVar(as, aliases)
		}
		return found == ""
	})
	return found
}

// mountedRouterCall recognizes a router built by one function and attached by
// another under a path:
//
//	r.Mount("/api/v1", apiv1.Routes())
//
// It returns the call that builds the router and the path it is attached at. The
// path must be a literal; a router mounted under a variable is mounted under
// something this cannot read.
func mountedRouterCall(call *ast.CallExpr) (builder *ast.CallExpr, recv, path string, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != "Mount" || len(call.Args) != 2 || !isStaticStringArg(call.Args[0]) {
		return nil, "", "", false
	}
	builder, isCall := call.Args[1].(*ast.CallExpr)
	if !isCall {
		return nil, "", "", false
	}
	return builder, identName(sel.X), extractStringArg(call, 0), true
}

// analyzeRouterFunc walks rf's body in source order, tracking the prefix of every
// router variable (its router parameter seeded to `seed`, each local root router
// to "", and each subrouter composed from its parent), and returns the edges to
// other registration functions it calls with a router argument. This is the
// interprocedural counterpart of the intra-function walk in extractRoutesFromStmt,
// and it reuses the same applySubrouterAssign composition so the two never drift.
func analyzeRouterFunc(rf routerFunc, seed string, mounted bool, index map[string]routerFunc, modulePath string, fieldTypes map[string]string) []routerEdge {
	prefixes := map[string]string{}
	if rf.hasParam && !mounted {
		prefixes[rf.paramName] = seed
	}
	// Build the same resolution context the call-graph extractor uses, so a
	// registration call reached through a variable or field (h.RegisterRoutes,
	// app.SettingsHandler.RegisterRoutes) resolves to the method's funcKey.
	ctx := resolveCtx{
		pkgDir:     rf.pkgDir,
		modulePath: modulePath,
		imports:    rf.imports,
		recvVar:    rf.recvVar,
		recvType:   rf.recvType,
		fieldTypes: fieldTypes,
	}
	ctx.localTypes = collectLocalTypes(rf.fn.Body, ctx)
	var edges []routerEdge

	// A mount's function literal is walked on its own, with the prefix it mounts
	// under in force, and skipped when the outer walk reaches it.
	scoped := map[*ast.FuncLit]bool{}
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncLit:
			if scoped[node] {
				return false
			}
		case *ast.AssignStmt:
			if v := newRouterLocalVar(node, rf.aliases); v != "" {
				prefixes[v] = ""
				// The router this function builds is the one its caller mounted.
				if mounted && v == rf.rootVar {
					prefixes[v] = seed
				}
			}
			applySubrouterAssign(node, prefixes)
		case *ast.CallExpr:
			if builder, recv, path, ok := mountedRouterCall(node); ok {
				if key := resolveCalleeKey(builder, ctx); key != "" && index[key].rootVar != "" {
					prefix := strings.TrimSuffix(joinRoutePath(prefixes[recv], path), "/")
					edges = append(edges, routerEdge{callee: key, prefix: prefix, mounted: true})
				}
			}
			if lit, scope := funcLitMount(node, prefixes); lit != nil {
				scoped[lit] = true
				restore := scope.apply(prefixes)
				ast.Inspect(lit.Body, visit)
				restore()
			}
			key := resolveCalleeKey(node, ctx)
			if key == "" {
				return true
			}
			callee, ok := index[key]
			if !ok || !callee.hasParam || callee.paramIndex >= len(node.Args) {
				return true
			}
			if prefix, ok := routerArgPrefix(node.Args[callee.paramIndex], prefixes); ok {
				edges = append(edges, routerEdge{callee: key, prefix: prefix})
			}
		}
		return true
	}
	ast.Inspect(rf.fn.Body, visit)
	return edges
}

// buildRoutePrefixIndex runs the module-wide fixpoint: from every root-router
// function it propagates the prefix each router argument carries to the callee's
// router parameter, accumulating the distinct prefix set per registration
// function. The result seeds extractRoutes. Deterministic: roots and edges are
// processed in sorted order and the emitted seed sets are sorted.
func buildRoutePrefixIndex(pkgs map[string]*parsedPkg, modulePath string, pkgNames map[string]string, fieldTypes map[string]string, wrappers map[string]string) routePrefixIndex {
	index := buildRouterFuncIndex(pkgs, modulePath, pkgNames, wrappers)

	type work struct {
		key, seed string
		mounted   bool
	}
	seeds := map[string]map[string]bool{}  // funcKey → prefixes its router parameter carries
	mounts := map[string]map[string]bool{} // funcKey → prefixes the router it builds is mounted under
	seen := map[work]bool{}
	var queue []work
	enqueue := func(w work) {
		if !seen[w] {
			seen[w] = true
			queue = append(queue, w)
		}
	}

	// A function whose router some other function mounts is not a root: its
	// routes are served under the mount, and analysing it at "" as well would
	// hand every function it calls a second, bare prefix. Found before the
	// fixpoint, by name alone, because the fixpoint needs it to pick its roots.
	isMounted := map[string]bool{}
	for _, rf := range index {
		ctx := resolveCtx{pkgDir: rf.pkgDir, modulePath: modulePath, imports: rf.imports, recvVar: rf.recvVar, recvType: rf.recvType, fieldTypes: fieldTypes}
		ast.Inspect(rf.fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if builder, _, _, ok := mountedRouterCall(call); ok {
					if key := resolveCalleeKey(builder, ctx); key != "" && index[key].rootVar != "" {
						isMounted[key] = true
					}
				}
			}
			return true
		})
	}

	rootKeys := make([]string, 0)
	for k, rf := range index {
		if rf.hasRoot && !isMounted[k] {
			rootKeys = append(rootKeys, k)
		}
	}
	sort.Strings(rootKeys)
	for _, k := range rootKeys {
		enqueue(work{key: k})
	}

	for len(queue) > 0 {
		w := queue[0]
		queue = queue[1:]
		rf, ok := index[w.key]
		if !ok {
			continue
		}
		edges := analyzeRouterFunc(rf, w.seed, w.mounted, index, modulePath, fieldTypes)
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].callee != edges[j].callee {
				return edges[i].callee < edges[j].callee
			}
			if edges[i].prefix != edges[j].prefix {
				return edges[i].prefix < edges[j].prefix
			}
			return !edges[i].mounted && edges[j].mounted
		})
		for _, e := range edges {
			into := seeds
			if e.mounted {
				into = mounts
			}
			if into[e.callee] == nil {
				into[e.callee] = map[string]bool{}
			}
			if !into[e.callee][e.prefix] {
				into[e.callee][e.prefix] = true
				enqueue(work{key: e.callee, seed: e.prefix, mounted: e.mounted})
			}
		}
	}

	sorted := func(set map[string]bool) []string {
		ps := make([]string, 0, len(set))
		for p := range set {
			ps = append(ps, p)
		}
		sort.Strings(ps)
		return ps
	}
	out := routePrefixIndex{}
	for key, set := range seeds {
		rf, ok := index[key]
		if !ok || !rf.hasParam {
			continue
		}
		out[key] = &routePrefixEntry{paramName: rf.paramName, seeds: sorted(set)}
	}
	// A function that builds the router it returns carries the mount on that
	// router's variable. One that also takes a router keeps the entry above: it
	// has one seed slot, and the router it was handed is the commoner case.
	for key, set := range mounts {
		rf, ok := index[key]
		if !ok || rf.rootVar == "" || out[key] != nil {
			continue
		}
		out[key] = &routePrefixEntry{paramName: rf.rootVar, seeds: sorted(set)}
	}
	return out
}
