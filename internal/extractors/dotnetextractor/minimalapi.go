package dotnetextractor

import (
	"path"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// ASP.NET Core minimal APIs.
//
// An endpoint is registered by a call rather than declared by an attribute:
//
//	var api = app.MapGroup("api/orders");
//	api.MapPut("/cancel", CancelOrderAsync);
//
// so the path is split between a group builder held in a LOCAL VARIABLE and the
// registration that uses it. Both live in one method body, which is what makes
// this resolvable without a whole-program pass — and also what bounds it: a group
// passed across a function boundary is not followed.
//
// This is the same shape the Go extractor composes for mux/chi subrouters and the
// Rust extractor for axum's .nest(), minus the interprocedural fixpoint.

// mapVerbs maps a minimal-API registration method to the HTTP verb it serves.
// MapGroup is deliberately absent — it mounts rather than serves, and is handled
// as a prefix binding.
var mapVerbs = map[string]string{
	"MapGet":    "GET",
	"MapPost":   "POST",
	"MapPut":    "PUT",
	"MapDelete": "DELETE",
	"MapPatch":  "PATCH",
}

// groupPrefix is a route-group builder's accumulated path. `known` is the whole
// point: a group built from a non-literal argument has a real prefix that this
// extractor cannot see, which is a different situation from a group mounted at the
// root, and conflating the two invents paths.
type groupPrefix struct {
	path  string
	known bool
}

// minimalAPIScan collects one file's minimal-API registrations.
type minimalAPIScan struct {
	src     []byte
	relFile string
	dir     string

	// typeStack qualifies a handler reference: a method-group argument names a
	// sibling of the registering method, so it resolves against the enclosing type.
	typeStack []string
	// methods are the enclosing methods, innermost last: a handler named by a
	// local function is looked for in their bodies.
	methods []scanMethod

	out []minimalRoute
}

// scanMethod is a method being scanned: its name and the node holding its body.
type scanMethod struct {
	name string
	node *sitter.Node
}

// minimalRoute is one resolved registration, ready to become a route fact.
type minimalRoute struct {
	Method  string
	Path    string
	Handler string // canonical fact name of a method-group handler, "" for a lambda
	// lambda is an inline handler, given a symbol by lambdaHandlerSymbols and then
	// cleared: the node does not outlive the file's parse tree. local names the
	// handler when it is a local function of the registering method, which may
	// serve several routes and is given one symbol for all of them.
	lambda *sitter.Node
	local  string
	// owner is the type the registration sits in, or the file's name for top-level
	// statements: what qualifies a lambda's symbol.
	owner string
	File  string
	Dir   string
	Line  int
}

// collectMinimalAPIRoutes walks a file for minimal-API registrations. It runs its
// own traversal rather than hooking the main walker because the analysis is
// scoped to a BODY and is order-sensitive — a group variable must be bound before
// the registration that reads it — while the main walker is organised around
// declarations.
func collectMinimalAPIRoutes(root *sitter.Node, src []byte, relFile, dir string) []minimalRoute {
	s := &minimalAPIScan{src: src, relFile: relFile, dir: dir}
	s.walk(root, nil)
	return s.out
}

// walk descends declarations, running a fresh scope over each body. `groups` is
// nil outside a body; a body starts with an empty map, so a variable in one method
// cannot leak into another.
func (s *minimalAPIScan) walk(node *sitter.Node, groups map[string]groupPrefix) {
	switch kindOf(node) {
	case "class_declaration", "struct_declaration", "record_declaration", "interface_declaration":
		name := nodeText(node.ChildByFieldName("name"), s.src)
		s.typeStack = append(s.typeStack, name)
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			s.walk(node.Child(i), nil)
		}
		s.typeStack = s.typeStack[:len(s.typeStack)-1]
		return
	case "method_declaration", "constructor_declaration", "local_function_statement":
		s.methods = append(s.methods, scanMethod{nodeText(node.ChildByFieldName("name"), s.src), node})
		defer func() { s.methods = s.methods[:len(s.methods)-1] }()
		scope := map[string]groupPrefix{}
		if body := node.ChildByFieldName("body"); body != nil {
			s.walk(body, scope)
		}
		if arrow := findChildByKind(node, "arrow_expression_clause"); arrow != nil {
			s.walk(arrow, scope)
		}
		return
	case "compilation_unit":
		// Top-level statements (a Program.cs with no class) are ONE implicit body
		// spread across sibling global_statement nodes, so the scope is seeded here
		// and threaded through them. Seeding it per global_statement instead gave
		// each statement a fresh map, and `var api = app.MapGroup("/api")` never
		// reached the `api.MapGet(...)` on the next line.
		//
		// Threading it down is safe because the two cases above take over: a type
		// declaration passes nil to its children, and a method body opens its own.
		groups = map[string]groupPrefix{}
	case "local_declaration_statement":
		if groups != nil {
			s.bindGroupVariable(node, groups)
		}
	case "invocation_expression":
		if groups != nil {
			s.registration(node, groups)
		}
	}

	for i := uint(0); i < uint(node.ChildCount()); i++ {
		s.walk(node.Child(i), groups)
	}
}

