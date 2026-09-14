package tsextractor

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/clientspec"
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
)

// Requests made through in-house HTTP clients the config declares (clients:, see
// internal/clientspec).
//
// enola recognises the clients it ships knowledge of: fetch, axios, Angular's
// HttpClient. A company's own wrapper, `httpRequestService.sendRequest(serviceName,
// path, options)`, is none of them, and nothing in its name or signature says HTTP. A
// spec says it: which injected type carries the client, which methods make a request,
// and which arguments hold the service name, the path and the verb.
//
// The spec admits a call; it never admits an edge. What this pass emits is a client
// route like any other, and the cross-repo linker still draws an edge only when a
// loaded repository serves that path.

// configuredClientFramework is the framework prop on a route read through a declared
// client. The library is the user's own, so there is no library name to report.
const configuredClientFramework = "configured-client"

// maxLocalFoldDepth bounds how many single-assigned locals a path may fold through, so
// a chain of locals cannot turn one argument into an unbounded walk.
const maxLocalFoldDepth = 8

// configuredMethod is one declared request method, with the spec that declared it.
type configuredMethod struct {
	spec   string
	method clientspec.Method
}

// indexClientSpecs maps receiver type, then method name, to its declaration, and
// returns the set of declared types. Validation guarantees a type and method pair is
// declared once.
func indexClientSpecs(specs []clientspec.Spec) (map[string]map[string]configuredMethod, map[string]bool) {
	index := map[string]map[string]configuredMethod{}
	types := map[string]bool{}
	for _, s := range specs {
		for _, t := range s.ReceiverTypes {
			types[t] = true
			if index[t] == nil {
				index[t] = map[string]configuredMethod{}
			}
			for _, m := range s.Methods {
				index[t][m.Name] = configuredMethod{spec: s.Name, method: m}
			}
		}
	}
	return index, types
}

// configuredClientFacts emits a client route for every request this file makes through
// a declared client.
//
// A call qualifies only as `this.<member>.<method>(…)`, where <member> is a field the
// enclosing class declared with a configured type and <method> is declared for that
// type. The method name alone never qualifies: an unrelated type with a method of the
// same name is not a client.
//
// Paths are derived, never guessed. `this.<field>` resolves against the ENCLOSING class
// only: an SDK declares a basePath on every connector it ships, so a repository-wide
// table would find each name bound to several values and resolve none of them. A local
// bound exactly once folds to its value. An operand that resolves to nothing is a path
// parameter, unless it leads the path, where it means the prefix is unknown and the
// call emits nothing.
func configuredClientFacts(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx, specs []clientspec.Spec) ([]facts.Fact, clientCounts) {
	index, types := indexClientSpecs(specs)
	var out []facts.Fact
	seen := map[string]bool{}
	counts := clientCounts{}
	for _, class := range angularClassNodes(kinds, root) {
		body := findChildByKind(kinds, class, "class_body")
		if body == nil {
			continue
		}
		receivers := typedReceivers(kinds, body, ctx.src, types)
		if len(receivers) == 0 {
			continue
		}
		for _, typ := range receivers {
			for _, s := range specs {
				if slices.Contains(s.ReceiverTypes, typ) {
					counts.get(s.Name).receivers++
				}
			}
		}
		className := ""
		if n := findChildByKind(kinds, class, "type_identifier"); n != nil {
			className = nodeText(n, ctx.src)
		}
		constants := angularClassConstants(kinds, body, ctx.src, className)

		for i := range body.ChildCount() {
			member := body.Child(i)
			switch kindOf(kinds, member) {
			case "method_definition", "public_field_definition":
			default:
				continue
			}
			r := pathResolver{
				kinds:     kinds,
				src:       ctx.src,
				className: className,
				constants: constants,
				locals:    singleAssignedLocals(kinds, member, ctx.src),
			}
			walkCalls(kinds, member, func(call *sitter.Node) {
				f, spec, cause, ok := configuredClientCall(call, ctx, index, receivers, r)
				if spec == "" {
					return // not a call through a declared client
				}
				n := counts.get(spec)
				n.calls++
				if !ok {
					n.skipped[cause]++
					return
				}
				n.routes++
				key := f.PropString("method") + "\x00" + f.Name + "\x00" + strconv.Itoa(f.Line)
				if seen[key] {
					return
				}
				seen[key] = true
				out = append(out, f)
			})
		}
	}
	return out, counts
}

