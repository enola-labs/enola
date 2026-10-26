package tsextractor

import (
	"net/url"
	"sort"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
)

// What a test file hits, recorded on its test_ref for the post-link binder that
// resolves each against the loaded routes and writes exercises relations.
const (
	propTestHTTPCalls  = "test_http_calls"  // "METHOD /path"; METHOD is "*" when the test does not state one
	propTestPageVisits = "test_page_visits" // "/path" a browser test navigates to
	propTestGraphQL    = "test_graphql"     // "Query.field" a test sends inline
	propTestGraphQLOps = "test_graphql_ops" // operation names a test intercepts by
)

// testURLParam stands for a path segment the test computes (`${account.id}`).
const testURLParam = "\x00"

var testHTTPVerbs = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true}

// testCalls is what one test file requests, visits and sends.
type testCalls struct {
	http, visits, graphql, ops map[string]bool
}

// collectTestCalls reads a Cypress or Playwright file's requests (cy.request,
// cy.intercept, page.route, request.get/post/...), navigations (cy.visit,
// page.goto), the GraphQL queries it sends inline, and the operation names it
// matches intercepted requests by (`req.body.operationName === 'ListUsers'`).
// A URL is read from literals, template strings and the file's string constants;
// a leading computed part (`${Cypress.env('apiUrl')}`) is the base URL and dropped,
// a computed segment elsewhere is a parameter. A URL with no path is not recorded.
func collectTestCalls(kinds *tsutil.KindTable, root *sitter.Node, src []byte) testCalls {
	c := testCalls{http: map[string]bool{}, visits: map[string]bool{}, graphql: map[string]bool{}, ops: map[string]bool{}}
	consts := map[string]*sitter.Node{}
	var bind func(n *sitter.Node)
	bind = func(n *sitter.Node) {
		if kindOf(kinds, n) == "variable_declarator" {
			if name, val := n.ChildByFieldName("name"), n.ChildByFieldName("value"); name != nil && val != nil && kindOf(kinds, name) == "identifier" {
				switch kindOf(kinds, val) {
				case "string", "template_string", "binary_expression", "identifier":
					consts[nodeText(name, src)] = val
				}
			}
		}
		for i := range n.NamedChildCount() {
			bind(n.NamedChild(i))
		}
	}
	bind(root)
	r := &testURLReader{kinds: kinds, src: src, consts: consts}

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		switch kindOf(kinds, n) {
		case "call_expression":
			r.call(n, &c)
		case "binary_expression":
			if op := n.ChildByFieldName("operator"); op != nil && (nodeText(op, src) == "===" || nodeText(op, src) == "==") {
				l, rt := n.ChildByFieldName("left"), n.ChildByFieldName("right")
				for _, pair := range [][2]*sitter.Node{{l, rt}, {rt, l}} {
					if pair[0] != nil && pair[1] != nil && kindOf(kinds, pair[0]) == "member_expression" &&
						strings.HasSuffix(nodeText(pair[0], src), ".operationName") && kindOf(kinds, pair[1]) == "string" {
						c.ops[strings.Trim(nodeText(pair[1], src), `"'`)] = true
					}
				}
			}
		}
		for i := range n.NamedChildCount() {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
	return c
}

type testURLReader struct {
	kinds  *tsutil.KindTable
	src    []byte
	consts map[string]*sitter.Node
}

func (r *testURLReader) call(n *sitter.Node, c *testCalls) {
	fn := n.ChildByFieldName("function")
	if fn == nil || kindOf(r.kinds, fn) != "member_expression" {
		if fn != nil && kindOf(r.kinds, fn) == "identifier" {
			// Cypress's documented GraphQL helpers: aliasQuery(req, 'Op'), hasOperationName(req, 'Op').
			switch nodeText(fn, r.src) {
			case "aliasQuery", "aliasMutation", "hasOperationName":
				if args := namedArgs(r.kinds, n.ChildByFieldName("arguments")); len(args) == 2 && kindOf(r.kinds, args[1]) == "string" {
					c.ops[strings.Trim(nodeText(args[1], r.src), `"'`)] = true
				}
			}
		}
		return
	}
	obj, prop := fn.ChildByFieldName("object"), fn.ChildByFieldName("property")
	if obj == nil || prop == nil {
		return
	}
	recv, method := nodeText(obj, r.src), nodeText(prop, r.src)
	args := namedArgs(r.kinds, n.ChildByFieldName("arguments"))
	if len(args) == 0 {
		return
	}
	switch {
	case recv == "cy" && method == "visit", recv == "page" && method == "goto":
		if p, ok := r.urlArg(args[0], "url"); ok {
			c.visits[p] = true
		}
	case recv == "cy" && (method == "request" || method == "intercept"), recv == "page" && method == "route":
		verb, target, body := "", args[0], (*sitter.Node)(nil)
		if kindOf(r.kinds, args[0]) == "object" {
			verb = r.objString(args[0], "method")
			target = args[0]
			body = r.objNode(args[0], "body")
		} else if v := strings.Trim(nodeText(args[0], r.src), `"'`+"`"); testHTTPVerbs[strings.ToUpper(v)] && len(args) > 1 {
			verb, target = v, args[1]
			if len(args) > 2 && method == "request" {
				body = args[2]
			}
		}
		if verb == "" && method == "request" {
			verb = "GET"
		}
		p, ok := r.urlArg(target, "url")
		if !ok {
			return
		}
		if verb == "" {
			verb = "*"
		}
		c.http[strings.ToUpper(verb)+" "+p] = true
		if body != nil && kindOf(r.kinds, body) == "object" {
			if q, ok := r.text(r.objNode(body, "query")); ok {
				for _, f := range extractGraphQLClientOps(q, "", "") {
					c.graphql[f.Name] = true
				}
			}
		}
	case recv == "request" || strings.HasSuffix(recv, ".request"):
		verb := strings.ToUpper(method)
		if method == "fetch" {
			verb = "*"
		}
		if !testHTTPVerbs[verb] && verb != "*" {
			return
		}
		if p, ok := r.urlArg(args[0], ""); ok {
			c.http[verb+" "+p] = true
		}
	}
}

// urlArg reads a URL argument, or the url field of an options object, as a path.
func (r *testURLReader) urlArg(n *sitter.Node, field string) (string, bool) {
	if field != "" && kindOf(r.kinds, n) == "object" {
		n = r.objNode(n, field)
	}
	raw, ok := r.text(n)
	if !ok {
		return "", false
	}
	return testURLPath(raw)
}

// text evaluates a string-valued expression; a part it cannot evaluate becomes
// testURLParam.
func (r *testURLReader) text(n *sitter.Node) (string, bool) {
	return r.eval(n, 0)
}

func (r *testURLReader) eval(n *sitter.Node, depth int) (string, bool) {
	if n == nil || depth > 8 {
		return "", false
	}
	switch kindOf(r.kinds, n) {
	case "string":
		return strings.Trim(nodeText(n, r.src), `"'`), true
	case "template_string":
		var b strings.Builder
		for i := range n.NamedChildCount() {
			part := n.NamedChild(i)
			switch kindOf(r.kinds, part) {
			case "string_fragment":
				b.WriteString(nodeText(part, r.src))
			case "template_substitution":
				if v, ok := r.eval(part.NamedChild(0), depth+1); ok {
					b.WriteString(v)
				} else {
					b.WriteString(testURLParam)
				}
			}
		}
		return b.String(), true
	case "identifier":
		if v, ok := r.consts[nodeText(n, r.src)]; ok {
			return r.eval(v, depth+1)
		}
	case "binary_expression":
		if op := n.ChildByFieldName("operator"); op != nil && nodeText(op, r.src) == "+" {
			l, lok := r.eval(n.ChildByFieldName("left"), depth+1)
			rt, rok := r.eval(n.ChildByFieldName("right"), depth+1)
			if !lok {
				l = testURLParam
			}
			if !rok {
				rt = testURLParam
			}
			if lok || rok {
				return l + rt, true
			}
		}
	}
	return "", false
}

func (r *testURLReader) objNode(obj *sitter.Node, key string) *sitter.Node {
	for i := range obj.NamedChildCount() {
		p := obj.NamedChild(i)
		if kindOf(r.kinds, p) == "pair" {
			if k := p.ChildByFieldName("key"); k != nil && strings.Trim(nodeText(k, r.src), `"'`) == key {
				return p.ChildByFieldName("value")
			}
		}
		if kindOf(r.kinds, p) == "shorthand_property_identifier" && nodeText(p, r.src) == key {
			return r.consts[key]
		}
	}
	return nil
}

func (r *testURLReader) objString(obj *sitter.Node, key string) string {
	v, _ := r.text(r.objNode(obj, key))
	return v
}

// testURLPath turns an evaluated URL into the path it requests: an absolute URL
// keeps its path, a leading computed part is the base URL, the query string is cut,
// and every other computed part is one parameter segment.
func testURLPath(raw string) (string, bool) {
	raw = strings.TrimLeft(raw, testURLParam)
	if i := strings.Index(raw, "://"); i >= 0 {
		u, err := url.Parse(strings.ReplaceAll(raw, testURLParam, "x"))
		if err != nil {
			return "", false
		}
		raw = u.Path
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if !strings.HasPrefix(raw, "/") || strings.Contains(raw, "*") {
		return "", false
	}
	segs := strings.Split(raw, "/")
	for i, s := range segs {
		if strings.Contains(s, testURLParam) {
			segs[i] = ":param"
		}
	}
	return strings.Join(segs, "/"), true
}

// applyTestCalls records c on a file's test_ref, creating one when the file
// references nothing else.
func applyTestCalls(out []facts.Fact, relFile string, c testCalls) []facts.Fact {
	if len(c.http)+len(c.visits)+len(c.graphql)+len(c.ops) == 0 {
		return out
	}
	idx := -1
	for i := range out {
		if out[i].Kind == facts.KindTestRef {
			idx = i
		}
	}
	if idx < 0 {
		out = append(out, facts.Fact{Kind: facts.KindTestRef, Name: relFile, File: relFile, Line: 1,
			Props: map[string]any{"language": "typescript"}})
		idx = len(out) - 1
	}
	for prop, set := range map[string]map[string]bool{
		propTestHTTPCalls: c.http, propTestPageVisits: c.visits, propTestGraphQL: c.graphql, propTestGraphQLOps: c.ops,
	} {
		if len(set) == 0 {
			continue
		}
		vals := make([]string, 0, len(set))
		for v := range set {
			vals = append(vals, v)
		}
		sort.Strings(vals)
		out[idx].SetProp(prop, vals)
	}
	return out
}
