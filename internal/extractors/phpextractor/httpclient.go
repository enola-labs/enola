package phpextractor

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	php "github.com/tree-sitter/tree-sitter-php/bindings/go"
)

// clientVerbs maps an HTTP-client method name to the canonical verb it represents.
// These cover Guzzle ($client->get(...)), the Laravel Http facade (Http::get(...)),
// and any PSR-18-style wrapper exposing verb methods.
var clientVerbs = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "patch": "PATCH",
	"delete": "DELETE", "head": "HEAD", "options": "OPTIONS",
}

// clientReceiverNames are the lowercased variable / property base names that denote
// a hand-written HTTP client (Guzzle, Symfony HttpClient, PSR-18). A receiver chain
// rooted at the Laravel `Http` facade is recognized separately.
var clientReceiverNames = map[string]bool{
	"client": true, "http": true, "httpclient": true,
	"httpclientinterface": true, "guzzle": true, "guzzleclient": true,
}

// extractPHPHTTPClientFacts scans a PHP file for outbound HTTP-client calls and
// emits one client-route fact per call so the cross-repo linker can match it (by
// method + path suffix) to the service route that serves it. It recognizes Guzzle
// and PSR-18 verb / request() calls, the Laravel `Http` facade, Symfony's
// HttpClient, raw cURL (CURLOPT_URL), and file_get_contents. Absolute (http://…)
// URLs are skipped — they are third-party, not cross-repo backend paths.
func extractPHPHTTPClientFacts(src []byte, relFile string) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(php.LanguagePHP())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()

	c := &clientWalker{
		src:     src,
		relFile: relFile,
		dir:     factpath.Dir(relFile),
		api:     phpAPIHint(relFile),
		envHint: phpEnvHint(string(src)), // file-level provider fallback
	}
	c.walk(tree.RootNode())
	return c.out
}

type clientWalker struct {
	src     []byte
	relFile string
	dir     string
	api     string
	envHint string
	out     []facts.Fact
}

func (c *clientWalker) walk(node *sitter.Node) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "member_call_expression":
		c.handleMemberCall(node)
	case "scoped_call_expression":
		c.handleScopedCall(node)
	case "function_call_expression":
		c.handleFunctionCall(node)
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		c.walk(node.Child(i))
	}
}

// handleMemberCall recognizes Guzzle/PSR-18 instance calls: $client->get('/path'),
// $this->http->post('/path'), $httpClient->request('GET', '/path'), and Laravel
// chains such as Http::withToken($t)->get('/path').
func (c *clientWalker) handleMemberCall(node *sitter.Node) {
	method := node.ChildByFieldName("name")
	if method == nil || kindOf(method) != "name" {
		return
	}
	obj := node.ChildByFieldName("object")
	recv, facade := chainRootReceiver(obj, c.src)

	mname := phpText(method, c.src)
	args := node.ChildByFieldName("arguments")

	if mname == "request" {
		// request(VERB, URL): first positional arg is the verb, second is the path.
		verb := strings.ToUpper(positionalString(args, 0, c.src))
		if !isHTTPVerb(verb) {
			return
		}
		if !facade && !isClientReceiver(recv) {
			return
		}
		raw := positionalString(args, 1, c.src)
		c.emit(node, verb, raw, c.clientFramework(recv, facade, mname))
		return
	}

	verb, ok := clientVerbs[mname]
	if !ok {
		return
	}
	if !facade && !isClientReceiver(recv) {
		return
	}
	raw := positionalString(args, 0, c.src)
	c.emit(node, verb, raw, c.clientFramework(recv, facade, mname))
}

// handleScopedCall recognizes the Laravel Http facade: Http::get('/path'),
// Http::post('/path'), etc.
func (c *clientWalker) handleScopedCall(node *sitter.Node) {
	scope := node.ChildByFieldName("scope")
	method := node.ChildByFieldName("name")
	if scope == nil || method == nil || kindOf(scope) != "name" {
		return
	}
	if phpText(scope, c.src) != "Http" {
		return
	}
	verb, ok := clientVerbs[phpText(method, c.src)]
	if !ok {
		return
	}
	raw := positionalString(node.ChildByFieldName("arguments"), 0, c.src)
	c.emit(node, verb, raw, "laravel-http")
}

