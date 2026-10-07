package goextractor

import (
	"go/ast"
	"go/token"
	"net/url"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// goClientVerbs maps a wrapper-client / stdlib method name to the HTTP verb it
// implies. Used for both http.Get/Post(...) helpers and wrapper calls like
// svcClient.Post(...).
var goClientVerbs = map[string]string{
	"Get": "GET", "Head": "HEAD", "Post": "POST",
	"Put": "PUT", "Patch": "PATCH", "Delete": "DELETE",
}

// urlVarSuffixes are env-var name suffixes stripped (longest first) to recover
// the target-service token, e.g. CORE_HTTP_CLIENT_BASE_URL -> "core",
// XENDO_URL -> "xendo".
var urlVarSuffixes = []string{
	"_HTTP_CLIENT_BASE_URL", "_CLIENT_BASE_URL", "_SERVICE_URL",
	"_BASE_URL", "_API_URL", "_URL", "_HOST", "_ENDPOINT",
}

// extractHTTPClientFacts emits a client-route fact for every outbound net/http
// call (http.NewRequest / http.Get / a wrapper client's verb method) in a Go
// source file. These represent calls the service makes, so the cross-repo linker
// can match them (by method + path suffix) to the service route that serves
// them. It runs only when net/http is imported, and skips external absolute URLs
// and non-literal targets so server-handler code is never misread as a client.
func extractHTTPClientFacts(fset *token.FileSet, f *ast.File, relFile, pkgDir string, baseURLLits map[string][]string) []facts.Fact {
	if !importsNetHTTP(f) {
		return nil
	}
	api := goAPIHint(relFile)

	var out []facts.Fact
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		method, urlArg := classifyClientCall(sel, call)
		if urlArg == nil {
			return true
		}
		path, ok := cleanGoURL(clientURLPath(urlArg))
		if !ok {
			return true
		}
		props := map[string]any{
			facts.PropRole:   facts.RoleClient,
			"method":         method,
			"framework":      "net-http",
			"language":       "go",
			facts.PropSource: facts.RouteSourceGoHTTPClient,
			"api":            api,
			"target_hint":    envHintFromExpr(urlArg),
		}
		// extractStringExpr discards the host of a `baseURL + "/path"` concat and
		// keeps only the path. Recover it here so the linker can bucket a call to
		// a hardcoded host as external rather than mistaking it for an unresolved
		// internal edge (GAP-LK-02). This only sets props; the route Name — which
		// fact identity and server matching key on — is left untouched.
		if external, host := resolveClientHost(urlArg, baseURLLits); external {
			props["external"] = true
			if host != "" {
				props["host"] = host
			}
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindRoute,
			Name:      path,
			File:      relFile,
			Line:      fset.Position(call.Pos()).Line,
			Props:     props,
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: pkgDir}},
		})
		return true
	})
	return out
}

// clientURLPath reads the path out of a client call's URL argument.
//
// extractStringExpr answers for a server registration, where the path is one
// literal. A client builds its URL, and the usual build puts a value in the
// middle: `c.baseURL + "/api/v1/orders/" + id + "/status"`. Read as "the
// rightmost absolute literal" that is "/status", a path no server serves under
// that name, and `"/api/v1/orders/" + id` is "/api/v1/orders/", which matches
// the collection route instead of the item route.
//
// So the concatenation is read left to right. The path starts at the first
// literal beginning with "/", everything before it is the host, and from there
// a literal is kept and anything else is one path parameter, written "{}" as
// the TypeScript client extractor writes a template substitution. A segment
// that mixes text with a parameter ("/files/" + name + ".json") is still one
// parameter. A value appended to an unterminated segment ("/items" + suffix)
// is dropped, since it may be a query string or nothing at all.
//
// An expression with no such literal falls back to extractStringExpr, so every
// shape that resolved before resolves to the same path.
func clientURLPath(urlArg ast.Expr) string {
	var operands []ast.Expr
	var flatten func(e ast.Expr)
	flatten = func(e ast.Expr) {
		switch x := e.(type) {
		case *ast.ParenExpr:
			flatten(x.X)
			return
		case *ast.BinaryExpr:
			if x.Op == token.ADD {
				flatten(x.X)
				flatten(x.Y)
				return
			}
		}
		operands = append(operands, e)
	}
	flatten(urlArg)

	var sb strings.Builder
	started := false
	for _, op := range operands {
		lit, isLit := stringLitValue(op)
		if !started {
			if !isLit || !strings.HasPrefix(lit, "/") {
				continue
			}
			started = true
		}
		switch {
		case isLit:
			sb.WriteString(lit)
		case strings.HasSuffix(sb.String(), "/"):
			sb.WriteString("{}")
		}
	}
	if !started {
		return extractStringExpr(urlArg)
	}
	path, query, hasQuery := strings.Cut(sb.String(), "?")
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if strings.Contains(seg, "{}") {
			segs[i] = "{}"
		}
	}
	path = strings.Join(segs, "/")
	if hasQuery {
		return path + "?" + query
	}
	return path
}