// bindGroupVariable records `var api = <expr>.MapGroup("prefix")`, resolving the
// prefix against the receiver's own when the receiver is another known group.
func (s *minimalAPIScan) bindGroupVariable(node *sitter.Node, groups map[string]groupPrefix) {
	decl := findChildByKind(node, "variable_declaration")
	if decl == nil {
		return
	}
	for i := uint(0); i < uint(decl.NamedChildCount()); i++ {
		d := decl.NamedChild(i)
		if kindOf(d) != "variable_declarator" {
			continue
		}
		name := nodeText(d.ChildByFieldName("name"), s.src)
		if name == "" {
			continue
		}
		// The initializer may be a chain — MapGroup("x").HasApiVersion(1.0) — so
		// find the MapGroup call inside it rather than requiring it at the top.
		for j := uint(0); j < uint(d.NamedChildCount()); j++ {
			if p, ok := s.groupPrefixOf(d.NamedChild(j), groups); ok {
				groups[name] = p
				break
			}
		}
	}
}

// groupPrefixOf resolves an expression that evaluates to a route group, returning
// its accumulated prefix. Descends a fluent chain to find the MapGroup call.
func (s *minimalAPIScan) groupPrefixOf(node *sitter.Node, groups map[string]groupPrefix) (groupPrefix, bool) {
	if node == nil {
		return groupPrefix{}, false
	}
	if kindOf(node) == "invocation_expression" {
		fn := node.ChildByFieldName("function")
		if fn != nil && kindOf(fn) == "member_access_expression" {
			if nodeText(fn.ChildByFieldName("name"), s.src) == "MapGroup" {
				recv, _ := s.receiverPrefix(fn.ChildByFieldName("expression"), groups)
				arg, literal := s.firstStringArg(node)
				if !literal {
					// The prefix is a parameter or a computed value — real, and
					// invisible here. Marking it unknown is what stops every route
					// under it from being published at a path it is not served at.
					return groupPrefix{known: false}, true
				}
				if !recv.known {
					return groupPrefix{known: false}, true
				}
				return groupPrefix{path: facts.JoinRoutePath(recv.path, arg), known: true}, true
			}
		}
	}
	// Not a MapGroup itself: look inside a fluent chain's receiver.
	if fn := node.ChildByFieldName("function"); fn != nil {
		if expr := fn.ChildByFieldName("expression"); expr != nil {
			return s.groupPrefixOf(expr, groups)
		}
	}
	return groupPrefix{}, false
}

// receiverPrefix resolves the prefix a registration's receiver contributes.
//
// A bare identifier is either a known group variable or the ROOT builder — the
// `app`/`endpoints` parameter every minimal-API extension method takes. Treating
// an unknown identifier as the root is what makes eShop's
// `MapOrdersApiV1(this IEndpointRouteBuilder app)` resolve, and it is safe in the
// direction that matters: a group built from a non-literal is marked unknown at
// its BINDING (above), so it never reaches here looking like a root.
func (s *minimalAPIScan) receiverPrefix(node *sitter.Node, groups map[string]groupPrefix) (groupPrefix, bool) {
	if node == nil {
		return groupPrefix{known: true}, true
	}
	if kindOf(node) == "identifier" {
		name := nodeText(node, s.src)
		if p, ok := groups[name]; ok {
			return p, true
		}
		return groupPrefix{known: true}, true // the root builder
	}
	// A chained receiver: app.NewVersionedApi().MapGroup(...) — the intermediate
	// call adds no path, so fall through to whatever it was called on.
	if p, ok := s.groupPrefixOf(node, groups); ok {
		return p, true
	}
	return groupPrefix{known: true}, true
}

