package scalaextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Route trees written in Scala, as opposed to Play's separate routes DSL.
//
// Both frameworks below build a route from nested combinators rather than from one
// annotation, so neither path nor method is available at a single node — the walker
// has to carry them down the tree. That is the same shape the Go, Rust and C#
// extractors compose for subrouter mounts, and it is why these are a dedicated
// recursive pass rather than a case in the main walker.

// pekkoVerbs are the Pekko/Akka HTTP method directives. A route tree that names none
// of them serves every verb, which is what facts.MethodAny exists to say.
var pekkoVerbs = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE",
	"patch": "PATCH", "head": "HEAD", "options": "OPTIONS",
}

// pekkoPathDirectives consume path segments. `path` matches to the END of the path
// and therefore terminates a route; `pathPrefix` and `rawPathPrefix` match a prefix
// and keep descending. That difference is what tells a mount from an endpoint.
var pekkoPathDirectives = map[string]bool{
	"path": true, "pathPrefix": true, "rawPathPrefix": true, "pathEnd": true,
	"pathPrefixTest": true, "pathSuffix": true,
}

// extractPekkoRoutes walks a file for Pekko/Akka HTTP route trees.
//
// A route is emitted where a `path(...)` directive is found, at the path composed
// from every enclosing `pathPrefix`. The method comes from the nearest enclosing verb
// directive, in either spelling the DSL allows — nested (`path("x") { get { … } }`)
// or conjoined (`(path("x") & post) { … }`).
//
// A non-literal segment (`pathPrefix(collection.path)`, a PathMatcher value) cannot
// be resolved without evaluating Scala, so the route keeps the part that IS known and
// is tagged `path_unresolved`. Reporting the resolvable half is more useful than
// dropping the endpoint, and the tag stops a partial path being read as a full one.
func extractPekkoRoutes(root *sitter.Node, src []byte, relFile, dir string) []facts.Fact {
	if !importsPekkoHTTPServer(src) {
		return nil
	}
	w := &dslWalker{src: src, relFile: relFile, dir: dir}
	w.walkPekko(root, "", "", false)
	return w.out
}

// pekkoServerImports are the packages that bring the routing directives into scope.
// Their presence is what makes `path(...)` the directive rather than a method.
var pekkoServerImports = []string{
	"org.apache.pekko.http.scaladsl.server",
	"akka.http.scaladsl.server",
	"org.apache.pekko.http.javadsl.server",
	"akka.http.javadsl.server",
}

// importsPekkoHTTPServer gates route extraction on the file importing the routing
// DSL, and the gate is not caution — it is a measured correction.
//
// `path` is an ordinary method name. Matching the directive structurally, the way the
// Rust extractor matches Axum's `.route(...)` builder, produced four routes from a
// metrics helper whose API is literally `path(result).record(nanos)` — paths like
// `/:issuccess` attached to a file that serves no HTTP at all. Axum's builder call is
// distinctive enough to stand alone; `path(...)` is not, so the import is what
// separates the two readings.
func importsPekkoHTTPServer(src []byte) bool {
	if containsAny(src, pekkoServerImports) {
		return true
	}
	// A route file frequently mixes in `Directives` rather than importing the
	// package path directly.
	return containsAny(src, directivesMixins)
}

// directivesMixins are the `Directives` spellings the gate accepts besides an
// explicit package import.
var directivesMixins = []string{
	"http.scaladsl.server.Directives", "extends Directives", "with Directives",
}

type dslWalker struct {
	src     []byte
	relFile string
	dir     string
	out     []facts.Fact
}

func (w *dslWalker) text(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	s, e := n.StartByte(), n.EndByte()
	if e > uint(len(w.src)) {
		e = uint(len(w.src))
	}
	return string(w.src[s:e])
}

