package swiftextractor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// --- route base classes ---
//
// A codebase's own request type, written once and used everywhere: a class whose
// initializer takes the path as `endpoint: String` and the verb as `method:` with a
// default. Each request is either a subclass calling `super.init(endpoint: "…")` or a
// direct construction `Base(endpoint: …, method: .post)`, often of a path wrapped by a
// normalizing helper (`Paths.normalize("/v1/items")`). The base is recognized by that
// initializer's shape, never by its name.

// swiftInitHead matches an initializer declaration head up to its parameter list.
var swiftInitHead = regexp.MustCompile(`\binit\s*(?:<[^>(]*>)?\s*\(`)

// swiftEndpointParam matches an `endpoint: String` initializer parameter.
var swiftEndpointParam = regexp.MustCompile(`\bendpoint\s*:\s*String\b`)

// swiftMethodParamDefault matches a `method:` parameter with an enum-literal default,
// capturing the verb.
var swiftMethodParamDefault = regexp.MustCompile(`\bmethod\s*:\s*[A-Za-z_][\w.]*\s*=\s*\.([A-Za-z]+)`)

// swiftClassHead matches a class declaration with an inheritance clause, capturing the
// name and the clause up to the body's brace.
var swiftClassHead = regexp.MustCompile(`\b(?:final\s+)?class\s+(\w+)\s*(?:<[^>{]*>)?\s*:\s*([^{]+)\{`)

// swiftEndpointArg, swiftGatewayPrefixArg: the `endpoint:` argument and a literal
// `gatewayEndpointPrefix:` argument of a call.
var (
	swiftEndpointArg      = regexp.MustCompile(`\bendpoint\s*:\s*`)
	swiftGatewayPrefixArg = regexp.MustCompile(`\bgatewayEndpointPrefix\s*:\s*"([^"]*)"`)
)

// swiftPathExpr is an endpoint value the pass resolves: a string literal, or a call
// whose only argument is one (a path-normalizing helper).
var swiftPathExpr = regexp.MustCompile(`^(?:"((?:[^"\\]|\\.)*)"|[A-Za-z_][\w.]*\(\s*"((?:[^"\\]|\\.)*)"\s*\))`)

// swiftLocalBinding matches `let|var <name> = <rest of line>`, capturing both.
var swiftLocalBinding = regexp.MustCompile(`(?m)^\s*(?:let|var)\s+(\w+)\s*(?::[^=\n]*)?=\s*(.+)$`)

// routeBase is a request base type and the verb its initializer defaults to.
type routeBase struct {
	name, defaultVerb string
}

// routeBases finds the request base types declared in one source.
func routeBases(text string) []routeBase {
	var out []routeBase
	for _, loc := range swiftTypeDecl.FindAllStringSubmatchIndex(text, -1) {
		name := text[loc[2]:loc[3]]
		rel := strings.IndexByte(text[loc[1]:], '{')
		if rel < 0 {
			continue
		}
		body, ok := braceBody(text, loc[1]+rel)
		if !ok {
			continue
		}
		for _, il := range swiftInitHead.FindAllStringIndex(body, -1) {
			params, ok := parenBody(body, il[1]-1)
			if !ok || !swiftEndpointParam.MatchString(params) {
				continue
			}
			m := swiftMethodParamDefault.FindStringSubmatch(params)
			if m == nil {
				continue
			}
			if verb := mapSwiftMethod(m[1]); verb != "" {
				out = append(out, routeBase{name: name, defaultVerb: verb})
				break
			}
		}
	}
	return out
}

// routeBaseCall is one request: the path it resolves to, its verb and where it is.
type routeBaseCall struct {
	path, verb string
	line       int
}