// registration records a `<recv>.MapGet("path", handler)` call.
func (s *minimalAPIScan) registration(node *sitter.Node, groups map[string]groupPrefix) {
	fn := node.ChildByFieldName("function")
	if fn == nil || kindOf(fn) != "member_access_expression" {
		return
	}
	verb, ok := mapVerbs[nodeText(fn.ChildByFieldName("name"), s.src)]
	if !ok {
		return
	}
	// A string-literal first argument is the discriminator. MapControllers(),
	// MapHub<T>() and MapRazorPages() take no path and are excluded by it, as is
	// any same-named method on something that is not a route builder.
	sub, literal := s.firstStringArg(node)
	if !literal {
		return
	}

	prefix, _ := s.receiverPrefix(fn.ChildByFieldName("expression"), groups)
	if !prefix.known {
		// The group's real prefix is invisible. Publishing the registration path
		// alone would claim an endpoint at a path the service does not serve —
		// and when that path is "" (csharp-sdk registers its whole MCP surface
		// that way) it would additionally collapse every such route onto "/",
		// the same phantom-root failure conventional routing produced.
		return
	}

	r := minimalRoute{
		Method:  verb,
		Path:    facts.JoinRoutePath(prefix.path, sub),
		Handler: s.handlerTarget(node),
		lambda:  s.handlerLambda(node),
		owner:   s.ownerName(),
		File:    s.relFile,
		Dir:     s.dir,
		Line:    int(node.StartPosition().Row) + 1,
	}
	if r.lambda == nil {
		r.lambda, r.local = s.localFunctionHandler(node)
	}
	s.out = append(s.out, r)
}

// handlerTarget resolves a registration's second argument when it is a method
// group (`GetOrderAsync`), which names a sibling of the registering method. A
// lambda has no symbol to point at and yields "".
func (s *minimalAPIScan) handlerTarget(node *sitter.Node) string {
	args := node.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	idx := 0
	for i := uint(0); i < uint(args.NamedChildCount()); i++ {
		a := args.NamedChild(i)
		if kindOf(a) != "argument" {
			continue
		}
		idx++
		if idx != 2 {
			continue
		}
		inner := firstNamedChild(a)
		if inner == nil || kindOf(inner) != "identifier" {
			return ""
		}
		name := nodeText(inner, s.src)
		if len(s.typeStack) == 0 {
			return ""
		}
		return s.dir + "." + strings.Join(s.typeStack, ".") + "." + name
	}
	return ""
}

// ownerName is the enclosing type, or for top-level statements the file's name.
func (s *minimalAPIScan) ownerName() string {
	if len(s.typeStack) > 0 {
		return strings.Join(s.typeStack, ".")
	}
	base := path.Base(s.relFile)
	return strings.TrimSuffix(base, path.Ext(base))
}

// localFunctionHandler returns the local function a registration's handler names,
// `router.MapGet("json", RewriteArray)` with RewriteArray declared in the method that
// registers it, and its name with the method's: "Configure.RewriteArray". The
// innermost enclosing method that declares one by that name wins, as C# scoping does.
func (s *minimalAPIScan) localFunctionHandler(node *sitter.Node) (*sitter.Node, string) {
	name := s.handlerIdentifier(node)
	if name == "" {
		return nil, ""
	}
	for i := len(s.methods) - 1; i >= 0; i-- {
		if fn := localFunctionNamed(s.methods[i].node, name, s.src); fn != nil {
			return fn, s.methods[i].name + "." + name
		}
	}
	return nil, ""
}

// handlerIdentifier is a registration's second argument when it is a bare name.
func (s *minimalAPIScan) handlerIdentifier(node *sitter.Node) string {
	args := node.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	idx := 0
	for i := uint(0); i < uint(args.NamedChildCount()); i++ {
		a := args.NamedChild(i)
		if kindOf(a) != "argument" {
			continue
		}
		if idx++; idx == 2 {
			if inner := firstNamedChild(a); inner != nil && kindOf(inner) == "identifier" {
				return nodeText(inner, s.src)
			}
			return ""
		}
	}
	return ""
}