// walkPekko descends the route tree carrying the accumulated prefix and method.
func (w *dslWalker) walkPekko(n *sitter.Node, prefix, method string, unresolved bool) {
	if n == nil {
		return
	}
	if kindOf(n) == "call_expression" {
		if name, args, block := w.splitDirective(n); name != "" {
			// A verb directive fixes the method for everything below it.
			if verb, ok := pekkoVerbs[name]; ok && block != nil {
				w.walkPekko(block, prefix, verb, unresolved)
				return
			}
			if pekkoPathDirectives[name] {
				seg, segOK := w.pathSegments(args)
				childPrefix := prefix
				if seg != "" {
					childPrefix = facts.JoinRoutePath(prefix, seg)
				}
				childUnresolved := unresolved || !segOK

				// `path` matches to the END of the URL, so this IS the endpoint. Its
				// verbs come from the directive itself when conjoined, else from the
				// block it wraps, else from an enclosing verb directive.
				if name == "path" || name == "pathEnd" {
					verbs := w.conjoinedVerbs(n)
					if len(verbs) == 0 && block != nil {
						verbs = w.verbsInBlock(block)
					}
					if len(verbs) == 0 && method != "" {
						verbs = []string{method}
					}
					if len(verbs) == 0 {
						verbs = []string{facts.MethodAny}
					}
					for _, v := range verbs {
						w.emitPekkoRoute(n, childPrefix, v, childUnresolved)
					}
				}
				if block != nil {
					w.walkPekko(block, childPrefix, method, childUnresolved)
					return
				}
			}
		}
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		w.walkPekko(n.Child(i), prefix, method, unresolved)
	}
}

// conjoinedVerbs reads the verbs of `(path("x") & post)`, where the directive is
// combined with its method by `&` rather than by nesting inside it.
//
// Read STRUCTURALLY, from the operands of the conjunction. An earlier version
// scanned the enclosing node's source TEXT for a verb name, which was wrong twice
// over: the text of a `~`-composed tree also contains its SIBLING routes' verbs, and
// the scan iterated a Go map, so which verb won varied between runs. A snapshot that
// changes when nothing changed is the one defect enola cannot ship, and it slipped
// through as a passing test that failed on re-run.
func (w *dslWalker) conjoinedVerbs(pathCall *sitter.Node) []string {
	// Walk out through any parentheses to the conjunction, and no further.
	p := pathCall.Parent()
	for p != nil && kindOf(p) == "parenthesized_expression" {
		p = p.Parent()
	}
	if p == nil || kindOf(p) != "infix_expression" {
		return nil
	}
	var verbs []string
	var scan func(n *sitter.Node)
	scan = func(n *sitter.Node) {
		switch kindOf(n) {
		case "infix_expression":
			for i := uint(0); i < n.ChildCount(); i++ {
				if c := n.Child(i); c.IsNamed() && kindOf(c) != "operator_identifier" {
					scan(c)
				}
			}
		case "identifier":
			if verb, ok := pekkoVerbs[w.text(n)]; ok {
				verbs = appendUnique(verbs, verb)
			}
		}
	}
	scan(p)
	return verbs
}

// verbsInBlock returns the verb directives applied directly inside a path block,
// descending only through `~` composition so a NESTED path's verbs are not stolen.
// `path("x") { get { … } ~ post { … } }` really does serve two methods, and emitting
// one route per verb is what lets a client call of either match.
func (w *dslWalker) verbsInBlock(block *sitter.Node) []string {
	var verbs []string
	var scan func(n *sitter.Node)
	scan = func(n *sitter.Node) {
		if kindOf(n) == "call_expression" {
			name, _, inner := w.splitDirective(n)
			if verb, ok := pekkoVerbs[name]; ok {
				verbs = appendUnique(verbs, verb)
				return
			}
			if pekkoPathDirectives[name] {
				return // belongs to a deeper endpoint, not to this one
			}
			if inner != nil {
				scan(inner)
			}
			return
		}
		if kindOf(n) == "identifier" {
			if verb, ok := pekkoVerbs[w.text(n)]; ok {
				verbs = appendUnique(verbs, verb)
			}
			return
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			if c := n.Child(i); c.IsNamed() {
				scan(c)
			}
		}
	}
	scan(block)
	return verbs
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

// splitDirective decomposes `name(args) { block }` into its parts. The DSL applies a
// directive to a block as a second argument list, so the callee is itself a call.
func (w *dslWalker) splitDirective(n *sitter.Node) (name string, args, block *sitter.Node) {
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if !c.IsNamed() {
			continue
		}
		switch kindOf(c) {
		case "call_expression":
			// The curried form: the inner call carries the name and arguments.
			inner := firstNamedChild(c)
			if inner != nil && (kindOf(inner) == "identifier" || kindOf(inner) == "field_expression") {
				name = w.lastIdentifier(inner)
				for j := uint(0); j < c.ChildCount(); j++ {
					if a := c.Child(j); a.IsNamed() && kindOf(a) == "arguments" {
						args = a
					}
				}
			}
		case "identifier":
			if name == "" {
				name = w.text(c)
			}
		case "arguments":
			if args == nil {
				args = c
			}
		case "block", "lambda_expression":
			block = c
		}
	}
	return name, args, block
}