// routeBaseCalls reads the requests one source makes through the given bases: each
// `super.init(…endpoint:…)` in a class inheriting a base, and each `Base(…endpoint:…)`
// or `Base<T>(…)` construction. A call whose endpoint does not resolve to a literal is
// skipped.
func routeBaseCalls(text string, bases map[string]string) []routeBaseCall {
	var out []routeBaseCall
	add := func(base string, open int) {
		args, ok := parenBody(text, open)
		if !ok {
			return
		}
		path, ok := endpointArgPath(text, open, args)
		if !ok {
			return
		}
		verb := ""
		if m := topLevelSubmatch(swiftVerbArg, args); m != nil {
			verb = mapSwiftMethod(m[1])
		}
		if verb == "" {
			verb = bases[base]
		}
		if m := topLevelSubmatch(swiftGatewayPrefixArg, args); m != nil && strings.Trim(m[1], "/") != "" {
			path = joinURLPath(m[1], path)
		}
		out = append(out, routeBaseCall{path: path, verb: verb, line: 1 + strings.Count(text[:open], "\n")})
	}

	// Subclasses: the base is named in the inheritance clause, the request in super.init.
	for _, loc := range swiftClassHead.FindAllStringSubmatchIndex(text, -1) {
		base := inheritedBase(text[loc[4]:loc[5]], bases)
		if base == "" {
			continue
		}
		bodyStart := loc[1] - 1
		body, ok := braceBody(text, bodyStart)
		if !ok {
			continue
		}
		for _, sl := range swiftSuperInit.FindAllStringIndex(body, -1) {
			add(base, bodyStart+1+sl[1]-1)
		}
	}
	// Direct constructions.
	for _, loc := range swiftGenericConstructorCall.FindAllStringSubmatchIndex(text, -1) {
		name := text[loc[2]:loc[3]]
		if _, ok := bases[name]; ok {
			add(name, loc[1]-1)
		}
	}
	// Implicit member constructions, `.init(…)`, typed by the declaration they are in:
	// `func x() -> Base<T> { .init(…) }`, `static var x: Base<T> { .init(…) }`.
	for _, loc := range swiftImplicitInit.FindAllStringIndex(text, -1) {
		if base := enclosingDeclBase(text[:loc[0]], bases); base != "" {
			add(base, loc[1]-1)
		}
	}
	return out
}

// swiftImplicitInit matches an implicit member initializer call, `.init(`, not
// `super.init(` or `self.init(`.
var swiftImplicitInit = regexp.MustCompile(`(?:^|[^\w.])\.init\s*\(`)

// swiftDeclHead matches the start of a function or property declaration.
var swiftDeclHead = regexp.MustCompile(`\b(func|var|let)\s`)

// enclosingDeclBase returns the base type the declaration enclosing the end of s
// names as its type, "" when none does. Declarations are read backwards: a local
// binding without a base type is passed over, the enclosing function ends the search.
func enclosingDeclBase(s string, bases map[string]string) string {
	locs := swiftDeclHead.FindAllStringSubmatchIndex(s, -1)
	for i := len(locs) - 1; i >= 0; i-- {
		sig := s[locs[i][0]:]
		isFunc := s[locs[i][2]:locs[i][3]] == "func"
		end := strings.IndexByte(sig, '{')
		if !isFunc {
			if eq := strings.IndexByte(sig, '='); eq >= 0 && (end < 0 || eq < end) {
				end = eq
			}
		}
		if end >= 0 {
			sig = sig[:end]
		}
		for _, m := range swiftTypeRef.FindAllStringSubmatch(sig, -1) {
			if _, ok := bases[m[1]]; ok {
				return m[1]
			}
		}
		if isFunc {
			return ""
		}
	}
	return ""
}

// swiftTypeRef matches a type written after `->` or `:` in a declaration head.
var swiftTypeRef = regexp.MustCompile(`(?:->|:)\s*(?:\w+\.)*([A-Z]\w*)`)

// swiftSuperInit matches a superclass initializer call head.
var swiftSuperInit = regexp.MustCompile(`\bsuper\.init\s*\(`)

// swiftLeadingIdent matches the identifier an expression starts with.
var swiftLeadingIdent = regexp.MustCompile(`^\w+`)

// swiftGenericConstructorCall matches `Type(` or `Type<…>(`, capturing the type name.
var swiftGenericConstructorCall = regexp.MustCompile(`\b([A-Z]\w*)\s*(?:<[^>()]*>)?\s*\(`)