// configuredClientCall reads one call against the declared clients. For every call made
// through a declared client it returns the declaring spec, with the route when the call
// became one and the cause when it did not. spec is empty for any other call.
func configuredClientCall(call *sitter.Node, ctx *extractCtx, index map[string]map[string]configuredMethod,
	receivers map[string]string, r pathResolver) (facts.Fact, string, string, bool) {

	fn := call.ChildByFieldName("function")
	if fn == nil {
		return facts.Fact{}, "", "", false
	}
	member, name, ok := thisMemberCall(r.kinds, fn, ctx.src)
	if !ok {
		return facts.Fact{}, "", "", false
	}
	typ, ok := receivers[member]
	if !ok {
		return facts.Fact{}, "", "", false
	}
	decl, ok := index[typ][name]
	if !ok {
		return facts.Fact{}, "", "", false
	}
	m := decl.method

	args := callArguments(r.kinds, call)
	pathArg := argAt(args, m.PathArg)
	if pathArg == nil {
		return facts.Fact{}, decl.spec, skipMissingPathArgument, false
	}
	raw, ok := r.text(pathArg)
	if !ok {
		return facts.Fact{}, decl.spec, skipDynamicPath, false
	}
	path, ok := rootRequestPath(raw)
	if !ok {
		return facts.Fact{}, decl.spec, skipNonRoutePath, false
	}

	verb := m.DefaultVerb
	if opts := argAt(args, m.OptionsArg); opts != nil {
		if v := objectVerb(r.kinds, opts, ctx.src, m.VerbOption); v != "" {
			verb = v
		}
	}
	if verb == "" {
		// Nothing states the verb, so the route carries none and the matcher pairs it
		// with whichever verb serves the path, as it does for a verb-less options call.
		verb = facts.MethodAny
	}

	props := map[string]any{
		facts.PropRole:       facts.RoleClient,
		"method":             verb,
		facts.PropFramework:  configuredClientFramework,
		"language":           "typescript",
		facts.PropSource:     facts.RouteSourceConfiguredHTTPClient,
		facts.PropClientSpec: decl.spec,
	}
	// The service name is a hint, not a host: it can only choose between repositories
	// that serve the path. A name that did not reduce to literal text is no name.
	if svc := argAt(args, m.ServiceArg); svc != nil {
		if service, ok := r.text(svc); ok && service != "" && !strings.Contains(service, "{}") {
			props["target_hint"] = service
		}
	}
	if testDoublePath(ctx.relFile) {
		props["test_double"] = true
	}
	return facts.Fact{
		Kind:      facts.KindRoute,
		Name:      path,
		File:      ctx.relFile,
		Line:      int(call.StartPosition().Row) + 1,
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: ctx.dir}},
	}, decl.spec, "", true
}

// thisMemberCallText matches `this.<member>.<method>` at the end of a callee, across the
// line breaks a chained call is written with. See angularHTTPReceiverCall for why the
// lexical form is needed beside the structural one.
var thisMemberCallText = regexp.MustCompile(`(?s)this\s*\.\s*([A-Za-z_$][\w$]*)\s*\.\s*([A-Za-z_$][\w$]*)$`)

// thisMemberCall reads `this.<member>.<method>` off a call's function node.
func thisMemberCall(kinds *tsutil.KindTable, fn *sitter.Node, src []byte) (member, method string, ok bool) {
	if kindOf(kinds, fn) == "member_expression" {
		prop := fn.ChildByFieldName("property")
		recv := fn.ChildByFieldName("object")
		if prop != nil && recv != nil {
			if m, found := angularThisMember(kinds, recv, src); found {
				return m, nodeText(prop, src), true
			}
		}
	}
	if m := thisMemberCallText.FindStringSubmatch(nodeText(fn, src)); m != nil {
		return m[1], m[2], true
	}
	return "", "", false
}

// callArguments returns a call's arguments in order, without comments.
func callArguments(kinds *tsutil.KindTable, call *sitter.Node) []*sitter.Node {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	var out []*sitter.Node
	for i := range args.ChildCount() {
		a := args.Child(i)
		if a.IsNamed() && kindOf(kinds, a) != "comment" {
			out = append(out, a)
		}
	}
	return out
}

// argAt returns the argument at a declared position, or nil when the position is not
// declared or the call passes fewer arguments.
func argAt(args []*sitter.Node, index *int) *sitter.Node {
	if index == nil || *index < 0 || *index >= len(args) {
		return nil
	}
	return args[*index]
}