// collectBaseURLLiterals maps an identifier name to every string literal bound to
// it anywhere in the package — const/var declarations, assignments (including the
// arms of a region switch), and composite-literal fields. It is used only to
// decide whether a `baseURL + "/path"` client call targets a hardcoded host; the
// bare-name keying is a heuristic, kept safe by resolveClientHost's requirement
// that EVERY binding be an absolute http(s) URL before tagging external.
func collectBaseURLLiterals(files []*ast.File) map[string][]string {
	out := map[string][]string{}
	add := func(name, val string) {
		if name == "" || name == "_" || val == "" {
			return
		}
		out[name] = append(out[name], val)
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.ValueSpec: // const NAME = "lit" / var NAME = "lit"
				for i, nm := range node.Names {
					if i < len(node.Values) {
						if s, ok := stringLitValue(node.Values[i]); ok {
							add(nm.Name, s)
						}
					}
				}
			case *ast.AssignStmt: // NAME = "lit" / NAME := "lit" / s.NAME = "lit"
				for i, lhs := range node.Lhs {
					if i < len(node.Rhs) {
						if s, ok := stringLitValue(node.Rhs[i]); ok {
							add(tailIdentName(lhs), s)
						}
					}
				}
			case *ast.KeyValueExpr: // composite literal field: NAME: "lit"
				if key, ok := node.Key.(*ast.Ident); ok {
					if s, ok := stringLitValue(node.Value); ok {
						add(key.Name, s)
					}
				}
			}
			return true
		})
	}
	return out
}

// resolveClientHost inspects the URL argument of a client call. When it is a
// `X + "/path"` concatenation whose base identifier X is bound only to absolute
// http(s) literals, it reports external=true and, when every binding agrees on a
// single host, that host. A base with any non-absolute or missing binding (e.g.
// a config-injected options.BaseURL) reports external=false, keeping the call an
// internal client route that the linker may still resolve or flag unresolved.
func resolveClientHost(urlArg ast.Expr, baseURLLits map[string][]string) (bool, string) {
	be, ok := urlArg.(*ast.BinaryExpr)
	if !ok || be.Op != token.ADD {
		return false, ""
	}
	if right := extractStringExpr(be.Y); !strings.HasPrefix(right, "/") {
		return false, ""
	}
	name := tailIdentName(be.X)
	vals := baseURLLits[name]
	if len(vals) == 0 {
		return false, ""
	}
	hosts := map[string]bool{}
	for _, v := range vals {
		if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return false, "" // a relative binding: cannot claim a hardcoded host
		}
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			hosts[u.Host] = true
		}
	}
	host := ""
	if len(hosts) == 1 {
		for h := range hosts {
			host = h
		}
	}
	return true, host
}

// tailIdentName returns the trailing identifier of a base-URL expression:
// "baseURL" for `baseURL`, `s.baseURL`, and `c.options.baseURL` alike, so a
// package const and a struct field carrying the same name resolve identically.
func tailIdentName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.ParenExpr:
		return tailIdentName(x.X)
	}
	return ""
}

