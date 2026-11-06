package dotnetextractor

// Outbound HTTP — what a .NET service CALLS.
//
// Until this existed a C# service linked to its neighbours only as a route
// PROVIDER: `role=client` routes were never emitted, so the cross-repo linker had
// one side of every edge and the benchmark's cross-repo coverage axis had no .NET
// answer at all.
//
// The hard part is that the path is rarely a literal at the call site. eShop's
// CatalogService is representative:
//
//	private readonly string remoteServiceBaseUrl = "api/catalog/";
//	var uri = $"{remoteServiceBaseUrl}items/{id}?api-version=2.0";
//	return httpClient.GetFromJsonAsync(uri, …);
//
// The verb is on the call, the path is in a local, and the local is an
// interpolation over a field. So a file-local literal environment is built first
// and the argument resolved against it — the same shape the TypeScript extractor
// uses for base-URL fields.

import (
	"regexp"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// httpVerbs maps an HttpClient method name onto the verb it issues. Only methods
// whose verb is unambiguous are here: `SendAsync` takes an HttpRequestMessage
// whose verb is set elsewhere, so it cannot be classified from the call site.
var httpVerbs = map[string]string{
	"GetAsync": "GET", "GetStringAsync": "GET", "GetStreamAsync": "GET",
	"GetByteArrayAsync": "GET", "GetFromJsonAsync": "GET", "GetFromJsonAsAsyncEnumerable": "GET",
	"PostAsync": "POST", "PostAsJsonAsync": "POST", "PostAsJsonAsyncEnumerable": "POST",
	"PutAsync": "PUT", "PutAsJsonAsync": "PUT",
	"PatchAsync": "PATCH", "PatchAsJsonAsync": "PATCH",
	"DeleteAsync": "DELETE", "DeleteFromJsonAsync": "DELETE",
}

// refitVerbs are the Refit attributes that declare an interface method's request.
var refitVerbs = map[string]string{
	"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH",
	"Delete": "DELETE", "Head": "HEAD", "Options": "OPTIONS",
}

// clientCall is one resolved outbound request.
type clientCall struct {
	Method string
	Path   string
	File   string
	Dir    string
	Line   int
}

type httpClientScan struct {
	src     []byte
	relFile string
	dir     string
	// literals maps an identifier to the literal string it holds, for the file.
	// Deliberately file-scoped and flow-insensitive: a base URL is declared once
	// near the top of the class that uses it, and tracking assignments properly
	// would need a whole-program pass to buy the same answer.
	literals map[string]string
	calls    []clientCall
}

func collectHTTPClientCalls(root *sitter.Node, src []byte, relFile, dir string) []clientCall {
	s := &httpClientScan{src: src, relFile: relFile, dir: dir, literals: map[string]string{}}
	// Two passes: every literal binding in the file first, so a call reached before
	// its base-URL field is declared still resolves.
	s.collectLiterals(root)
	s.walk(root, nil)
	return s.calls
}

// memberBodies are the constructs that own a local scope. Locals are resolved
// within one of these before falling back to the type's fields, because a class
// routinely reuses the name `uri` in every method — a single file-wide map made
// all of eShop's CatalogService calls resolve to whichever `uri` was written last.
var memberBodies = map[string]bool{
	"method_declaration": true, "constructor_declaration": true,
	"accessor_declaration": true, "local_function_statement": true,
	"lambda_expression": true, "operator_declaration": true,
}

// collectLiterals records `x = "literal"` for TYPE-level fields and properties.
// Locals are collected per member body instead; see walk.
func (s *httpClientScan) collectLiterals(node *sitter.Node) {
	if memberBodies[kindOf(node)] {
		return // locals belong to the member, not the file
	}
	switch kindOf(node) {
	case "variable_declarator":
		// No "value" field in this grammar: the initializer is simply the last named
		// child after the name.
		if name := node.ChildByFieldName("name"); name != nil && node.NamedChildCount() > 1 {
			if lit, ok := s.literalOf(node.NamedChild(node.NamedChildCount() - 1)); ok {
				s.literals[nodeText(name, s.src)] = lit
			}
		}
	case "property_declaration":
		if name := node.ChildByFieldName("name"); name != nil {
			for i := uint(0); i < node.NamedChildCount(); i++ {
				c := node.NamedChild(i)
				if kindOf(c) == "arrow_expression_clause" {
					if lit, ok := s.literalOf(firstNamedChild(c)); ok {
						s.literals[nodeText(name, s.src)] = lit
					}
				}
			}
		}
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		s.collectLiterals(node.NamedChild(i))
	}
}

// interpolationHole matches `{expr}` inside an interpolated string.
var interpolationHole = regexp.MustCompile(`\{([^{}]*)\}`)

// literalOf resolves a node to a path string when it can be known statically.
//
// An interpolation hole becomes `{name}` — a path PARAMETER — rather than being
// dropped, which is what makes `$"{baseUrl}items/{id}"` come out as
// `api/catalog/items/{id}` and match the server's `items/{id}` template. A hole
// naming a known literal is substituted instead.
func (s *httpClientScan) resolve(node *sitter.Node, locals map[string]string) (string, bool) {
	if node == nil {
		return "", false
	}
	switch kindOf(node) {
	case "string_literal", "verbatim_string_literal", "raw_string_literal":
		return stringLiteralText(node, s.src)
	case "identifier":
		name := nodeText(node, s.src)
		if v, ok := locals[name]; ok {
			return v, true
		}
		v, ok := s.literals[name]
		return v, ok
	case "interpolated_string_expression":
		return s.interpolated(node, locals)
	case "binary_expression":
		// `baseUrl + "items"` — string concatenation of two resolvable parts.
		l, lok := s.resolve(node.ChildByFieldName("left"), locals)
		r, rok := s.resolve(node.ChildByFieldName("right"), locals)
		if lok && rok && operatorText(node, s.src) == "+" {
			return l + r, true
		}
	}
	return "", false
}

// literalOf resolves with type-level bindings only, for the field pass.
func (s *httpClientScan) literalOf(node *sitter.Node) (string, bool) {
	return s.resolve(node, nil)
}

func (s *httpClientScan) interpolated(node *sitter.Node, locals map[string]string) (string, bool) {
	raw := nodeText(node, s.src)
	// Strip the `$"` / `$@"` prefix and trailing quote.
	raw = strings.TrimPrefix(raw, "$")
	raw = strings.TrimPrefix(raw, "@")
	raw = strings.TrimPrefix(raw, `"`)
	raw = strings.TrimSuffix(raw, `"`)

	out := interpolationHole.ReplaceAllStringFunc(raw, func(m string) string {
		inner := strings.TrimSpace(m[1 : len(m)-1])
		// A format specifier or alignment is not part of the name.
		if i := strings.IndexAny(inner, ":,"); i >= 0 {
			inner = strings.TrimSpace(inner[:i])
		}
		if v, ok := locals[inner]; ok {
			return v
		}
		if v, ok := s.literals[inner]; ok {
			return v
		}
		// Anything else is a value substituted at run time: a path parameter. Its
		// NAME is kept when it is a plain identifier so the shape matches a server
		// template; an expression collapses to a wildcard.
		if isIdentifierPath(inner) {
			if i := strings.LastIndex(inner, "."); i >= 0 {
				inner = inner[i+1:]
			}
			return "{" + inner + "}"
		}
		return "{}"
	})
	return out, out != ""
}

func (s *httpClientScan) walk(node *sitter.Node, locals map[string]string) {
	if memberBodies[kindOf(node)] {
		locals = s.localsOf(node)
	}
	if kindOf(node) == "invocation_expression" {
		s.registration(node, locals)
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		s.walk(node.NamedChild(i), locals)
	}
}

// localsOf collects the literal-valued local bindings of one member body.
func (s *httpClientScan) localsOf(body *sitter.Node) map[string]string {
	out := map[string]string{}
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		if kindOf(n) == "variable_declarator" && n.NamedChildCount() > 1 {
			if name := n.ChildByFieldName("name"); name != nil {
				if lit, ok := s.resolve(n.NamedChild(n.NamedChildCount()-1), out); ok {
					out[nodeText(name, s.src)] = lit
				}
			}
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			visit(n.NamedChild(i))
		}
	}
	visit(body)
	return out
}

