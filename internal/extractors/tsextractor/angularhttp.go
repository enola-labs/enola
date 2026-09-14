// Angular's HttpClient call sites, read from a receiver whose type is known.
//
// The general TypeScript pass requires a client call's path to be a "/"-rooted
// literal. That rule is what keeps `map.get("key")` and `headers.get("content-type")`
// out of the graph, and it is the right rule when the receiver is anonymous. Here it
// is not: a class that injects `HttpClient` has a member whose declared type says so,
// and `this.<that member>.get(…)` is an HTTP request whatever its argument looks
// like. Two shapes that rule was rejecting, both measured on the corpus:
//
//   - `this.http.post<T>('api/v1/appdeployment', spec)` — a real request path with no
//     leading slash. One client's entire Angular module contributed zero call sites.
//   - `this.authHttp.get<T>(RunnerService.BASE_URL + '/registration-tokens')` — a
//     class-static base concatenated with a literal tail. 214 call sites in one
//     application, of which the general pass detected 12.
//
// Both are derivations. The receiver's type is declared, the static's value is a
// single assignment, and the tail is a literal. Nothing here is guessed: a call whose
// path cannot be reduced to literal text contributes nothing and is counted.
package tsextractor

import (
	"regexp"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
)

// angularHTTPTypes are the injected types whose members make HTTP requests.
var angularHTTPTypes = map[string]bool{
	"HttpClient":     true,
	"HttpXhrBackend": true,
}

// angularHTTPVerbs maps a method name on such a member to its HTTP verb.
var angularHTTPVerbs = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "patch": "PATCH",
	"delete": "DELETE", "head": "HEAD", "options": "OPTIONS",
}

// angularPathPart is one operand of a request path: literal text, or a reference to
// a constant whose value may live in another file. References are resolved after the
// whole repository is read — a base URL is routinely a static of the service that
// owns the resource, named by every other service that touches it.
type angularPathPart struct {
	literal string
	ref     string
}

// angularPendingRequest is one call site, held until the constants are all in hand.
type angularPendingRequest struct {
	verb  string
	line  int
	parts []angularPathPart
}

// angularHTTPFile is what one file contributes to the repo-wide request pass.
type angularHTTPFile struct {
	relFile   string
	dir       string
	constants map[string]string
	pending   []angularPendingRequest
}

