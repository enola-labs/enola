package javaextractor

import (
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// restTemplateVerbs maps a Spring RestTemplate method name to the HTTP verb it
// performs. exchange/execute carry the verb in an HttpMethod argument instead, so
// they map to "" and the verb is read from the call's arguments.
var restTemplateVerbs = map[string]string{
	"getForObject":    "GET",
	"getForEntity":    "GET",
	"postForObject":   "POST",
	"postForEntity":   "POST",
	"postForLocation": "POST",
	"put":             "PUT",
	"delete":          "DELETE",
	"patchForObject":  "PATCH",
	"headForHeaders":  "HEAD",
	"optionsForAllow": "OPTIONS",
	"exchange":        "",
	"execute":         "",
}

// detectRestTemplateCall emits a client-route fact for a Spring RestTemplate call
// site (restTemplate.getForEntity("/api/x", …), .exchange("/api/x", HttpMethod.POST, …)).
// It is called from handleInvocation for every method_invocation node.
//
// Precision (the appendingPathComponent lesson from Swift): a route is emitted only
// when an argument is a string literal that looks like a request path (starts with
// "/"), which excludes generic same-named calls (map.put("key", v)) and external
// absolute URLs. For exchange/execute the HTTP verb must be resolvable from an
// HttpMethod.X argument, else the call is skipped.
func (w *astWalker) detectRestTemplateCall(node *sitter.Node, name string) {
	method, ok := restTemplateVerbs[name]
	if !ok {
		return
	}
	args := node.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	path := firstPathLiteral(args, w.src)
	if path == "" {
		return
	}
	if method == "" { // exchange/execute — verb lives in an HttpMethod arg
		method = httpMethodArg(args, w.src)
		if method == "" {
			return
		}
	}
	cleaned := cleanJavaClientPath(path)
	if cleaned == "" || cleaned == "/" { // query-only literal ("/?x=1") → no linkable path
		return
	}
	props := map[string]any{
		facts.PropRole:   facts.RoleClient,
		"method":         method,
		"framework":      "resttemplate",
		"language":       "java",
		facts.PropSource: facts.RouteSourceJavaHTTPClient,
		"api":            javaAPIHint(w.relFile),
	}
	// The method whose body makes the call, lambdas included: the walker's owner.
	// A field initializer's owner is its class, which is no caller.
	if owner := w.currentOwner(); owner != nil && isCallerKind(owner.PropString("symbol_kind")) {
		props[facts.PropCaller] = owner.Name
	}
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindRoute,
		Name:      cleaned,
		File:      w.relFile,
		Line:      int(node.StartPosition().Row) + 1,
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
}

// isCallerKind reports whether a symbol of this kind has a body that makes calls.
// Constructors are emitted as methods.
func isCallerKind(kind string) bool {
	return kind == facts.SymbolMethod || kind == facts.SymbolFunc
}

// feignClientFacts emits a client-route fact for each HTTP method a @FeignClient
// interface method declares, combining the interface base path with the method
// mapping. Mirrors springRouteFacts but marks role=client and carries the Feign
// service name as the cross-repo target hint.
func feignClientFacts(basePath, hint string, methodAnns []javaAnnotation, relFile string, line int, dir, method string) []facts.Fact {
	var out []facts.Fact
	for _, a := range methodAnns {
		var methods []string
		var sub string
		if verb, ok := mappingMethods[a.name]; ok {
			methods = []string{verb}
			sub = mappingPath(&a)
		} else if a.name == "RequestMapping" {
			methods = requestMappingVerbs(&a)
			sub = mappingPath(&a)
		} else {
			continue
		}
		full := cleanJavaClientPath(facts.JoinRoutePath(basePath, sub))
		for _, m := range methods {
			props := map[string]any{
				facts.PropRole:   facts.RoleClient,
				"method":         m,
				"framework":      "feign",
				"language":       "java",
				facts.PropSource: facts.RouteSourceFeign,
				"api":            javaAPIHint(relFile),
			}
			if hint != "" {
				props["target_hint"] = hint
			}
			// A Feign route is declared by an interface method, which is its caller
			// as a Retrofit route's is: code calls the method, the method is the call.
			if method != "" {
				props[facts.PropCaller] = method
			}
			out = append(out, facts.Fact{
				Kind:      facts.KindRoute,
				Name:      full,
				File:      relFile,
				Line:      line,
				Props:     props,
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
			})
		}
	}
	return out
}

// feignServiceHint returns the service identity a @FeignClient targets, used as the
// cross-repo linker's disambiguation hint: its name=/value= attribute, else the host
// of its url= attribute.
func feignServiceHint(annotations []javaAnnotation) string {
	fc := findAnnotation(annotations, "FeignClient")
	if fc == nil {
		return ""
	}
	if v := fc.named["name"]; v != "" {
		return v
	}
	if v := fc.named["value"]; v != "" {
		return v
	}
	if len(fc.positional) > 0 {
		return fc.positional[0]
	}
	if u := fc.named["url"]; u != "" {
		return feignURLHost(u)
	}
	return ""
}

// feignURLHost extracts a service-ish token from a Feign url= attribute, e.g.
// "http://billing:8080" -> "billing". Returns "" for placeholder URLs ("${…}").
func feignURLHost(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexAny(u, ":/"); i >= 0 {
		u = u[:i]
	}
	if u == "" || strings.Contains(u, "$") {
		return ""
	}
	return u
}

// firstPathLiteral returns the first string literal in document order under args
// that looks like a request path (leading "/"), unquoted. This finds the path in
// `baseURL + "/api/x"` (a binary_expression) and ignores non-path string args and
// external absolute URLs (which start with "http", not "/").
func firstPathLiteral(args *sitter.Node, src []byte) string {
	var found string
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil || found != "" {
			return
		}
		if v, ok := stringLiteralValue(n, src); ok {
			// A request path, not a bare "/" separator from `base + "/" + id`:
			// require a leading slash and at least one real segment character.
			if strings.HasPrefix(v, "/") && strings.Trim(v, "/") != "" {
				found = v
			}
			return
		}
		for i := uint(0); i < uint(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		walk(args.Child(i))
		if found != "" {
			break
		}
	}
	return found
}

// httpMethodArg returns the HTTP verb named by an HttpMethod.X / RequestMethod.X
// argument under args (used by exchange/execute), or "" if none is present.
func httpMethodArg(args *sitter.Node, src []byte) string {
	var verb string
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil || verb != "" {
			return
		}
		t := nodeText(n, src)
		if strings.HasPrefix(t, "HttpMethod.") || strings.HasPrefix(t, "RequestMethod.") {
			if i := strings.LastIndexByte(t, '.'); i >= 0 {
				verb = strings.ToUpper(strings.TrimSpace(t[i+1:]))
				return
			}
		}
		for i := uint(0); i < uint(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		walk(args.Child(i))
		if verb != "" {
			break
		}
	}
	return verb
}

// cleanJavaClientPath strips a query string from a client path; path params like
// {id} are left for the cross-repo linker's normalizePath to collapse.
func cleanJavaClientPath(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p
}

// javaAPIHint returns the source file's base name without extension (e.g.
// "RestClient"), used as the cross-repo linker's disambiguation hint.
func javaAPIHint(relFile string) string {
	base := filepath.Base(relFile)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
