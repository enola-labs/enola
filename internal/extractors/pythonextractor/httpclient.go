package pythonextractor

import (
	"bytes"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Python outbound HTTP calls: requests, httpx and aiohttp.
//
// Until this pass the Python extractor emitted no client routes, so a Python service
// never appeared as the caller of another repository: every cross-repo edge out of
// one was missing, and so was every caller a Python client could name.
//
// A call is read only where its receiver is known to be one of these libraries:
//   - a module function, `requests.get(url)`, `httpx.post(url)`, through the file's
//     imports, so `import httpx as h; h.get(…)` counts and a local named `requests`
//     does not;
//   - a client instance, a name the file binds to `requests.Session()`,
//     `httpx.Client()`, `httpx.AsyncClient()` or `aiohttp.ClientSession()`, by
//     assignment or by `with … as name`.
//
// The URL must reduce to a path: a string literal, an f-string whose leading
// interpolation (the base URL) is dropped and whose others become `{}`, or a `+`
// concatenation read the same way. An absolute URL is split into host and path and
// marked external, as the other client passes do.

// pyHTTPLibraries are the modules whose calls are HTTP requests.
var pyHTTPLibraries = map[string]bool{"requests": true, "httpx": true, "aiohttp": true}

// pyHTTPClientConstructors are the constructors returning a client instance.
var pyHTTPClientConstructors = map[string]bool{
	"requests.Session": true, "requests.session": true,
	"httpx.Client": true, "httpx.AsyncClient": true,
	"aiohttp.ClientSession": true,
}

// pyHTTPVerbs maps a client method to its HTTP verb.
var pyHTTPVerbs = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "patch": "PATCH",
	"delete": "DELETE", "head": "HEAD", "options": "OPTIONS",
}

// pyQualifiedName resolves a dotted expression through the file's imports of the
// HTTP libraries: `h.Client` with `import httpx as h` is "httpx.Client", `Session`
// with `from requests import Session` is "requests.Session". A name the file did
// not import from one of them resolves to "", so a local named `requests` is not
// the library.
func (w *pyWalker) pyQualifiedName(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	head, rest, dotted := strings.Cut(pyText(n, w.src), ".")
	t, ok := w.httpAliases[head]
	if !ok {
		return ""
	}
	if dotted {
		return t + "." + rest
	}
	return t
}

// collectHTTPImports binds the local names the file imports from the HTTP
// libraries. It reads the import statements itself: the walker's own import map is
// filled during the walk, after the client instances have to be known.
func (w *pyWalker) collectHTTPImports(root *sitter.Node) {
	bind := func(local, qualified string) {
		if lib, _, _ := strings.Cut(qualified, "."); pyHTTPLibraries[lib] {
			w.httpAliases[local] = qualified
		}
	}
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		switch kindOf(n) {
		case "import_statement":
			for i := range n.NamedChildCount() {
				c := n.NamedChild(i)
				switch kindOf(c) {
				case "dotted_name":
					name := pyText(c, w.src)
					head, _, _ := strings.Cut(name, ".")
					bind(head, head)
				case "aliased_import":
					if name, alias := c.ChildByFieldName("name"), c.ChildByFieldName("alias"); name != nil && alias != nil {
						bind(pyText(alias, w.src), pyText(name, w.src))
					}
				}
			}
			return
		case "import_from_statement":
			mod := n.ChildByFieldName("module_name")
			if mod == nil {
				return
			}
			module := pyText(mod, w.src)
			for i := range n.NamedChildCount() {
				c := n.NamedChild(i)
				if c.StartByte() == mod.StartByte() {
					continue
				}
				switch kindOf(c) {
				case "dotted_name":
					bind(pyText(c, w.src), module+"."+pyText(c, w.src))
				case "aliased_import":
					if name, alias := c.ChildByFieldName("name"), c.ChildByFieldName("alias"); name != nil && alias != nil {
						bind(pyText(alias, w.src), module+"."+pyText(name, w.src))
					}
				}
			}
			return
		}
		for i := range n.NamedChildCount() {
			visit(n.NamedChild(i))
		}
	}
	visit(root)
}

