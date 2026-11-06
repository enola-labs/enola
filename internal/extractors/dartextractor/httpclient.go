package dartextractor

import (
	"net/url"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Outbound HTTP call sites.
//
// This is the pass that makes a Flutter app a participant in the cross-repo graph
// rather than an isolated island. A mobile client's architectural value is almost
// entirely in what it CALLS: it serves nothing, imports nothing from its backend, and
// shares no code with it, so an HTTP call site is the only structural evidence that the
// app and the service belong to the same system.

// httpVerbs are the client methods `http`, `dio` and `chopper` all spell the same way.
var httpVerbs = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "patch": "PATCH",
	"delete": "DELETE", "head": "HEAD", "read": "GET", "readBytes": "GET",
}

// retrofitVerbs are the annotation names retrofit/chopper put on an abstract method.
var retrofitVerbs = map[string]string{
	"GET": "GET", "POST": "POST", "PUT": "PUT", "PATCH": "PATCH",
	"DELETE": "DELETE", "HEAD": "HEAD",
}

// extractHTTPClients emits a client-role route per outbound call site.
func (w *walker) extractHTTPClients(root *sitter.Node) {
	if w.importsAny("package:retrofit/", "package:chopper/") {
		w.retrofitRoutes(root)
	}
	if !w.importsAny("package:http/", "package:dio/", "package:http_client", "dart:io") {
		return
	}
	w.callSiteRoutes(root)
}

// callSiteRoutes reads `http.get(Uri.parse('...'))` and `dio.post('/path', …)`.
func (w *walker) callSiteRoutes(root *sitter.Node) {
	var visit func(*sitter.Node)
	visit = func(n *sitter.Node) {
		kids := namedChildren(n)
		for i, c := range kids {
			if kindOf(c) != "selector" || childOfKind(c, "argument_part") == nil {
				continue
			}
			name, receiver, _ := w.calleeOf(kids, i)
			method, ok := httpVerbs[name]
			if !ok {
				continue
			}
			// The receiver gate is what keeps `map.get(k)` and `list.remove(x)` out.
			// The import gate above already established that this file CAN do HTTP;
			// this establishes that this particular call is the HTTP one.
			if !looksLikeHTTPClient(receiver) {
				continue
			}
			args := argumentsOf(childOfKind(c, "argument_part"))
			raw := w.urlArgument(args)
			if raw == "" {
				continue
			}
			w.emitClientRoute(raw, method, c)
		}
		for _, c := range kids {
			visit(c)
		}
	}
	visit(root)
}

// urlArgument reads the URL out of the first argument, unwrapping `Uri.parse(...)`.
//
// package:http takes a Uri while dio takes a bare string, so both spellings have to be
// read; an interpolated URL yields "" from stringLiteralValue and is skipped rather
// than published with its `$id` intact.
func (w *walker) urlArgument(args *sitter.Node) string {
	first := positionalArg(args, 0)
	if first == nil {
		return ""
	}
	if s := stringLiteralValue(first, w.src); s != "" {
		return s
	}
	// Uri.parse('…') / Uri.https('host', '/path')
	txt := strings.TrimSpace(first.Utf8Text(w.src))
	if !strings.HasPrefix(txt, "Uri.") {
		return ""
	}
	inner := firstOfKind(first, "arguments")
	if inner == nil {
		return ""
	}
	if s := stringLiteralValue(positionalArg(inner, 0), w.src); s != "" {
		if strings.HasPrefix(txt, "Uri.parse") {
			return s
		}
		// Uri.https('api.example.com', '/v1/users') — host then path.
		if p := stringLiteralValue(positionalArg(inner, 1), w.src); p != "" {
			scheme := "https"
			if strings.HasPrefix(txt, "Uri.http(") {
				scheme = "http"
			}
			return scheme + "://" + s + p
		}
		return s
	}
	return ""
}

// httpClientTokens are receiver-name fragments that mark an HTTP client instance.
var httpClientTokens = []string{"http", "dio", "client", "api", "request", "rest", "gateway"}

func looksLikeHTTPClient(receiver string) bool {
	if receiver == "" {
		return false
	}
	lower := strings.ToLower(receiver)
	for _, t := range httpClientTokens {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}

// retrofitRoutes reads the annotation-declared client interfaces retrofit and chopper
// generate from.
//
// These are the cleanest client routes in the language: the path is a literal in an
// annotation on an abstract method, so there is no receiver to guess and no call site to
// resolve. The generated implementation is a `.g.dart` this extractor excludes, which
// is exactly why the annotation has to be read — the request would otherwise be
// invisible.
func (w *walker) retrofitRoutes(root *sitter.Node) {
	var visit func(*sitter.Node)
	visit = func(n *sitter.Node) {
		kids := namedChildren(n)
		for i, c := range kids {
			if kindOf(c) != "annotation" {
				continue
			}
			verb, ok := retrofitVerbs[annotationName(c, w.src)]
			if !ok {
				continue
			}
			path := stringLiteralValue(positionalArg(argumentsOf(c), 0), w.src)
			if path == "" {
				continue
			}
			// The annotated member follows the annotation, and its name is the best
			// available description of the call.
			handler := ""
			for j := i + 1; j < len(kids); j++ {
				if kindOf(kids[j]) == "annotation" {
					continue
				}
				if sig := firstOfKind(kids[j], "function_signature"); sig != nil {
					handler = signatureName(sig, w.src)
				}
				break
			}
			w.emitClientRoute(path, verb, c, handler)
		}
		for _, c := range kids {
			visit(c)
		}
	}
	visit(root)
}

// emitClientRoute appends one client-role route fact.
func (w *walker) emitClientRoute(raw, method string, n *sitter.Node, handler ...string) {
	path, host, external := splitClientURL(raw)
	if path == "" {
		return
	}
	props := map[string]any{
		"language":          "dart",
		facts.PropRole:      facts.RoleClient,
		facts.PropSource:    facts.RouteSourceDartHTTPClient,
		facts.PropFramework: "dart",
		"method":            method,
	}
	if external {
		// A hardcoded absolute URL is a third-party API, not an unresolved internal
		// edge. Tagging it is what stops it being counted as a coverage gap the way a
		// relative path legitimately is.
		props["external"] = true
		props["host"] = host
	}
	for _, h := range handler {
		if h != "" {
			props["handler"] = h
		}
	}
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindRoute, Name: path, File: w.relFile, Line: lineOf(n),
		Props: props,
	})
}

// splitClientURL separates an absolute URL into host and path, leaving a relative path
// alone. A relative path stays internal and therefore linkable to a backend in the same
// snapshot; an absolute one names somebody else's service.
func splitClientURL(raw string) (path, host string, external bool) {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		if raw == "" || !strings.HasPrefix(raw, "/") {
			// A bare segment with no leading slash is as likely to be a key or a
			// filename as a path. The TypeScript extractor requires the leading slash
			// for the same reason.
			return "", "", false
		}
		return stripQuery(raw), "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	return p, u.Host, true
}

// stripQuery removes a query string, which is call-site data rather than route identity
// — leaving it on would make `/users?page=1` and `/users?page=2` two different routes
// and neither would match the endpoint that serves both.
func stripQuery(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		return p[:i]
	}
	return p
}