// angularHTTPRoutes collects the requests this class makes, and the constants it
// declares for other classes to name.
func angularHTTPRoutes(kinds *tsutil.KindTable, body *sitter.Node, ctx *extractCtx, className string, into *angularHTTPFile) {
	if body == nil {
		return
	}
	for k, v := range angularClassConstants(kinds, body, ctx.src, className) {
		if _, exists := into.constants[k]; !exists {
			into.constants[k] = v
		}
	}
	receivers := angularHTTPReceivers(kinds, body, ctx.src)
	if len(receivers) == 0 {
		return
	}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if kindOf(kinds, n) == "call_expression" {
			if req, ok := angularHTTPCall(kinds, n, ctx, receivers); ok {
				into.pending = append(into.pending, req)
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(body)
}

// angularHTTPReceivers returns the member names bound to an HTTP client, by either
// injection dialect: a constructor parameter property or an `inject()` field.
func angularHTTPReceivers(kinds *tsutil.KindTable, body *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	for member := range typedReceivers(kinds, body, src, angularHTTPTypes) {
		out[member] = true
	}
	return out
}

// typedReceivers returns the members of a class bound to one of the given types, as
// member name to type name, by either injection dialect: a constructor parameter
// property or an `inject()` field.
func typedReceivers(kinds *tsutil.KindTable, body *sitter.Node, src []byte, types map[string]bool) map[string]string {
	out := map[string]string{}
	for i := range body.ChildCount() {
		member := body.Child(i)
		switch kindOf(kinds, member) {
		case "method_definition":
			if methodDecoratorName(kinds, member, src) != "constructor" {
				continue
			}
			params := member.ChildByFieldName("parameters")
			if params == nil {
				continue
			}
			for j := range params.ChildCount() {
				p := params.Child(j)
				switch kindOf(kinds, p) {
				case "required_parameter", "optional_parameter":
				default:
					continue
				}
				ann := p.ChildByFieldName("type")
				if ann == nil {
					continue
				}
				typ := plainTypeName(kinds, ann, src)
				if !types[typ] {
					continue
				}
				// The name is the `pattern` field, or — for a parameter property,
				// where the modifiers sit between — the first plain identifier.
				name := p.ChildByFieldName("pattern")
				if name == nil || kindOf(kinds, name) != "identifier" {
					name = findChildByKind(kinds, p, "identifier")
				}
				if name != nil {
					out[nodeText(name, src)] = typ
				}
			}
		case "public_field_definition":
			for _, t := range injectCallTypes(kinds, member, src) {
				if !types[t] {
					continue
				}
				if name := member.ChildByFieldName("name"); name != nil {
					out[nodeText(name, src)] = t
				}
			}
		}
	}
	return out
}

// angularClassConstants folds this class's string-valued fields to their literal
// path text, keyed by both the bare member name and its qualified form.
//
// `private static BASE_URL = environment.apiUrl + '/api/v1/runners'` reduces to
// `/api/v1/runners`: the identifier operand is a host this pass cannot know and the
// linker does not need — it matches on path suffixes — while the literal operands
// are exactly the path. A field with no literal operand at all reduces to nothing.
func angularClassConstants(kinds *tsutil.KindTable, body *sitter.Node, src []byte, className string) map[string]string {
	out := map[string]string{}
	for i := range body.ChildCount() {
		member := body.Child(i)
		if kindOf(kinds, member) != "public_field_definition" {
			continue
		}
		name := member.ChildByFieldName("name")
		value := member.ChildByFieldName("value")
		if name == nil || value == nil {
			continue
		}
		lit := angularLiteralConcat(kinds, value, src)
		if lit == "" {
			continue
		}
		key := nodeText(name, src)
		out[key] = lit
		out[className+"."+key] = lit
		out["this."+key] = lit
	}
	return out
}

// angularLiteralConcat reduces a `+` chain to the concatenation of its string
// literals, dropping every non-literal operand. A template literal contributes the
// text outside its interpolations.
func angularLiteralConcat(kinds *tsutil.KindTable, n *sitter.Node, src []byte) string {
	switch kindOf(kinds, n) {
	case "string":
		return strings.Trim(nodeText(n, src), `"'`)
	case "template_string":
		return tsInterpolation.ReplaceAllString(strings.Trim(nodeText(n, src), "`"), "")
	case "binary_expression":
		left, right := n.ChildByFieldName("left"), n.ChildByFieldName("right")
		if left == nil || right == nil {
			return ""
		}
		return angularLiteralConcat(kinds, left, src) + angularLiteralConcat(kinds, right, src)
	}
	return ""
}

// angularHTTPCall reads one `this.<httpMember>.<verb>(<path>, …)` call.
func angularHTTPCall(kinds *tsutil.KindTable, call *sitter.Node, ctx *extractCtx,
	receivers map[string]bool) (angularPendingRequest, bool) {

	fn := call.ChildByFieldName("function")
	if fn == nil {
		return angularPendingRequest{}, false
	}
	verb, member, ok := angularHTTPReceiverCall(kinds, fn, ctx.src)
	// The receiver must be a member this class declared as an HTTP client. A local
	// named `http`, or an unrelated object, is not one.
	if !ok || !receivers[member] {
		return angularPendingRequest{}, false
	}

	args := call.ChildByFieldName("arguments")
	if args == nil {
		return angularPendingRequest{}, false
	}
	var first *sitter.Node
	for i := range args.ChildCount() {
		if a := args.Child(i); a.IsNamed() {
			first = a
			break
		}
	}
	if first == nil {
		return angularPendingRequest{}, false
	}
	parts := angularRequestParts(kinds, first, ctx.src)
	if len(parts) == 0 {
		return angularPendingRequest{}, false
	}
	return angularPendingRequest{verb: verb, line: int(call.StartPosition().Row) + 1, parts: parts}, true
}

// composeAngularRequests resolves every held request against the constants the whole
// repository declared, and emits the ones that reduce to a path.
func composeAngularRequests(files []*angularHTTPFile) ([]facts.Fact, angularCounts) {
	var counts angularCounts
	constants := map[string]string{}
	ambiguous := map[string]bool{}
	for _, f := range files {
		if f == nil {
			continue
		}
		for k, v := range f.constants {
			if prev, ok := constants[k]; ok && prev != v {
				// Two classes name the same constant differently; neither resolves.
				ambiguous[k] = true
				continue
			}
			constants[k] = v
		}
	}
	for k := range ambiguous {
		delete(constants, k)
	}

	var out []facts.Fact
	for _, f := range files {
		if f == nil {
			continue
		}
		seen := map[string]bool{}
		for _, req := range f.pending {
			path, ok := angularResolveParts(req.parts, constants)
			if !ok {
				counts.miss("dynamic_request_path")
				continue
			}
			// A request path is rooted at the application's origin, so a relative
			// one is rooted here. The general client pass cannot do this — it has no
			// receiver type and so no way to tell a path from a map key — which is
			// why one client's whole Angular module contributed nothing.
			clean, ok := rootRequestPath(path)
			if !ok {
				counts.miss("dynamic_request_path")
				continue
			}
			key := req.verb + "\x00" + clean
			if seen[key] {
				continue
			}
			seen[key] = true
			counts.resolved++
			out = append(out, facts.Fact{
				Kind: facts.KindRoute,
				Name: clean,
				File: f.relFile,
				Line: req.line,
				Props: map[string]any{
					facts.PropRole:   facts.RoleClient,
					"method":         req.verb,
					"framework":      AngularFramework,
					"language":       "typescript",
					facts.PropSource: facts.RouteSourceTSHTTPClient,
					"api":            "angular-httpclient",
				},
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: f.dir}},
			})
		}
	}
	return out, counts
}