// stringLitValue unquotes a string-literal expression (both "..." and `...`
// forms), or reports ok=false for anything else.
func stringLitValue(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// classifyClientCall returns the HTTP method and the URL argument expression for
// an outbound call, or (_, nil) when the call is not a recognized client call.
//
// Two shapes are recognized:
//
//	(a) package helpers on the `http` import: http.NewRequest(method, url, …),
//	    http.NewRequestWithContext(ctx, method, url, …), http.Get/Head(url),
//	    http.Post/PostForm(url, …).
//	(b) wrapper verb calls on any other receiver with a SINGLE argument:
//	    svcClient.Get(url) / .Post(url) / … — the single-arg gate keeps router
//	    registrations like r.Get("/x", handler) (≥2 args) from matching.
func classifyClientCall(sel *ast.SelectorExpr, call *ast.CallExpr) (string, ast.Expr) {
	name := sel.Sel.Name
	if identName(sel.X) == "http" {
		switch name {
		case "NewRequest":
			if len(call.Args) >= 2 {
				return goMethod(call.Args[0]), call.Args[1]
			}
		case "NewRequestWithContext":
			if len(call.Args) >= 3 {
				return goMethod(call.Args[1]), call.Args[2]
			}
		case "Get", "Head", "Post", "PostForm":
			if len(call.Args) >= 1 {
				verb := goClientVerbs[name]
				if name == "PostForm" {
					verb = "POST"
				}
				return verb, call.Args[0]
			}
		}
		return "", nil
	}
	// (b) wrapper client: single-arg verb method on a non-http receiver.
	if verb, ok := goClientVerbs[name]; ok && len(call.Args) == 1 {
		return verb, call.Args[0]
	}
	return "", nil
}

// goMethod resolves an HTTP method from a request constructor's method argument:
// a string literal ("POST") or an http.MethodX selector. Defaults to GET when
// unresolvable (the net/http default).
func goMethod(expr ast.Expr) string {
	if s := extractStringExpr(expr); s != "" {
		return strings.ToUpper(s)
	}
	if sel, ok := expr.(*ast.SelectorExpr); ok && identName(sel.X) == "http" {
		if v := strings.ToUpper(strings.TrimPrefix(sel.Sel.Name, "Method")); v != sel.Sel.Name {
			return v
		}
	}
	return "GET"
}

// cleanGoURL turns a resolved URL string into a matchable route path, or
// ok=false when it is external (absolute), empty, or not a path. Note that
// extractStringExpr already returns just the "/path" portion of a
// `baseURL + "/path"` concatenation, so base-URL stripping is free.
//
// A genuine route path contains a "/" separator; this requirement is the key
// guard against the verb-named stdlib methods that collide with a wrapper
// client's .Get/.Post — url.Values.Get("page"), http.Header.Get("Content-Type")
// — whose single string argument is a bare key, never a path.
func cleanGoURL(raw string) (string, bool) {
	p := strings.TrimSpace(raw)
	if p == "" || strings.HasPrefix(p, "http") {
		return "", false
	}
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimSpace(p)
	if p == "" || p == "/" || !strings.Contains(p, "/") {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg != "" {
			return p, true
		}
	}
	return "", false
}

// envHintFromExpr derives a provider hint from an os.Getenv("X")/os.LookupEnv("X")
// call appearing anywhere within the URL argument expression (e.g.
// os.Getenv("XENDO_URL") + "/path"), or "" when none is present.
func envHintFromExpr(expr ast.Expr) string {
	var hint string
	ast.Inspect(expr, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || identName(sel.X) != "os" {
			return true
		}
		if (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv") && len(call.Args) >= 1 {
			if name := extractStringExpr(call.Args[0]); name != "" {
				hint = stripURLVarSuffix(name)
				return false
			}
		}
		return true
	})
	return hint
}