func (w *dslWalker) lastIdentifier(n *sitter.Node) string {
	if kindOf(n) == "identifier" {
		return w.text(n)
	}
	var last *sitter.Node
	for i := uint(0); i < n.ChildCount(); i++ {
		if c := n.Child(i); c.IsNamed() {
			last = c
		}
	}
	return w.text(last)
}

// pathSegments renders a directive's path argument. ok is false when any part of it
// is not a literal — a PathMatcher value, a constant, a computed segment — which the
// caller records rather than papering over.
func (w *dslWalker) pathSegments(args *sitter.Node) (path string, ok bool) {
	if args == nil {
		return "", true
	}
	var parts []string
	resolved := true
	var scan func(n *sitter.Node)
	scan = func(n *sitter.Node) {
		switch kindOf(n) {
		case "string":
			parts = append(parts, strings.Trim(w.text(n), `"`))
			return
		case "infix_expression":
			// `"v1" / "admin"` and `Segment / "x"` compose left to right.
			for i := uint(0); i < n.ChildCount(); i++ {
				c := n.Child(i)
				if !c.IsNamed() || kindOf(c) == "operator_identifier" {
					continue
				}
				scan(c)
			}
			return
		case "identifier", "field_expression", "call_expression", "generic_function":
			// A matcher (Segment, LongNumber) or a value holding the segment. Both
			// are real path components whose text is not knowable here.
			resolved = false
			parts = append(parts, ":"+strings.ToLower(w.lastIdentifier(n)))
			return
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			if c := n.Child(i); c.IsNamed() {
				scan(c)
			}
		}
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		if c := args.Child(i); c.IsNamed() {
			scan(c)
		}
	}
	return strings.Join(parts, "/"), resolved
}