// rootRequestPath roots a path read off a typed client receiver at the application's
// origin and cleans it for matching. The receiver's type already established that the
// call is a request, so a relative path is a path rather than a map key.
func rootRequestPath(path string) (string, bool) {
	path = strings.TrimPrefix(path, "./")
	if !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "http") {
		path = "/" + path
	}
	path = strings.ReplaceAll(path, "/./", "/")
	clean, ok := cleanTSPath(path, nil)
	return clean, ok && clean != ""
}

// angularResolveParts joins a request's operands into path text.
//
// An unresolved operand in the LEADING position means the prefix is unknown and the
// whole path is unknown — there is no honest way to write it. Anywhere else it is a
// path parameter, which is what `BASE + '/' + id + '/stats'` says and what the
// cross-repo linker matches as a wildcard segment.
func angularResolveParts(parts []angularPathPart, constants map[string]string) (string, bool) {
	var b strings.Builder
	for i, p := range parts {
		if p.ref == "" {
			b.WriteString(p.literal)
			continue
		}
		v, ok := constants[p.ref]
		if !ok {
			if short := p.ref[strings.LastIndexByte(p.ref, '.')+1:]; short != p.ref {
				v, ok = constants[short]
			}
		}
		if ok {
			b.WriteString(v)
			continue
		}
		if i == 0 {
			return "", false
		}
		b.WriteString("{}")
	}
	return b.String(), true
}