// handleFunctionCall recognizes URL-bearing builtins: curl_setopt($ch, CURLOPT_URL,
// 'url'), curl_init('url'), and file_get_contents('url').
func (c *clientWalker) handleFunctionCall(node *sitter.Node) {
	fn := node.ChildByFieldName("function")
	if fn == nil || kindOf(fn) != "name" {
		return
	}
	args := node.ChildByFieldName("arguments")
	switch phpText(fn, c.src) {
	case "curl_setopt":
		// curl_setopt($ch, CURLOPT_URL, 'url'): only the URL option carries a path.
		if positionalText(args, 1, c.src) != "CURLOPT_URL" {
			return
		}
		c.emit(node, "GET", positionalString(args, 2, c.src), "curl")
	case "curl_init":
		c.emit(node, "GET", positionalString(args, 0, c.src), "curl")
	case "file_get_contents":
		c.emit(node, "GET", positionalString(args, 0, c.src), "file-get-contents")
	}
}

// emit appends a client-route fact for a cleaned path, or does nothing when the
// literal is empty, dynamic, or an absolute third-party URL.
func (c *clientWalker) emit(node *sitter.Node, method, raw, framework string) {
	path, ok := cleanPHPPath(raw)
	if !ok {
		return
	}
	c.out = append(c.out, facts.Fact{
		Kind: facts.KindRoute,
		Name: path,
		File: c.relFile,
		Line: line(node),
		Props: map[string]any{
			facts.PropRole:   facts.RoleClient,
			"method":         method,
			"framework":      framework,
			"language":       "php",
			facts.PropSource: facts.RouteSourcePHPHTTPClient,
			"api":            c.api,
			"target_hint":    c.envHint,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: c.dir}},
	})
	if c.envHint != "" {
		c.out[len(c.out)-1].SetProp(facts.PropTargetHintFrom, facts.HintFromEnv)
	}
}

// clientFramework classifies the client library from the receiver and method. The
// Laravel facade is identified by the chain root; request() on a *httpClient*
// receiver is Symfony's HttpClient, request() elsewhere is treated as Guzzle, and a
// verb method (get/post/…) on a plain client variable is Guzzle/PSR-18.
func (c *clientWalker) clientFramework(recv string, facade bool, method string) string {
	if facade {
		return "laravel-http"
	}
	base := receiverBase(recv)
	if method == "request" {
		if strings.Contains(base, "httpclient") {
			return "symfony-httpclient"
		}
		return "guzzle"
	}
	return "guzzle"
}

// chainRootReceiver walks the object/scope chain of a member-call receiver down to
// its root, returning the root receiver's source text and whether the chain is
// rooted at a static facade call (e.g. Http::withToken(...)->get(...)). For a static
// facade root it returns the facade class name (the scope) and facade=true.
func chainRootReceiver(obj *sitter.Node, src []byte) (recv string, facade bool) {
	for obj != nil {
		switch kindOf(obj) {
		case "member_call_expression", "nullsafe_member_call_expression":
			// Descend through a call chain ($http->withFoo()->get(...)).
			obj = obj.ChildByFieldName("object")
		case "scoped_call_expression":
			scope := obj.ChildByFieldName("scope")
			return phpText(scope, src), phpText(scope, src) == "Http"
		default:
			return phpText(obj, src), false
		}
	}
	return "", false
}

// isClientReceiver reports whether a receiver chain's root names a hand-written HTTP
// client: a known client variable ($client/$http/$httpClient/$guzzle) or property
// ($this->client, $this->http, …).
func isClientReceiver(recv string) bool {
	return clientReceiverNames[receiverBase(recv)]
}

// receiverBase reduces a receiver expression to its lowercased base identifier:
// "$httpClient" -> "httpclient", "$this->client" -> "client".
func receiverBase(recv string) string {
	recv = strings.TrimSpace(recv)
	if i := strings.LastIndex(recv, "->"); i >= 0 {
		recv = recv[i+2:]
	}
	recv = strings.TrimPrefix(recv, "$")
	return strings.ToLower(recv)
}

// isHTTPVerb reports whether verb is a known HTTP verb (used to validate the first
// argument of request()).
func isHTTPVerb(verb string) bool {
	for _, v := range clientVerbs {
		if v == verb {
			return true
		}
	}
	return false
}