func (s *httpClientScan) registration(node *sitter.Node, locals map[string]string) {
	fn := node.ChildByFieldName("function")
	if fn == nil || kindOf(fn) != "member_access_expression" {
		return
	}
	nameNode := fn.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	verb, ok := httpVerbs[nodeText(nameNode, s.src)]
	if !ok {
		return
	}
	args := node.ChildByFieldName("arguments")
	if args == nil || args.NamedChildCount() == 0 {
		return
	}
	first := args.NamedChild(0)
	if kindOf(first) == "argument" {
		first = firstNamedChild(first)
	}
	raw, ok := s.resolve(first, locals)
	if !ok {
		return
	}
	if p, ok := normalizeClientPath(raw); ok {
		s.calls = append(s.calls, clientCall{
			Method: verb, Path: p, File: s.relFile, Dir: s.dir,
			Line: int(node.StartPosition().Row) + 1,
		})
	}
}

// normalizeClientPath turns a resolved argument into a comparable path, or
// rejects it.
//
// An absolute URL keeps only its path — the host is deployment configuration, and
// under .NET Aspire it is a service NAME (`https+http://catalog-api`) rather than
// a hostname at all. A query string is dropped: it is not part of the route
// template a server declares.
func normalizeClientPath(raw string) (string, bool) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", false
	}
	if i := strings.Index(p, "://"); i >= 0 {
		rest := p[i+3:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return "", false // a bare host with no path says nothing about an endpoint
		}
		p = rest[slash:]
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		return "", false
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", false
	}
	// At least one LITERAL segment is required. A path of only parameters (`/{x}`)
	// would match every server template at that depth, so it names no endpoint.
	literal := false
	for _, seg := range strings.Split(p, "/") {
		if seg != "" && !strings.HasPrefix(seg, "{") {
			literal = true
			break
		}
	}
	if !literal {
		return "", false
	}
	return p, true
}