// angularHTTPReceiverCall reads `this.<member>.<verb>` off a call's function node,
// structurally where the tree allows and lexically where it does not.
//
// The lexical half is not a shortcut. `await this.http_` followed by a newline and
// `.post<T>(…)` is genuinely ambiguous to the grammar — `post<string>(…)` also reads
// as two comparisons — and it parses as neither a member call nor an error. Every
// request in one client is written that way. The lexical form is still anchored: it
// must read `this.<member>.<verb>` at the end of the callee, and <member> must be a
// field this class declared as an HTTP client.
func angularHTTPReceiverCall(kinds *tsutil.KindTable, fn *sitter.Node, src []byte) (verb, member string, ok bool) {
	if kindOf(kinds, fn) == "member_expression" {
		prop := fn.ChildByFieldName("property")
		recv := fn.ChildByFieldName("object")
		if prop != nil && recv != nil {
			if v, isVerb := angularHTTPVerbs[nodeText(prop, src)]; isVerb {
				if m, found := angularThisMember(kinds, recv, src); found {
					return v, m, true
				}
			}
		}
	}
	if m := angularThisVerbCall.FindStringSubmatch(nodeText(fn, src)); m != nil {
		if v, isVerb := angularHTTPVerbs[m[2]]; isVerb {
			return v, m[1], true
		}
	}
	return "", "", false
}

// angularThisVerbCall matches `this.<member>.<verb>` at the end of a callee, across
// the line breaks a chained request is routinely written with.
var angularThisVerbCall = regexp.MustCompile(`(?s)this\s*\.\s*([A-Za-z_$][\w$]*)\s*\.\s*([a-z]+)$`)

// angularThisMember returns the member name a receiver expression reads off `this`,
// looking through the wrappers that can sit between.
//
// `await this.http_.post(…)` does not parse the way it reads: the grammar makes the
// call's receiver `await this.http_`, an await_expression, so a plain string-prefix
// test for "this." sees nothing. Every real client wraps requests that way, and it
// was the difference between reading a service's calls and reading none of them.
func angularThisMember(kinds *tsutil.KindTable, recv *sitter.Node, src []byte) (string, bool) {
	for depth := 0; recv != nil && depth < 4; depth++ {
		switch kindOf(kinds, recv) {
		case "member_expression":
			obj := recv.ChildByFieldName("object")
			prop := recv.ChildByFieldName("property")
			if obj == nil || prop == nil {
				return "", false
			}
			if kindOf(kinds, obj) == "this" || nodeText(obj, src) == "this" {
				return nodeText(prop, src), true
			}
			// `await this.http_` sits where the object would be.
			recv = obj
		case "await_expression", "parenthesized_expression", "non_null_expression":
			var next *sitter.Node
			for i := range recv.ChildCount() {
				if c := recv.Child(i); c.IsNamed() {
					next = c
				}
			}
			if next == nil {
				return "", false
			}
			recv = next
		default:
			return "", false
		}
	}
	return "", false
}

// angularRequestParts reduces a request's first argument to literal text and
// constant references, in order.
//
// The leading slash the general pass demands is NOT required: the receiver's type
// already established that this is a request, so `'api/v1/x'` is a path and not a
// map key. What is still required is that every operand be literal or a named
// constant — anything else makes the call dynamic, and it is counted rather than
// invented.
func angularRequestParts(kinds *tsutil.KindTable, arg *sitter.Node, src []byte) []angularPathPart {
	switch kindOf(kinds, arg) {
	case "string":
		return []angularPathPart{{literal: strings.Trim(nodeText(arg, src), `"'`)}}
	case "template_string":
		return []angularPathPart{{literal: strings.Trim(nodeText(arg, src), "`")}}
	case "identifier", "member_expression":
		return []angularPathPart{{ref: nodeText(arg, src)}}
	case "binary_expression":
		left, right := arg.ChildByFieldName("left"), arg.ChildByFieldName("right")
		if left == nil || right == nil {
			return nil
		}
		l := angularRequestParts(kinds, left, src)
		r := angularRequestParts(kinds, right, src)
		if len(l) == 0 || len(r) == 0 {
			return nil
		}
		return append(l, r...)
	}
	return nil
}