// prepareHTTPClients readies the client pass for one file: it binds the client
// instances when the file mentions one of the libraries at all, which most do not.
func (w *pyWalker) prepareHTTPClients(root *sitter.Node) {
	w.httpClients = map[string]string{}
	w.httpAliases = map[string]string{}
	w.httpCallSites = map[uint]int{}
	for lib := range pyHTTPLibraries {
		if bytes.Contains(w.src, []byte(lib)) {
			w.collectHTTPImports(root)
			if len(w.httpAliases) > 0 {
				w.collectHTTPClients(root)
			}
			return
		}
	}
}

// collectHTTPClients records the names this file binds to a client instance, by
// assignment (`self.session = requests.Session()`) or `with … as name`.
func (w *pyWalker) collectHTTPClients(root *sitter.Node) {
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		switch kindOf(n) {
		case "assignment":
			left, right := n.ChildByFieldName("left"), n.ChildByFieldName("right")
			if left != nil && w.isHTTPClientConstructor(right) {
				w.httpClients[pyText(left, w.src)] = libraryOf(w.pyQualifiedName(right.ChildByFieldName("function")))
			}
		case "as_pattern":
			// `with httpx.Client() as client` / `async with aiohttp.ClientSession() as s`
			var value, alias *sitter.Node
			for i := range n.NamedChildCount() {
				c := n.NamedChild(i)
				if kindOf(c) == "as_pattern_target" {
					alias = c
				} else if value == nil {
					value = c
				}
			}
			if alias != nil && w.isHTTPClientConstructor(value) {
				w.httpClients[pyText(alias, w.src)] = libraryOf(w.pyQualifiedName(value.ChildByFieldName("function")))
			}
		}
		for i := range n.NamedChildCount() {
			visit(n.NamedChild(i))
		}
	}
	visit(root)
}

func (w *pyWalker) isHTTPClientConstructor(n *sitter.Node) bool {
	if n == nil || kindOf(n) != "call" {
		return false
	}
	fn := n.ChildByFieldName("function")
	return fn != nil && pyHTTPClientConstructors[w.pyQualifiedName(fn)]
}

func libraryOf(qualified string) string {
	lib, _, _ := strings.Cut(qualified, ".")
	return lib
}

// emitHTTPClientRoute emits a client route for a request call, with the enclosing
// function or method as its caller.
func (w *pyWalker) emitHTTPClientRoute(call *sitter.Node) {
	if facts.IsTestPath(w.relFile) {
		return
	}
	fn := call.ChildByFieldName("function")
	if fn == nil || kindOf(fn) != "attribute" {
		return
	}
	obj, attr := fn.ChildByFieldName("object"), fn.ChildByFieldName("attribute")
	if obj == nil || attr == nil {
		return
	}
	library := ""
	if q := w.pyQualifiedName(obj); pyHTTPLibraries[q] {
		library = q
	} else if lib, ok := w.httpClients[pyText(obj, w.src)]; ok {
		library = lib
	}
	if library == "" {
		return
	}

	args := call.ChildByFieldName("arguments")
	positional := positionalArgs(call)
	method := pyText(attr, w.src)
	verb, urlArg := pyHTTPVerbs[method], (*sitter.Node)(nil)
	switch {
	case verb != "":
		if len(positional) > 0 {
			urlArg = positional[0]
		}
	case method == "request":
		// request("GET", url) — the verb must be a literal.
		if len(positional) < 2 {
			return
		}
		verb = strings.ToUpper(strings.Trim(pyText(positional[0], w.src), `"'`))
		if !pyHTTPVerbValid(verb) {
			return
		}
		urlArg = positional[1]
	default:
		return
	}
	if urlArg == nil && args != nil {
		urlArg = keywordArg(call, w.src, "url")
	}
	path, host, ok := pyRequestPath(urlArg, w.src)
	if !ok {
		return
	}

	caller := ""
	if owner := w.currentOwner(); owner != nil && owner.Kind == facts.KindSymbol &&
		(owner.PropString("symbol_kind") == facts.SymbolFunc || owner.PropString("symbol_kind") == facts.SymbolMethod) {
		caller = owner.Name
	}
	// A method's body is walked again from its class, with the class as owner; the
	// call site is one route, named by the function that makes it.
	if i, seen := w.httpCallSites[call.StartByte()]; seen {
		if caller != "" && w.out[i].PropString(facts.PropCaller) == "" {
			w.out[i].SetProp(facts.PropCaller, caller)
		}
		return
	}

	props := map[string]any{
		facts.PropRole:      facts.RoleClient,
		facts.PropSource:    facts.RouteSourcePythonHTTPClient,
		facts.PropFramework: library,
		"method":            verb,
		"language":          "python",
	}
	if host != "" {
		props["external"] = true
		props["host"] = host
	}
	if caller != "" {
		props[facts.PropCaller] = caller
	}
	w.httpCallSites[call.StartByte()] = len(w.out)
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindRoute,
		Name:      path,
		File:      w.relFile,
		Line:      int(call.StartPosition().Row) + 1,
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
}