// objectVerb reads a verb from an options object literal's named property: a quoted
// verb, or an identifier or enum member named for one (`GET`, `HttpMethod.POST`).
// Anything else, including a shorthand property, states no verb.
func objectVerb(kinds *tsutil.KindTable, obj *sitter.Node, src []byte, property string) string {
	if property == "" || kindOf(kinds, obj) != "object" {
		return ""
	}
	for i := range obj.ChildCount() {
		pair := obj.Child(i)
		if kindOf(kinds, pair) != "pair" {
			continue
		}
		key, value := pair.ChildByFieldName("key"), pair.ChildByFieldName("value")
		if key == nil || value == nil || strings.Trim(nodeText(key, src), `"'`) != property {
			continue
		}
		switch kindOf(kinds, value) {
		case "string", "identifier", "member_expression":
		default:
			return ""
		}
		text := strings.Trim(nodeText(value, src), `"'`)
		if dot := strings.LastIndexByte(text, '.'); dot >= 0 {
			text = text[dot+1:]
		}
		return mapClientVerb(text)
	}
	return ""
}

// walkCalls visits every call expression under n.
func walkCalls(kinds *tsutil.KindTable, n *sitter.Node, visit func(*sitter.Node)) {
	if n == nil {
		return
	}
	if kindOf(kinds, n) == "call_expression" {
		visit(n)
	}
	for i := range n.ChildCount() {
		walkCalls(kinds, n.Child(i), visit)
	}
}