// localFunctionNamed finds a local function of the given name declared in a
// method's body, not inside another local function of it.
func localFunctionNamed(method *sitter.Node, name string, src []byte) *sitter.Node {
	body := method.ChildByFieldName("body")
	if body == nil {
		return nil
	}
	var found *sitter.Node
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		for i := uint(0); i < n.NamedChildCount() && found == nil; i++ {
			c := n.NamedChild(i)
			if kindOf(c) == "local_function_statement" {
				if nodeText(c.ChildByFieldName("name"), src) == name {
					found = c
				}
				continue
			}
			visit(c)
		}
	}
	visit(body)
	return found
}

// handlerLambda returns a registration's second argument when it is an inline
// function: `() => ...`, `async context => ...`, or `delegate { ... }`.
func (s *minimalAPIScan) handlerLambda(node *sitter.Node) *sitter.Node {
	args := node.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	idx := 0
	for i := uint(0); i < uint(args.NamedChildCount()); i++ {
		a := args.NamedChild(i)
		if kindOf(a) != "argument" {
			continue
		}
		if idx++; idx != 2 {
			continue
		}
		if inner := firstNamedChild(a); inner != nil {
			switch kindOf(inner) {
			case "lambda_expression", "anonymous_method_expression":
				return inner
			}
		}
		return nil
	}
	return nil
}

// firstStringArg returns an invocation's first argument when it is a string
// literal.
func (s *minimalAPIScan) firstStringArg(node *sitter.Node) (string, bool) {
	args := node.ChildByFieldName("arguments")
	if args == nil {
		return "", false
	}
	for i := uint(0); i < uint(args.NamedChildCount()); i++ {
		a := args.NamedChild(i)
		if kindOf(a) != "argument" {
			continue
		}
		inner := firstNamedChild(a)
		if inner == nil {
			return "", false
		}
		return stringLiteralText(inner, s.src)
	}
	return "", false
}

// minimalRouteFacts turns resolved registrations into route facts, binding each to
// its handler when that handler names a real symbol.
func minimalRouteFacts(routes []minimalRoute, symbols map[string]bool) []facts.Fact {
	out := make([]facts.Fact, 0, len(routes))
	for _, r := range routes {
		props := map[string]any{
			"method":    r.Method,
			"framework": "aspnetcore",
			"language":  "csharp",
		}
		rels := []facts.Relation{{Kind: facts.RelDeclares, Target: r.Dir}}
		if r.Handler != "" && symbols[r.Handler] {
			props["handler"] = r.Handler
			rels = append(rels, facts.Relation{Kind: facts.RelHandledBy, Target: r.Handler})
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindRoute,
			Name:      r.Path,
			File:      r.File,
			Line:      r.Line,
			Props:     props,
			Relations: rels,
		})
	}
	return out
}

// lambdaHandlerSymbols gives each inline minimal-API handler a symbol of its own,
// "<dir>.<Type>.handler:<VERB>:<line>", or for a local function of the registering
// method "<dir>.<Type>.<Method>.<Function>", one for every route it serves,
// carrying the calls of its body, and points the
// route at it. Without one the endpoint had no entry and its calls were the
// registering method's. The framework invokes it, so it is framework_registered.
func (w *astWalker) lambdaHandlerSymbols(routes []minimalRoute) {
	locals := map[uint]string{} // a local function's start byte -> its symbol
	for i := range routes {
		r := &routes[i]
		if r.lambda == nil {
			continue
		}
		if name, seen := locals[r.lambda.StartByte()]; seen {
			r.Handler, r.lambda = name, nil
			continue
		}
		short := "handler:" + r.Method + ":" + strconv.Itoa(r.Line)
		if r.local != "" {
			short = r.local
		}
		w.out = append(w.out, facts.Fact{
			Kind:    facts.KindSymbol,
			Name:    r.Dir + "." + r.owner + "." + short,
			File:    r.File,
			Line:    int(r.lambda.StartPosition().Row) + 1,
			EndLine: tsutil.EndLine(r.lambda),
			Props: map[string]any{
				"symbol_kind":          facts.SymbolFunc,
				"language":             "csharp",
				"framework_registered": true,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: r.Dir}},
		})
		idx := len(w.out) - 1
		w.pushOwner(idx)
		w.walkBodyWithMetrics(r.lambda, idx, short, 0)
		w.popOwner()
		r.Handler = w.out[idx].Name
		if r.local != "" {
			locals[r.lambda.StartByte()] = r.Handler
		}
		r.lambda = nil
	}
}