// positionalString returns the literal content of the idx-th positional (unnamed)
// argument when it is a plain string, or "" otherwise.
func positionalString(args *sitter.Node, idx int, src []byte) string {
	v := positionalArg(args, idx)
	if v == nil {
		return ""
	}
	return stringLiteral(v, src)
}

// positionalText returns the raw source text of the idx-th positional argument's
// value node (used to read constant names like CURLOPT_URL).
func positionalText(args *sitter.Node, idx int, src []byte) string {
	v := positionalArg(args, idx)
	if v == nil {
		return ""
	}
	return phpText(v, src)
}

// positionalArg returns the value expression node of the idx-th positional (unnamed)
// argument in an arguments node, or nil.
func positionalArg(args *sitter.Node, idx int) *sitter.Node {
	if args == nil {
		return nil
	}
	pos := 0
	for i := uint(0); i < args.ChildCount(); i++ {
		a := args.Child(i)
		if kindOf(a) != "argument" {
			continue
		}
		if a.ChildByFieldName("name") != nil {
			continue // named argument (name: value) — not positional
		}
		v := argValue(a)
		if v == nil {
			continue
		}
		if pos == idx {
			return v
		}
		pos++
	}
	return nil
}

// argValue returns the value expression of an argument node, skipping the optional
// name: label of a named argument.
func argValue(arg *sitter.Node) *sitter.Node {
	for i := uint(0); i < arg.ChildCount(); i++ {
		c := arg.Child(i)
		if !c.IsNamed() {
			continue
		}
		if arg.FieldNameForChild(uint32(i)) == "name" {
			continue
		}
		return c
	}
	return nil
}

// phpInterpolationLike collapses a residual placeholder run; PHP literal
// interpolation is already dropped upstream by stringLiteral (interpolated strings
// return ""), so this only normalizes leftover braces.
var phpInterpolationLike = regexp.MustCompile(`\{[^}]*\}`)

// cleanPHPPath turns a client call's URL literal into a matchable route path, or
// returns ok=false when it is not a cross-repo backend path (absolute/external,
// empty, or fully dynamic). It drops the query string and requires at least one
// concrete (non-placeholder) segment. Mirrors rubyextractor.cleanRubyPath.
func cleanPHPPath(raw string) (string, bool) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", false
	}
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return "", false // absolute URL → third-party, not a cross-repo backend path
	}
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	p = phpInterpolationLike.ReplaceAllString(p, "{}")
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg != "" && seg != "{}" {
			return p, true
		}
	}
	return "", false
}

// phpEnvHint returns a provider hint derived from the first base-URL env var read in
// s (getenv / env / $_ENV / $_SERVER), or "" if none.
var phpEnvVar = regexp.MustCompile(`(?:getenv\s*\(\s*|[\$]?env\s*\(\s*|\$_ENV\s*\[\s*|\$_SERVER\s*\[\s*)['"]([A-Za-z_][A-Za-z0-9_]*)['"]`)

func phpEnvHint(s string) string {
	if m := phpEnvVar.FindStringSubmatch(s); m != nil {
		return stripURLVarSuffix(m[1])
	}
	return ""
}

// urlVarSuffixes are env-var name suffixes stripped (longest first) to recover the
// target-service token, e.g. BILLING_BASE_URL -> "billing". Mirrors the same table
// in goextractor/rubyextractor (intentionally duplicated per the no-cross-import
// convention between sibling extractors).
var urlVarSuffixes = []string{
	"_HTTP_CLIENT_BASE_URL", "_CLIENT_BASE_URL", "_SERVICE_URL",
	"_BASE_URL", "_API_URL", "_URL", "_HOST", "_ENDPOINT",
}

// stripURLVarSuffix removes the longest matching base-URL suffix from an env-var
// name and lowercases the remainder (dropping underscores), yielding a provider hint.
func stripURLVarSuffix(name string) string {
	for _, suf := range urlVarSuffixes {
		if strings.HasSuffix(name, suf) && len(name) > len(suf) {
			name = name[:len(name)-len(suf)]
			break
		}
	}
	return strings.ReplaceAll(strings.ToLower(name), "_", "")
}

// phpAPIHint returns the source file's base name without extension, used as the
// cross-repo linker's disambiguation hint.
func phpAPIHint(relFile string) string {
	base := filepath.Base(relFile)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