// singleAssignedLocals maps each name bound exactly once in a class member, and never
// reassigned, to the expression it was bound to.
//
// A name bound more than once anywhere in the member folds to nothing, parameters
// included. Which binding a use sees is a scoping question this pass does not answer,
// and declining to answer it costs a route, where answering it wrongly invents one.
func singleAssignedLocals(kinds *tsutil.KindTable, scope *sitter.Node, src []byte) map[string]*sitter.Node {
	values := map[string]*sitter.Node{}
	bindings := map[string]int{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		switch kindOf(kinds, n) {
		case "variable_declarator":
			if name := n.ChildByFieldName("name"); name != nil && kindOf(kinds, name) == "identifier" {
				id := nodeText(name, src)
				bindings[id]++
				values[id] = n.ChildByFieldName("value")
			}
		case "required_parameter", "optional_parameter":
			if name := n.ChildByFieldName("pattern"); name != nil && kindOf(kinds, name) == "identifier" {
				bindings[nodeText(name, src)]++
			}
		case "assignment_expression", "augmented_assignment_expression":
			if left := n.ChildByFieldName("left"); left != nil && kindOf(kinds, left) == "identifier" {
				bindings[nodeText(left, src)] += 2
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(scope)

	out := map[string]*sitter.Node{}
	for id, v := range values {
		if bindings[id] == 1 && v != nil {
			out[id] = v
		}
	}
	return out
}

// pathPart is one operand of an argument: text that is known, or an operand that
// resolved to nothing.
type pathPart struct {
	text  string
	known bool
}

// pathResolver reduces call arguments to text within one class member.
type pathResolver struct {
	kinds     *tsutil.KindTable
	src       []byte
	className string
	constants map[string]string
	locals    map[string]*sitter.Node
}

// text reduces an argument to text, writing an unknown operand as the {} path
// parameter. It fails when the LEADING operand is unknown: then the prefix is unknown,
// and there is no honest way to write the path.
func (r pathResolver) text(n *sitter.Node) (string, bool) {
	parts := r.parts(n, 0)
	if len(parts) == 0 || !parts[0].known {
		return "", false
	}
	var b strings.Builder
	for _, p := range parts {
		if p.known {
			b.WriteString(p.text)
		} else {
			b.WriteString("{}")
		}
	}
	return b.String(), true
}

func (r pathResolver) parts(n *sitter.Node, depth int) []pathPart {
	unknown := []pathPart{{}}
	if n == nil {
		return unknown
	}
	switch kindOf(r.kinds, n) {
	case "string":
		return []pathPart{{text: strings.Trim(nodeText(n, r.src), `"'`), known: true}}

	case "template_string":
		return r.templateParts(n, depth)

	case "identifier":
		if v, ok := r.locals[nodeText(n, r.src)]; ok && depth < maxLocalFoldDepth {
			return r.parts(v, depth+1)
		}
		return unknown

	case "member_expression":
		// Only this class's own fields, as `this.x` or `ClassName.X`. Any other object
		// is a value this pass cannot see.
		ref := strings.Join(strings.Fields(nodeText(n, r.src)), "")
		if strings.HasPrefix(ref, "this.") || (r.className != "" && strings.HasPrefix(ref, r.className+".")) {
			if v, ok := r.constants[ref]; ok {
				return []pathPart{{text: v, known: true}}
			}
		}
		return unknown

	case "binary_expression":
		op := n.ChildByFieldName("operator")
		left, right := n.ChildByFieldName("left"), n.ChildByFieldName("right")
		if op == nil || nodeText(op, r.src) != "+" || left == nil || right == nil {
			return unknown
		}
		return append(r.parts(left, depth), r.parts(right, depth)...)

	case "parenthesized_expression", "as_expression", "non_null_expression", "satisfies_expression":
		for i := range n.ChildCount() {
			if c := n.Child(i); c.IsNamed() {
				return r.parts(c, depth)
			}
		}
	}
	return unknown
}

// templateParts splits a template literal into its literal text and the operands of
// its substitutions, in order.
func (r pathResolver) templateParts(n *sitter.Node, depth int) []pathPart {
	var out []pathPart
	literal := func(from, to uint) {
		if to > from {
			out = append(out, pathPart{text: string(r.src[from:to]), known: true})
		}
	}
	pos := n.StartByte() + 1 // past the opening backtick
	for i := range n.ChildCount() {
		c := n.Child(i)
		if kindOf(r.kinds, c) != "template_substitution" {
			continue
		}
		literal(pos, c.StartByte())
		var expr *sitter.Node
		for j := range c.ChildCount() {
			if e := c.Child(j); e.IsNamed() {
				expr = e
				break
			}
		}
		out = append(out, r.parts(expr, depth)...)
		pos = c.EndByte()
	}
	literal(pos, n.EndByte()-1) // up to the closing backtick
	return out
}

// Why a call through a declared client did not become a route.
const (
	skipMissingPathArgument = "missing_path_argument" // the call passes fewer arguments than path_arg names
	skipDynamicPath         = "dynamic_path"          // the path's leading operand is nothing enola can read as text
	skipNonRoutePath        = "non_route_path"        // the path is an absolute URL, or holds no literal segment
)

// configuredClientEdgeType is the edge_type a declared client's account reports under.
const configuredClientEdgeType = "configured_http_call"

// clientCount is what one declared client found, in one file or across a repository.
type clientCount struct {
	receivers int            // class members declared with one of the client's types
	calls     int            // calls to a declared method on such a member
	routes    int            // calls that became a client route
	skipped   map[string]int // calls that did not, by cause
}

// clientCounts is keyed by spec name.
type clientCounts map[string]*clientCount

func (c clientCounts) get(spec string) *clientCount {
	n := c[spec]
	if n == nil {
		n = &clientCount{skipped: map[string]int{}}
		c[spec] = n
	}
	return n
}

func (c clientCounts) merge(o clientCounts) {
	for spec, n := range o {
		t := c.get(spec)
		t.receivers += n.receivers
		t.calls += n.calls
		t.routes += n.routes
		for cause, k := range n.skipped {
			t.skipped[cause] += k
		}
	}
}

// clientCoverageFacts accounts for each declared client in one repository: the members
// that carry it, the calls made through it, the routes they became and the calls that
// did not, by cause.
//
// One fact per declared client, even one that found nothing here. "Declared, looked,
// found nothing" is exactly what a misspelt receiver type looks like, and the receiver
// count tells that apart from a client this repository injects but never calls. Whether
// nothing found is a problem is decided across every loaded repository, not here: a
// server repository calls no client at all.
func clientCoverageFacts(repoPath string, specs []clientspec.Spec, totals clientCounts) []facts.Fact {
	out := make([]facts.Fact, 0, len(specs))
	for _, s := range specs {
		n := totals[s.Name]
		if n == nil {
			n = &clientCount{}
		}
		unresolved := 0
		causes := make([]string, 0, len(n.skipped))
		for cause, k := range n.skipped {
			unresolved += k
			causes = append(causes, cause)
		}
		sort.Strings(causes)
		props := map[string]any{
			"extractor":          "typescript",
			"language":           "typescript",
			facts.PropClientSpec: s.Name,
			"receivers":          n.receivers,
			"edge_coverage": []map[string]any{{
				"edge_type":  configuredClientEdgeType,
				"detected":   n.calls,
				"resolved":   n.routes,
				"unresolved": unresolved,
			}},
		}
		if len(causes) > 0 {
			parts := make([]string, 0, len(causes))
			for _, cause := range causes {
				parts = append(parts, fmt.Sprintf("%s=%d", cause, n.skipped[cause]))
			}
			props["skipped"] = strings.Join(parts, ",")
		}
		out = append(out, facts.Fact{
			Kind:  facts.KindExtraction,
			Name:  "typescript:client:" + s.Name,
			File:  filepath.Base(repoPath),
			Props: props,
		})
	}
	return out
}