// inheritedBase returns the base named first in an inheritance clause, "" when the
// class inherits none of the bases.
func inheritedBase(clause string, bases map[string]string) string {
	first := strings.TrimSpace(strings.SplitN(clause, ",", 2)[0])
	if i := strings.IndexByte(first, '<'); i >= 0 {
		first = first[:i]
	}
	first = lastDotComponent(strings.TrimSpace(first))
	if _, ok := bases[first]; ok {
		return first
	}
	return ""
}

// endpointArgPath resolves a call's top-level `endpoint:` argument to a path: a
// literal, a single-literal helper call, or a local bound to either earlier in the
// source.
func endpointArgPath(text string, open int, args string) (string, bool) {
	var value string
	for _, m := range swiftEndpointArg.FindAllStringIndex(args, -1) {
		if depthAt(args[:m[0]]) == 0 {
			value = strings.TrimSpace(args[m[1]:])
			break
		}
	}
	if value == "" {
		return "", false
	}
	if p, ok := literalPath(value); ok {
		return p, true
	}
	ident := swiftLeadingIdent.FindString(value)
	if ident == "" {
		return "", false
	}
	// The nearest preceding binding of that name.
	var bound string
	for _, m := range swiftLocalBinding.FindAllStringSubmatchIndex(text[:open], -1) {
		if text[m[2]:m[3]] == ident {
			bound = text[m[4]:m[5]]
		}
	}
	if bound == "" {
		return "", false
	}
	return literalPath(strings.TrimSpace(bound))
}

// literalPath reads a path from the start of an expression: a string literal or a
// call whose only argument is one.
func literalPath(expr string) (string, bool) {
	m := swiftPathExpr.FindStringSubmatch(expr)
	if m == nil {
		return "", false
	}
	raw := m[1]
	if raw == "" {
		raw = m[2]
	}
	p := cleanSwiftPath(raw)
	if p == "" || hasNonAPIExtension(p) {
		return "", false
	}
	return p, true
}

// topLevelSubmatch is FindStringSubmatch restricted to matches at nesting depth 0.
func topLevelSubmatch(re *regexp.Regexp, args string) []string {
	for _, loc := range re.FindAllStringSubmatchIndex(args, -1) {
		if depthAt(args[:loc[0]]) != 0 {
			continue
		}
		out := make([]string, len(loc)/2)
		for i := range out {
			if loc[2*i] >= 0 {
				out[i] = args[loc[2*i]:loc[2*i+1]]
			}
		}
		return out
	}
	return nil
}

// extractRouteBaseFacts finds the repository's request base types, then emits one
// client route per request made through them. Repo-wide, because the base and its
// uses live in different modules.
func extractRouteBaseFacts(repoPath string, files []string, moduleForFile func(string) string) []facts.Fact {
	texts := map[string]string{}
	bases := map[string]string{}
	for _, relFile := range files {
		if !isSwiftFile(relFile) || isSwiftTestFile(relFile) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil || !strings.Contains(string(src), "endpoint") {
			continue
		}
		texts[relFile] = string(src)
		for _, b := range routeBases(string(src)) {
			if prev, ok := bases[b.name]; !ok || b.defaultVerb < prev {
				bases[b.name] = b.defaultVerb
			}
		}
	}
	if len(bases) == 0 {
		return nil
	}
	var out []facts.Fact
	for _, relFile := range files {
		text, ok := texts[relFile]
		if !ok {
			continue
		}
		seen := map[string]bool{}
		for _, c := range routeBaseCalls(text, bases) {
			key := c.verb + "\x00" + c.path
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, facts.Fact{
				Kind: facts.KindRoute,
				Name: c.path,
				File: relFile,
				Line: c.line,
				Props: map[string]any{
					facts.PropRole:   facts.RoleClient,
					"method":         c.verb,
					"framework":      "apiendpoint",
					"language":       "swift",
					facts.PropSource: facts.RouteSourceSwiftEndpoint,
					"api":            swiftAPIHint(relFile),
				},
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: moduleForFile(relFile)}},
			})
		}
	}
	return out
}