func (w *dslWalker) emitPekkoRoute(n *sitter.Node, path, method string, unresolved bool) {
	if path == "" {
		return
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	props := map[string]any{
		"language":          "scala",
		facts.PropFramework: "pekko-http",
		facts.PropSource:    facts.RouteSourcePekkoHTTP,
		facts.PropRole:      facts.RoleServer,
		"method":            method,
		"path":              path,
	}
	if unresolved {
		props["path_unresolved"] = true
	}
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindRoute,
		Name:      path,
		File:      w.relFile,
		Line:      int(n.StartPosition().Row) + 1,
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
}

// extractHTTP4sRoutes walks a file for http4s route definitions.
//
// `HttpRoutes.of { case GET -> Root / "users" / LongVar(id) => … }` puts the whole
// endpoint in a PATTERN rather than in calls, so it is read from the case clause's
// infix tree: the leftmost operand is the method, `Root` anchors the path, and each
// `/` appends a segment. An extractor variable (`LongVar(id)`) becomes `:id`, the
// same canonical parameter spelling Play and the other frameworks normalize to.
func extractHTTP4sRoutes(root *sitter.Node, src []byte, relFile, dir string) []facts.Fact {
	w := &dslWalker{src: src, relFile: relFile, dir: dir}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if kindOf(n) == "call_expression" && w.isHTTP4sOf(n) {
			for i := uint(0); i < n.ChildCount(); i++ {
				if c := n.Child(i); kindOf(c) == "case_block" {
					w.emitHTTP4sCases(c)
				}
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return w.out
}

// isHTTP4sOf reports whether a call is `HttpRoutes.of` / `HttpRoutes.strict` (with or
// without a type argument), the two constructors that take a request pattern match.
func (w *dslWalker) isHTTP4sOf(n *sitter.Node) bool {
	fn := firstNamedChild(n)
	if fn == nil {
		return false
	}
	if kindOf(fn) == "generic_function" {
		fn = firstNamedChild(fn)
	}
	if fn == nil || kindOf(fn) != "field_expression" {
		return false
	}
	txt := strings.ReplaceAll(w.text(fn), " ", "")
	return strings.HasSuffix(txt, "HttpRoutes.of") || strings.HasSuffix(txt, "HttpRoutes.strict") ||
		strings.HasSuffix(txt, "AuthedRoutes.of")
}

func (w *dslWalker) emitHTTP4sCases(caseBlock *sitter.Node) {
	for i := uint(0); i < caseBlock.ChildCount(); i++ {
		c := caseBlock.Child(i)
		if kindOf(c) != "case_clause" {
			continue
		}
		for j := uint(0); j < c.ChildCount(); j++ {
			p := c.Child(j)
			if !p.IsNamed() || kindOf(p) != "infix_pattern" {
				continue
			}
			if method, path, ok := w.parseHTTP4sPattern(p); ok {
				w.out = append(w.out, facts.Fact{
					Kind: facts.KindRoute,
					Name: path,
					File: w.relFile,
					Line: int(c.StartPosition().Row) + 1,
					Props: map[string]any{
						"language":          "scala",
						facts.PropFramework: "http4s",
						facts.PropSource:    facts.RouteSourceHTTP4s,
						facts.PropRole:      facts.RoleServer,
						"method":            method,
						"path":              path,
					},
					Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
				})
			}
			break
		}
	}
}

// parseHTTP4sPattern flattens `GET -> Root / "users" / LongVar(id)` into a method and
// a path. The tree is left-nested, so it is collected and then read in source order.
func (w *dslWalker) parseHTTP4sPattern(n *sitter.Node) (method, path string, ok bool) {
	var operands []*sitter.Node
	var sawArrow bool
	var flatten func(node *sitter.Node)
	flatten = func(node *sitter.Node) {
		if kindOf(node) != "infix_pattern" {
			operands = append(operands, node)
			return
		}
		for i := uint(0); i < node.ChildCount(); i++ {
			c := node.Child(i)
			if !c.IsNamed() {
				continue
			}
			if kindOf(c) == "operator_identifier" {
				if w.text(c) == "->" {
					sawArrow = true
				}
				continue
			}
			flatten(c)
		}
	}
	flatten(n)
	if !sawArrow || len(operands) < 2 {
		return "", "", false
	}

	method = strings.ToUpper(w.text(operands[0]))
	if !isHTTPMethod(method) {
		return "", "", false
	}

	var segs []string
	for _, o := range operands[1:] {
		switch kindOf(o) {
		case "identifier":
			t := w.text(o)
			if t == "Root" {
				continue // the anchor, not a segment
			}
			// A bare binding in the pattern captures a segment.
			segs = append(segs, ":"+t)
		case "string":
			segs = append(segs, strings.Trim(w.text(o), `"`))
		case "case_class_pattern":
			// `LongVar(id)` / `UUIDVar(x)`: the binding name is the parameter.
			segs = append(segs, ":"+w.extractorBinding(o))
		default:
			segs = append(segs, ":"+strings.ToLower(w.lastIdentifier(o)))
		}
	}
	path = "/" + strings.Join(segs, "/")
	if len(segs) == 0 {
		path = "/"
	}
	return method, path, true
}

// extractorBinding returns the variable an http4s path extractor binds, so
// `LongVar(id)` yields `id` rather than the extractor's own name.
func (w *dslWalker) extractorBinding(n *sitter.Node) string {
	var last string
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c.IsNamed() && kindOf(c) == "identifier" {
			last = w.text(c)
		}
	}
	if last == "" {
		last = strings.ToLower(w.lastIdentifier(n))
	}
	return last
}

func isHTTPMethod(s string) bool {
	switch s {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "TRACE", "CONNECT":
		return true
	}
	return false
}

// extractDSLRoutes runs the Scala-source route and storage passes over one file,
// parsing it itself. The extractor goes through extractFileFacts instead, which
// shares the declaration walk's parse; this entry point remains for tests that
// exercise the DSL passes alone.
func extractDSLRoutes(src []byte, relFile string) []facts.Fact {
	ff, _ := withScalaTree(src, func(root *sitter.Node) ([]facts.Fact, string) {
		return dslFactsFrom(root, src, relFile), ""
	})
	return ff
}

// dslFactsFrom runs those passes over a tree the caller already parsed. Each one
// descends a combinator tree carrying state the declaration walk has no use for,
// which is why they are separate walks; they no longer need a separate parse.
func dslFactsFrom(root *sitter.Node, src []byte, relFile string) []facts.Fact {
	dir := factpath.Dir(relFile)

	out := extractPekkoRoutes(root, src, relFile, dir)
	out = append(out, extractHTTP4sRoutes(root, src, relFile, dir)...)
	out = append(out, extractHTTPClients(root, src, relFile, dir)...)
	return append(out, extractStorage(root, src, relFile, dir)...)
}