func pyHTTPVerbValid(v string) bool {
	for _, verb := range pyHTTPVerbs {
		if v == verb {
			return true
		}
	}
	return false
}

// pyRequestPath reduces a URL expression to a request path, and the host when the
// URL is absolute. A leading interpolated or concatenated operand is the base URL
// and is dropped when a literal "/…" follows it; any other interpolation becomes a
// `{}` segment. It fails for a URL with no literal path.
func pyRequestPath(n *sitter.Node, src []byte) (path, host string, ok bool) {
	if n == nil {
		return "", "", false
	}
	var parts []string // literal text, or "\x00" for a dynamic part
	var flatten func(n *sitter.Node) bool
	flatten = func(n *sitter.Node) bool {
		switch kindOf(n) {
		case "string":
			for i := range n.NamedChildCount() {
				c := n.NamedChild(i)
				switch kindOf(c) {
				case "string_content":
					parts = append(parts, pyText(c, src))
				case "interpolation":
					parts = append(parts, "\x00")
				}
			}
			return true
		case "concatenated_string":
			for i := range n.NamedChildCount() {
				if !flatten(n.NamedChild(i)) {
					return false
				}
			}
			return true
		case "binary_operator":
			if op := n.ChildByFieldName("operator"); op == nil || pyText(op, src) != "+" {
				return false
			}
			return flatten(n.ChildByFieldName("left")) && flatten(n.ChildByFieldName("right"))
		case "parenthesized_expression":
			for i := range n.NamedChildCount() {
				if !flatten(n.NamedChild(i)) {
					return false
				}
			}
			return true
		default:
			parts = append(parts, "\x00")
			return true
		}
	}
	if !flatten(n) {
		return "", "", false
	}
	// A leading dynamic part followed by a rooted literal is the base URL.
	if len(parts) > 1 && parts[0] == "\x00" && strings.HasPrefix(parts[1], "/") {
		parts = parts[1:]
	}
	var b strings.Builder
	for _, p := range parts {
		if p == "\x00" {
			b.WriteString("{}")
		} else {
			b.WriteString(p)
		}
	}
	raw := b.String()
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	for _, scheme := range []string{"https://", "http://"} {
		if rest, found := strings.CutPrefix(raw, scheme); found {
			h, p, _ := strings.Cut(rest, "/")
			if h == "" || strings.Contains(h, "{}") {
				return "", "", false
			}
			host, raw = h, "/"+p
		}
	}
	if !strings.HasPrefix(raw, "/") || strings.Trim(raw, "/{}") == "" {
		return "", "", false
	}
	return raw, host, true
}