// clientRouteFacts turns resolved calls into role=client route facts.
func clientRouteFacts(calls []clientCall) []facts.Fact {
	seen := map[string]bool{}
	out := make([]facts.Fact, 0, len(calls))
	for _, c := range calls {
		key := c.Method + " " + c.Path + " " + c.Dir
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, facts.Fact{
			Kind: facts.KindRoute,
			Name: c.Path,
			File: c.File,
			Line: c.Line,
			Props: map[string]any{
				"method":    c.Method,
				"role":      facts.RoleClient,
				"language":  "csharp",
				"framework": "httpclient",
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: c.Dir}},
		})
	}
	return out
}

// ── Refit ───────────────────────────────────────────────────────────────────

// noteRefit records a Refit interface method: `[Get("/api/orders/{id}")]` on an
// interface method declares an outbound request the same way an attribute
// declares an inbound one.
func (w *astWalker) noteRefit(node *sitter.Node, line int) {
	attrs := findChildByKind(node, "attribute_list")
	if attrs == nil {
		return
	}
	for i := uint(0); i < attrs.NamedChildCount(); i++ {
		a := attrs.NamedChild(i)
		if kindOf(a) != "attribute" {
			continue
		}
		name := nodeText(a.ChildByFieldName("name"), w.src)
		verb, ok := refitVerbs[strings.TrimSuffix(name, "Attribute")]
		if !ok {
			continue
		}
		args := findChildByKind(a, "attribute_argument_list")
		if args == nil || args.NamedChildCount() == 0 {
			continue
		}
		lit, ok := stringLiteralText(firstNamedChild(args.NamedChild(0)), w.src)
		if !ok {
			continue
		}
		if p, ok := normalizeClientPath(lit); ok {
			w.scaffold.clients = append(w.scaffold.clients, clientCall{
				Method: verb, Path: p, File: w.relFile, Dir: w.dir, Line: line,
			})
		}
	}
}