// stripURLVarSuffix removes the longest matching base-URL suffix from an env-var
// name and lowercases the remainder (dropping underscores), yielding a provider
// hint. Returns the lowercased name unchanged if no suffix matches.
func stripURLVarSuffix(name string) string {
	for _, suf := range urlVarSuffixes {
		if strings.HasSuffix(name, suf) && len(name) > len(suf) {
			name = name[:len(name)-len(suf)]
			break
		}
	}
	return strings.ReplaceAll(strings.ToLower(name), "_", "")
}

// importsNetHTTP reports whether the file imports net/http.
func importsNetHTTP(f *ast.File) bool {
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) == "net/http" {
			return true
		}
	}
	return false
}

// goAPIHint returns the source file's base name without the .go extension, used
// as the cross-repo linker's disambiguation hint.
func goAPIHint(relFile string) string {
	base := relFile
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return strings.TrimSuffix(base, ".go")
}

// pathLiteralArg reads a call's arguments for a path-shaped string literal and
// the HTTP verb sitting beside it, which is how a repo talks to its own HTTP
// helper: `api.Request[Task]("/me/tasks", api.Options{Method: "PUT"})`.
//
// It answers about the CALL SITE only. Whether the callee actually reaches
// net/http is a question about the whole module and is settled by the seam
// binder — this pass sees one package, and in the case that motivated it the
// helper is two packages away. Recording a candidate here and deciding there is
// what keeps the two halves honest.
//
// The verb defaults to GET because that is what an HTTP client with no method
// set does, and the linker discriminates on it. A composite-literal field named
// Method carries the override in both the forms Go writes: a string literal, and
// the net/http constants.
func pathLiteralArg(call *ast.CallExpr) (path, verb string, ok bool) {
	verb = "GET"
	for _, arg := range call.Args {
		if lit, isLit := arg.(*ast.BasicLit); isLit && lit.Kind == token.STRING {
			if candidate, err := strconv.Unquote(lit.Value); err == nil && isPathShaped(candidate) && path == "" {
				path = candidate
			}
			continue
		}
		if composite, isComposite := arg.(*ast.CompositeLit); isComposite {
			if m := methodFieldOf(composite); m != "" {
				verb = m
			}
		}
	}
	return path, verb, path != ""
}

// isPathShaped keeps the candidate list to strings that could name an endpoint.
// A leading slash and no whitespace excludes prose and format strings; requiring
// a segment excludes "/" itself, which names nothing a client could be matched on.
func isPathShaped(s string) bool {
	if len(s) < 2 || s[0] != '/' || strings.ContainsAny(s, " \t\n\"") {
		return false
	}
	return strings.Trim(s, "/") != ""
}

// methodFieldOf reads a `Method:` field out of an options struct literal, in the
// two forms Go writes it: a string literal, and http.MethodPut.
func methodFieldOf(lit *ast.CompositeLit) string {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Method" {
			continue
		}
		switch v := kv.Value.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, err := strconv.Unquote(v.Value); err == nil {
					return strings.ToUpper(s)
				}
			}
		case *ast.SelectorExpr:
			// http.MethodPut and friends.
			if strings.HasPrefix(v.Sel.Name, "Method") {
				return strings.ToUpper(strings.TrimPrefix(v.Sel.Name, "Method"))
			}
		}
	}
	return ""
}

// isModuleLocalCallee reports whether a resolved call target names a function in
// this module rather than a dependency. Module-internal imports resolve to a
// relative path (`internal/api.Request`); a dependency keeps its domain
// (`github.com/gorilla/mux.Router.HandleFunc`), and a domain is the one thing a
// Go import path has that a relative one never does.
//
// Imprecise in one direction on purpose: a method on a local variable resolves
// to `publicAPI.GET` and passes this test. The seam binder rejects it, because a
// local variable is not a symbol that reaches net/http. Being wrong toward the
// binder is safe; being wrong toward the published fact set is not.
func isModuleLocalCallee(resolved string) bool {
	head := resolved
	if i := strings.IndexAny(head, "./"); i >= 0 {
		head = head[:i]
	}
	return !strings.Contains(head, ".") && head != ""
}
