package rubyextractor

import (
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

// parseRouteFileAST parses a Rails route file with tree-sitter and emits KindRoute
// facts. Block boundaries come from the grammar (do_block) rather than counting
// `do`/`end`, so nested namespaces/resources/scopes are tracked precisely.
func parseRouteFileAST(src []byte, relFile string) []facts.Fact {
	ff, _ := parseRouteFile(src, relFile, "", jsonapiContext{format: jsonapiRouteDasherized})
	return ff
}

// routeMount records a `mount SomeEngine, at: '/x'` site: the constant that was
// mounted and the full URL prefix it was mounted under (including any enclosing
// scope/namespace). extractAllRoutes resolves the constant to the engine directory
// whose config/routes.rb should be parsed below that prefix.
type routeMount struct {
	constant string
	prefix   string
}

// routeFileResult is what parsing one route file taught us about the OTHER files that
// make up the same route table.
type routeFileResult struct {
	// draws maps each draw(:pkg) delegation found in this file to the URL prefix it is
	// scoped under, so the caller can parse config/routes/<pkg>.rb with it.
	draws map[string]string
	// mounts lists the engine mount sites found in this file.
	mounts []routeMount
	// unresolved counts the route-declaring macros this file used that the walker
	// could not expand, keyed by macro name, so the coverage fact can account for
	// them rather than letting the absence read as "no routes here".
	unresolved map[string]int
}

// parseRouteFile parses a Rails route file, seeding the scope stack with
// initialPrefix (the URL prefix under which a parent routes.rb delegated or mounted
// this file), and returns what it learned about further route files.
func parseRouteFile(src []byte, relFile, initialPrefix string, jsonapi jsonapiContext) ([]facts.Fact, routeFileResult) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
		return nil, routeFileResult{}
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()

	var stack []routeScope
	if initialPrefix != "" {
		stack = []routeScope{{pathPrefix: initialPrefix}}
	}
	rw := &routeWalker{
		src:       src,
		relFile:   relFile,
		dir:       factpath.Dir(relFile),
		draws:     map[string]string{},
		concerns:  map[string]*sitter.Node{},
		unhandled: map[string]int{},
		jsonapi:   jsonapi,
	}
	rw.walk(tree.RootNode(), stack)
	return rw.out, routeFileResult{draws: rw.draws, mounts: rw.mounts, unresolved: rw.unhandled}
}

type routeWalker struct {
	src     []byte
	relFile string
	dir     string
	out     []facts.Fact
	// draws maps each draw(:pkg) delegation found in this file to the URL prefix it
	// is scoped under, so the caller can parse config/routes/<pkg>.rb with it.
	draws map[string]string
	// mounts collects the engine mount sites found in this file.
	mounts []routeMount
	// concerns maps a `concern :name do ... end` definition to its block body, so a
	// later `concerns: :name` can replay it under the scope that referenced it. Rails
	// requires the definition to precede the reference, so a single forward pass is
	// enough.
	concerns map[string]*sitter.Node
	// concernDepth bounds concern replay. A concern that references itself would
	// otherwise recurse forever.
	concernDepth int
	// unhandled counts route-declaring macros this walker does not know, keyed by
	// macro name. `jsonapi_resources :companies` declares routes and produces
	// none here; counting it is what stops that absence reading as "no routes".
	unhandled map[string]int
	// jsonapi carries what a JSONAPI::Resources declaration needs to expand: how
	// this repository formats route segments, why it could not be read when it
	// could not, and the resolver from declaration to resource class.
	jsonapi jsonapiContext
}

// routeWrappers are macros that take a block and *contain* routes rather than
// declaring them. The walker descends into every one, so their contents are
// extracted and they are not misses. Listing them explicitly keeps the
// unhandled tally about macros whose routes are genuinely lost.
var routeWrappers = map[string]bool{
	"constraints": true, "authenticate": true, "authenticated": true,
	"unauthenticated": true, "defaults": true, "with_options": true,
	"direct": true, "resolve": true, "devise_for": true,
	"devise_scope": true, "as": true, "shallow": true, "expose": true,
	// Modifiers that take symbols but declare nothing. Doorkeeper's
	// skip_controllers *removes* routes; counting it as unread would inflate the
	// tally with a macro that resolving could not gain anything from.
	"skip_controllers": true, "skip_authorization": true,
}

// walk iterates the statements of a program / body_statement, dispatching each
// route-DSL call with the current scope stack.
//
// It also descends through plain Ruby control flow, because a route file is Ruby and
// real ones are full of it: GitLab guards whole files with `unless
// @organization_scoped_routes` and `if Rails.env.development?`, solidus wraps its admin
// routes in `if SolidusSupport.admin_available?`, and Rails' own activestorage route
// file is one `draw do … end` closed by an `if` MODIFIER. Iterating only direct `call`
// children silently dropped every one of them — five whole route files across the
// corpus, and the failure is invisible because a file that parses fine and yields
// nothing looks exactly like a file with no routes.
//
// Both branches of a conditional are walked. Which one Rails takes depends on runtime
// configuration the extractor cannot see, and a route that exists under some
// configuration is a route.
func (rw *routeWalker) walk(node *sitter.Node, stack []routeScope) {
	if node == nil {
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		if kindOf(c) == "call" {
			rw.handleCall(c, stack)
			continue
		}
		if isControlFlowNode(kindOf(c)) {
			rw.walkControlFlow(c, stack)
		}
	}
}

// walkControlFlow descends into a conditional or block-structured statement, skipping
// its CONDITION — `if Rails.env.development?` would otherwise be dispatched as a route
// DSL call named `development?`.
func (rw *routeWalker) walkControlFlow(node *sitter.Node, stack []routeScope) {
	cond := node.ChildByFieldName("condition")
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		if cond != nil && c.Id() == cond.Id() {
			continue
		}
		if kindOf(c) == "call" {
			rw.handleCall(c, stack)
			continue
		}
		if isControlFlowNode(kindOf(c)) {
			rw.walkControlFlow(c, stack)
		}
	}
}

// isControlFlowNode reports whether a node kind is Ruby control flow whose body may
// contain route declarations.
func isControlFlowNode(kind string) bool {
	switch kind {
	case "if", "unless", "elsif", "else", "then", "if_modifier", "unless_modifier",
		"case", "when", "begin", "ensure", "rescue", "body_statement",
		"parenthesized_statements", "while", "until", "each":
		return true
	}
	return false
}

// blockBody returns the body_statement of a call's do/brace block, or nil.
func blockBody(call *sitter.Node) *sitter.Node {
	block := call.ChildByFieldName("block")
	if block == nil {
		return nil
	}
	return block.ChildByFieldName("body")
}

func (rw *routeWalker) handleCall(call *sitter.Node, stack []routeScope) {
	method := rubyText(call.ChildByFieldName("method"), rw.src)
	args := call.ChildByFieldName("arguments")
	body := blockBody(call)
	prefix := buildPrefix(stack)

	switch method {
	case "get", "post", "put", "patch", "delete":
		// Accept both a string path ('cities_by_zip') and a bare symbol (:cities_by_zip);
		// the positional helper also avoids picking up a `to:` handler string.
		path := firstPositionalPath(args, rw.src)
		// `get :settings, path: "verify_new_email/:token"` renames the segment the
		// action is served at. Reading only the action name emits a path the app
		// does not serve AND misses the one it does — one declaration reported as
		// two defects, which is the shape this comparison keeps finding.
		override, pathGiven := pairStringPresent(args, "path", rw.src)
		if pathGiven {
			path = override
		}
		if path == "" {
			return
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		// Rails distinguishes three placements inside a `resources` block and
		// they do not agree on the parameter name. A bare verb nests under the
		// parent member id (`/steps/:step_id/status`), which buildPrefix has
		// already supplied; `on: :member` addresses the resource itself and
		// uses `:id` (`/steps/:id/audit`); `on: :collection` stays at the
		// collection path.
		on := pairSymbol(args, "on", rw.src)
		switch on {
		case "collection":
			prefix = collectionPrefix(stack)
		case "member":
			prefix = collectionPrefix(stack) + memberSegment(stack)
		}
		// Rails' path_for_action: inside a member/collection scope — the block form
		// or the `on:` option — an action given as a SYMBOL that is one of the
		// canonical actions is served at the scope's own path. `collection do
		// get :index, to: 'reviews#show' end` is GET /orders/:id/review,
		// not /review/index. A string path (`get 'index'`) or an explicit `path:`
		// keeps its segment, as Rails does.
		inMethodScope := on == "collection" || on == "member" ||
			(len(stack) > 0 && stack[len(stack)-1].resourceMethodScope)
		if inMethodScope && !pathGiven && firstPositionalIsSymbol(args) &&
			canonicalResourceActions[strings.TrimPrefix(path, "/")] {
			path = ""
		}
		props := map[string]any{
			"method":    strings.ToUpper(method),
			"framework": "rails",
			"language":  "ruby",
		}
		handler := pairString(args, "to", rw.src)
		if handler == "" {
			// The hash-rocket form `get 'path' => 'ctrl#action'`, where the handler is
			// the VALUE of the pair whose key is the path. Discourse and lobsters write
			// nearly every route this way, and reading only `to:` left thousands of
			// routes with no handler at all.
			handler = hashRocketHandler(args, rw.src)
		}
		if handler == "" && !routeEndpointGiven(args, rw.src) {
			// Rails' own shorthand: a multi-segment string path that names no endpoint
			// of its own IS the endpoint. get_to_from_path turns "billing/invoices"
			// into a `to:` of "billing#invoices" before anything else is read, so the
			// derived name OUTRANKS the enclosing controller rather than deferring to
			// it: `get "reports/monthly"` inside `controller :legacy` is served by
			// reports#monthly. Deriving the action alone and keeping the enclosing
			// controller names a controller that exists and does not serve this route.
			// An interpolated path is known only as far as its literal prefix,
			// and the shorthand derives BOTH names from it, so a prefix would
			// invent a controller and an action the application never serves.
			// Declining leaves the route with no handler, which is the honest
			// answer and the one this extractor already gives elsewhere.
			if literal, interpolated := firstPositionalStringParts(args, rw.src); !interpolated {
				handler = matchShorthand(literal)
			}
		}
		if handler != "" {
			handler = qualifyHandler(handler, buildModule(stack))
		} else {
			// Still nothing, so the handler has to be assembled from an action and a
			// controller named separately.
			action := firstPositionalName(args, rw.src)
			// `action:` names the action outright, in either spelling. Reading only
			// `action: "x"` misses the commoner `action: :x`, and the positional
			// fallback finds nothing whenever the path is not itself a bare action name
			// (`get "summary/:period", action: :summary`), so the route was emitted with
			// no handler at all rather than with the wrong one.
			if act := pairSymbol(args, "action", rw.src); act != "" {
				action = act
			} else if act := pairString(args, "action", rw.src); act != "" {
				action = act
			}
			// The verb may name its own controller, and when it does that name wins
			// over the enclosing one: Rails reads `controller:` off the call
			// (`map_match(..., controller: nil, ...)`) and only then falls back to the
			// scope (`controller ||= @scope[:controller]`). Reading the action while
			// leaving this option unread resolves the route to whichever controller
			// encloses it, which is a real controller that does not serve this route —
			// `get :group_analytics, controller: "group_analytics/stage_types",
			// action: :index` is served by group_analytics/stage_types#index and by
			// nothing else. Otherwise the enclosing resources/controller block names it
			// (`resources :posts do get :publish, on: :member end` -> posts#publish),
			// which is how the majority of non-REST Rails routes are written.
			//
			// When neither names one there is nothing to derive from, and the route is
			// emitted with no handler at all. A route that claims no handler is a gap a
			// consumer can see; a route that names a controller not serving it cannot be
			// told apart from one that does.
			ctrl, absolute, given := controllerOption(args, rw.src)
			if !given {
				ctrl, absolute = currentController(stack)
			}
			if action != "" && ctrl != "" {
				handler = composeController(ctrl, buildModule(stack), absolute) + "#" + action
			}
		}
		if handler != "" {
			props["handler"] = handler
		}
		// A Rails optional segment `foo(/:bar)` serves two paths; emit both.
		for _, p := range expandOptionalSegments(path) {
			rw.emit(prefix+p, line(call), props)
		}

	case "root":
		handler := pairString(args, "to", rw.src)
		if handler == "" {
			handler = firstStringArg(args, rw.src)
		}
		props := map[string]any{
			"method":    "GET",
			"framework": "rails",
			"language":  "ruby",
		}
		if handler != "" {
			props["handler"] = qualifyHandler(handler, buildModule(stack))
		}
		rw.emit(prefix+"/", line(call), props)

	case "resources", "resource", "jsonapi_resources", "jsonapi_resource":
		// Symbol or string, as for `namespace` above.
		name := firstSymbolArg(args, rw.src)
		if name == "" {
			name = strings.Trim(firstStringArg(args, rw.src), "/")
		}
		if name == "" {
			return
		}
		only, onlyGiven := pairSymbolsPresent(args, "only", rw.src)
		except, exceptGiven := pairSymbolsPresent(args, "except", rw.src)
		singular := method == "resource" || method == "jsonapi_resource"
		jsonapi := method == "jsonapi_resources" || method == "jsonapi_resource"
		if jsonapi && rw.jsonapi.format == jsonapiRouteUnknown {
			// A repository-supplied route formatter decides the URL segment, and
			// reading Ruby to find out what it decides is guessing. Count the
			// declaration against the cause rather than the macro: "29 unread
			// jsonapi_resources" reads as an extractor limitation, and this is a
			// located line of configuration.
			cause := rw.jsonapi.refusalCause
			if cause == "" {
				cause = method
			}
			rw.unhandled[cause]++
			return
		}

		// A resource nested inside a *plural* `resources` block nests under the parent
		// member (`/widgets/:widget_id/...`), and buildPrefix has already materialized
		// that param into the prefix, so this resource's own segment must not repeat
		// it. parentNesting is the parent's segment and member param together, which
		// is exactly what a shallow member route strips back off below.
		parentNesting := ""
		if len(stack) > 0 {
			if p := stack[len(stack)-1].memberParam; p != "" {
				parentNesting = stack[len(stack)-1].pathPrefix + "/:" + p
			}
		}
		// `path:` overrides the URL segment while the resource name still drives the
		// props and the nested member param (Rails derives `:name_id` from the name).
		segmentName := name
		if jsonapi {
			segmentName = jsonapiSegment(name, rw.jsonapi.format)
		}
		if p := pairString(args, "path", rw.src); p != "" {
			segmentName = strings.Trim(p, "/")
		}
		segment := "/" + segmentName
		resourcePath := prefix + segment

		// Rails `shallow: true` serves a nested plural resource's MEMBER routes
		// (show/edit/update/destroy) at a shallow path — the parent resource segment
		// and its member param are dropped — while the collection routes
		// (index/create/new) stay nested. shallowBase is the prefix with the parent
		// resource segment stripped, plus this resource's own segment.
		// `shallow: true` is declared on the *parent* and applies to everything
		// nested inside it, so it has to be inherited down the stack rather than
		// read only off this call. Reading it locally means the common spelling —
		// `resources :posts, shallow: true do resources :comments end` — never
		// takes effect on the resource it was written for.
		shallow := !singular && parentNesting != "" &&
			(pairBool(args, "shallow", rw.src) || inheritedShallow(stack))
		shallowBase := strings.TrimSuffix(prefix, parentNesting) + "/" + segmentName

		// The controller a resource is served by, unless `controller:` names one
		// explicitly. Rails spells this rule twice and the two disagree: `Resource`
		// takes the name verbatim (`@controller = options[:controller] || @name`, so
		// `resources :people` is served by people), while `SingletonResource`
		// pluralizes (`@controller = options[:controller] || plural`, so
		// `resource :profile` is served by profiles). Applying the singular rule to
		// both turns people into peoples and points at a controller that never exists.
		//
		// The pluralization that rule needs is ActiveSupport's, which knows the
		// irregulars: `resource :person` is served by people. This extractor's
		// inflector is a handful of suffix rules and answers persons, so a singular
		// resource that does not name its controller gets NO handler rather than a
		// guessed one — the same refusal the extractor has always made here, and the
		// reason it has never grown a pluralize-for-Rails rule. Naming a controller
		// that does not serve the route cannot be distinguished from naming the one
		// that does; naming none is a gap that can be seen and closed.
		controllerName, absolute, given := controllerOption(args, rw.src)
		switch {
		case given:
		case !singular:
			controllerName = name
		default:
			controllerName = ""
		}
		// `resources :widgets, module: :dashboards` is served by dashboards/widgets:
		// Rails lifts a module: option off a resource declaration into a scope
		// wrapping it, so it composes into this resource's own handlers and into
		// every route declared inside its block. It is pushed only now, after the
		// parent's member param has been read off the top of the stack.
		if resourceModule := pairSymbol(args, "module", rw.src); resourceModule != "" {
			stack = append(stack, routeScope{module: resourceModule})
		} else if resourceModule := pairString(args, "module", rw.src); resourceModule != "" {
			stack = append(stack, routeScope{module: resourceModule})
		}
		mod := buildModule(stack)
		// The module composes here for this declaration's own RESTful routes, and
		// again at every route site inside the block — which is why childScope carries
		// the BARE name below. Rails composes at the route rather than at the
		// declaration: `Mapping.build` captures `scope[:module]` where the route is
		// created and `add_controller_module` joins it on there, so a `scope module:`
		// entered between this declaration and a verb inside its block is part of that
		// verb's handler and nothing of it belongs to these.
		//
		// add_controller_module joins a name that already carries a namespace just as
		// it joins a bare one: `controller: "candidate/job_offers"` inside a `connect`
		// module scope is served at connect/candidate/job_offers, which the booted
		// route table states plainly. Skipping the composition for a namespaced
		// override is reasoning rather than measurement, and it was wrong for 399 of
		// the monolith's handlers. Its one exception is the leading slash, which
		// composeController carries.
		qualifiedController := ""
		if controllerName != "" {
			qualifiedController = composeController(controllerName, mod, absolute)
		}

		filter := actionFilter{only: only, except: except, onlyGiven: onlyGiven, exceptGiven: exceptGiven}
		actions := restfulActions(filter)
		switch {
		case jsonapi && singular:
			actions = jsonapiRestfulActionsSingular(filter)
		case jsonapi:
			actions = jsonapiRestfulActions(filter)
		case singular:
			actions = restfulActionsSingular(filter)
		}
		var resourceClass *jsonapiResourceClass
		if jsonapi && rw.jsonapi.resolver != nil {
			resourceClass = rw.jsonapi.resolver.resourceClass(mod, name)
		}
		if resourceClass != nil && resourceClass.immutable {
			actions = filterActions(actions, writeActions, false)
		}
		// `resources :sync, param: :key` renames the member segment: Rails serves
		// /sync/:key, and every member route under it uses that name. The action
		// tables spell the default, so the rename is applied to what they produce.
		memberName := "id"
		if p := pairSymbol(args, "param", rw.src); p != "" {
			memberName = p
		}
		for _, a := range actions {
			routePath := resourcePath + strings.ReplaceAll(a.suffix, "/:id", "/:"+memberName)
			if shallow && strings.HasPrefix(a.suffix, "/:id") {
				routePath = shallowBase + strings.ReplaceAll(a.suffix, "/:id", "/:"+memberName)
			}
			resourceProps := map[string]any{
				"method":    a.method,
				"framework": "rails",
				"language":  "ruby",
				"resource":  name,
				"action":    a.name,
			}
			if qualifiedController != "" {
				resourceProps["handler"] = qualifiedController + "#" + a.name
			}
			rw.emit(routePath, line(call), resourceProps)
		}
		if resourceClass != nil {
			rw.emitJsonapiRelationships(call, resourcePath, mod, name, singular, resourceClass)
		}
		// A plural resource exposes a member id to its children; a singular one does not.
		childMember := ""
		if !singular {
			childMember = singularize(name) + "_id"
		}
		childScope := routeScope{
			pathPrefix:         segment,
			memberParam:        childMember,
			controller:         controllerName,
			controllerAbsolute: absolute,
			controllerUnknown:  controllerName == "",
			ownParam:           memberName,
			singularOwner:      singular,
			shallow:            pairBool(args, "shallow", rw.src) || inheritedShallow(stack),
		}
		if body != nil {
			rw.walk(body, append(stack, childScope))
		}
		// `resources :posts, concerns: :commentable` replays the named concern's block
		// inside the resource's own scope, exactly as if it had been written inline.
		rw.replayConcerns(concernNames(args, rw.src), append(stack, childScope))

	case "match":
		// `match 'x', via: [:get, :post]` maps one path to several verbs; emit one
		// route per listed verb. Without a `via:` the verb set is ambiguous (older
		// Rails defaulted to all), so emit nothing rather than guess.
		path := firstPositionalPath(args, rw.src)
		if path == "" {
			return
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		handler := pairString(args, "to", rw.src)
		for _, v := range symbolValues(findPairValue(args, "via", rw.src), rw.src) {
			verb := strings.ToUpper(v)
			if verb == "" || verb == "ALL" {
				continue // via: :all matches every verb — not a concrete route
			}
			props := map[string]any{
				"method":    verb,
				"framework": "rails",
				"language":  "ruby",
			}
			if handler != "" {
				props["handler"] = handler
			}
			for _, p := range expandOptionalSegments(path) {
				rw.emit(prefix+p, line(call), props)
			}
		}

	case "namespace":
		// Rails accepts a symbol or a string: `namespace :admin` and
		// `namespace "recaptcha"` are the same declaration. Reading only the symbol form
		// made the whole block invisible, taking every route inside it with it.
		name := firstSymbolArg(args, rw.src)
		if name == "" {
			name = firstStringArg(args, rw.src)
		}
		if name == "" || body == nil {
			return
		}
		// `path:` overrides the URL segment (module stays the symbol name), e.g.
		// `namespace :admin, path: 'administration'`.
		pathSeg := "/" + name
		if p, present := pairStringPresent(args, "path", rw.src); present {
			if p != "" && !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			pathSeg = p
		}
		rw.walk(body, append(stack, routeScope{pathPrefix: pathSeg, module: name}))

	case "scope":
		// module: and path: are independent — a scope may set either or both. Read a
		// positional string path or a `path:` keyword for the URL prefix, and `module:`
		// for the controller namespace.
		ns := routeScope{}
		// Only a *positional* string is a path. `scope module: "internal"` names
		// a controller namespace and contributes no URL segment; reading its
		// value as a path prefixes every route inside with /internal.
		path, explicit := firstPositionalString(args, rw.src), false
		if path == "" {
			path, explicit = pairStringPresent(args, "path", rw.src)
		}
		if path == "" {
			// A bare positional symbol is a path prefix: `scope :users` == `scope
			// path: 'users'`. (module: is a keyword pair, so it is not read here.)
			path = firstSymbolArg(args, rw.src)
		}
		if path != "" || explicit {
			if path != "" && !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			ns.pathPrefix = path
		}
		// `scope module: "api"` is as common as the symbol form, and reading only
		// symbols loses the whole "api" segment of every controller under it.
		mod := pairSymbol(args, "module", rw.src)
		if mod == "" {
			mod = pairString(args, "module", rw.src)
		}
		if mod != "" {
			ns.module = mod
		}
		// `scope controller: "copilot"` fixes the controller for its block exactly as the
		// `controller ... do` form does — merge_controller_scope keeps the child and
		// discards the parent — and it is the only other construct that writes the
		// @scope[:controller] a verb falls back to. Leaving it unread does not leave the
		// routes inside without a controller: the search walks outward to the enclosing
		// resource instead and names one that exists and serves entirely different
		// routes. The name goes on bare, so the module in force at each route site
		// composes there rather than here.
		//
		// A `controller:` that was written and could not be read stops the search rather
		// than falling through it, which is the same refusal a singular resource makes.
		if name, absolute, given := controllerOption(args, rw.src); given {
			ns.controller = name
			ns.controllerAbsolute = absolute
			ns.controllerUnknown = name == ""
		}
		if body != nil {
			rw.walk(body, append(stack, ns))
		}

	case "member", "collection":
		memberPrefix := ""
		if method == "member" {
			memberPrefix = memberSegment(stack)
		}
		if body != nil {
			rw.walk(body, append(stack, routeScope{
				pathPrefix: memberPrefix, dropParentMember: true, resourceMethodScope: true,
			}))
		}

	case "controller":
		// `controller :photos do get 'search' end` fixes the controller for its block
		// without changing the URL. The name goes onto the scope bare, so each route
		// inside composes the namespace in force at its own site — the same rule
		// `resources` follows, which is what stops the two spellings of one route from
		// disagreeing about who serves it. Rails' add_controller_module joins the
		// module on whether or not the name already carries a namespace, so skipping
		// the composition for `controller "audit/exports"` loses the enclosing module
		// exactly as it did for a namespaced `controller:` override — and the leading
		// slash escapes it here for the same reason it does there.
		name := firstSymbolArg(args, rw.src)
		if name == "" {
			name = firstStringArg(args, rw.src)
		}
		if name == "" || body == nil {
			return
		}
		rw.walk(body, append(stack, routeScope{
			controller:         strings.TrimPrefix(name, "/"),
			controllerAbsolute: strings.HasPrefix(name, "/"),
		}))

	case "concern":
		// `concern :commentable do ... end` DEFINES a reusable block; it serves nothing
		// on its own. Record the body so each `concerns: :commentable` reference can
		// replay it under the scope that referenced it, and do NOT descend here — the
		// routes inside belong to the referencing scopes, not to this definition site.
		if name := firstSymbolArg(args, rw.src); name != "" && body != nil {
			rw.concerns[name] = body
		}

	case "concerns":
		// The bare block form: `resources :posts do concerns :commentable end`.
		rw.replayConcerns(symbolArgs(args, rw.src), stack)

	case "mount":
		// `mount SomeEngine, at: '/x'` or `mount SomeEngine => '/x'`. The engine's whole
		// route table is served below the mount path, so record the site for
		// extractAllRoutes to resolve; the placeholder route below keeps the mount
		// itself visible in the graph.
		constant, at := parseMount(args, rw.src)
		if constant == "" {
			rw.unhandled[method]++
			return
		}
		if !strings.HasPrefix(at, "/") {
			at = "/" + at
		}
		mountPrefix := strings.TrimSuffix(prefix+at, "/")
		rw.mounts = append(rw.mounts, routeMount{constant: constant, prefix: mountPrefix})
		// Method "MOUNT" is inert to the cross-repo linker for the same reason "DRAW"
		// is: it is a mount point, not an endpoint a client can call.
		rw.out = append(rw.out, facts.Fact{
			Kind: facts.KindRoute,
			Name: mountPrefix + "/",
			File: rw.relFile,
			Line: line(call),
			Props: map[string]any{
				"method":    "MOUNT",
				"framework": "rails",
				"language":  "ruby",
				"mounts":    constant,
			},
			Relations: []facts.Relation{
				{Kind: facts.RelDeclares, Target: rw.dir},
				{Kind: facts.RelHandledBy, Target: normalizeConstant(constant)},
			},
		})

	case "draw":
		// `draw do ... end` is the routes wrapper (Rails.application.routes.draw,
		// engine routers, etc.) — recurse into the block. `draw(:pkg)` with no
		// block is a packwerk delegation — emit a DRAW route.
		if body != nil {
			rw.walk(body, stack)
			return
		}
		if pkg := firstSymbolArg(args, rw.src); pkg != "" {
			// Record the delegation so the caller can parse config/routes/<pkg>.rb
			// seeded with this prefix, giving its routes their real /api/vN scope.
			if rw.draws != nil {
				rw.draws[pkg] = prefix
			}
			// A DRAW placeholder route is still emitted (it backs route helpers); the
			// linker treats method "DRAW" as inert, so it never matches or is flagged.
			rw.out = append(rw.out, facts.Fact{
				Kind: facts.KindRoute,
				Name: prefix + "/" + pkg,
				File: rw.relFile,
				Line: line(call),
				Props: map[string]any{
					"method":    "DRAW",
					"framework": "rails",
					"language":  "ruby",
					"delegate":  pkg,
				},
			})
		}

	default:
		// Unknown DSL call — descend into any block so nested routes are still
		// discovered. A macro naming a resource by symbol is a different case: it
		// declares routes this walker cannot produce, and going quiet about that
		// is what makes an absent API surface look like an empty one. Count it.
		if !routeWrappers[method] && firstSymbolArg(args, rw.src) != "" {
			rw.unhandled[method]++
		}
		if body != nil {
			rw.walk(body, stack)
		}
	}
}

// emit appends a route fact with a declares relation to the file's directory, plus a
// handled_by relation to the controller action serving it when the handler resolves to
// one. That second edge is what makes a Rails route reachable from its controller:
// without it a route is an isolated node, impact analysis from a controller cannot see
// the endpoints it serves, and a controller referenced only by the route table looks
// like dead code.
func (rw *routeWalker) emit(name string, lineNum int, props map[string]any) {
	rels := []facts.Relation{{Kind: facts.RelDeclares, Target: rw.dir}}
	if h, _ := props["handler"].(string); h != "" {
		if sym := controllerSymbol(h); sym != "" {
			rels = append(rels, facts.Relation{Kind: facts.RelHandledBy, Target: sym})
		}
	}
	rw.out = append(rw.out, facts.Fact{
		Kind:      facts.KindRoute,
		Name:      name,
		File:      rw.relFile,
		Line:      lineNum,
		Props:     props,
		Relations: rels,
	})
}

// --- mounts and concerns ---

// parseMount reads the two shapes Rails accepts for a mount:
//
//	mount Spree::Core::Engine, at: '/'
//	mount Sidekiq::Web => '/sidekiq'
//
// and returns the mounted constant with the path it is mounted at. A Rack app built
// inline (`mount Coverband::Reporters::Web.new, at: '/coverage'`) yields its receiver
// constant, which is the thing worth recording. The path defaults to "/" — Rails'
// own default when `at:` is omitted.
func parseMount(args *sitter.Node, src []byte) (constant, at string) {
	if args == nil {
		return "", ""
	}
	at = pairString(args, "at", src)
	for i := uint(0); i < args.ChildCount(); i++ {
		c := args.Child(i)
		switch kindOf(c) {
		case "constant", "scope_resolution":
			if constant == "" {
				constant = rubyText(c, src)
			}
		case "call":
			// `Foo::Bar.new` — the receiver is the constant.
			if r := c.ChildByFieldName("receiver"); r != nil && constant == "" {
				switch kindOf(r) {
				case "constant", "scope_resolution":
					constant = rubyText(r, src)
				}
			}
		case "pair":
			// Hash-rocket form: the KEY names the mounted app, the value the path.
			k := c.ChildByFieldName("key")
			if k == nil {
				continue
			}
			named := ""
			switch kindOf(k) {
			case "constant", "scope_resolution":
				named = rubyText(k, src)
			case "call":
				// A Rack app built inline on the key side —
				// `mount Flipper::UI.app(Flipper) => "/admin/flipper"`. The `at:`
				// spelling of the same shape already yields its receiver constant;
				// reading only a bare constant here dropped the whole declaration,
				// path included.
				if r := k.ChildByFieldName("receiver"); r != nil {
					switch kindOf(r) {
					case "constant", "scope_resolution":
						named = rubyText(r, src)
					}
				}
			}
			if named == "" {
				continue
			}
			if constant == "" {
				constant = named
			}
			if v := c.ChildByFieldName("value"); v != nil && at == "" {
				at = firstStringArg(v, src)
			}
		}
	}
	if constant == "" {
		return "", ""
	}
	if at == "" {
		at = "/"
	}
	return constant, at
}

// hashRocketHandler reads the handler out of the hash-rocket route form
// `get 'path' => 'controller#action'`, where the path is the pair's KEY and the handler
// its VALUE.
//
// Only a plain string value is accepted. `get 'x' => redirect('/y')` and
// `get 'x' => SomeRackApp` are also legal and name no controller action, so returning
// their text would produce a handled_by edge to a node that never exists.
func hashRocketHandler(args *sitter.Node, src []byte) string {
	v := hashRocketRouteValue(args)
	if v == nil || kindOf(v) != "string" {
		return ""
	}
	return firstStringArg(v, src)
}

// hashRocketRouteValue returns the value node of the hash-rocket route form's pair —
// the one whose key is the path string — whatever that value is. Whether it names a
// controller action is a separate question from whether Rails saw a `to:` at all,
// which is what the match shorthand turns on.
func hashRocketRouteValue(args *sitter.Node) *sitter.Node {
	if args == nil {
		return nil
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		c := args.Child(i)
		if kindOf(c) != "pair" {
			continue
		}
		if k := c.ChildByFieldName("key"); k != nil && kindOf(k) == "string" {
			return c.ChildByFieldName("value")
		}
	}
	return nil
}

// concernNames returns the names referenced by a `concerns:` option, which takes either
// a single symbol or an array of them.
func concernNames(args *sitter.Node, src []byte) []string {
	return symbolValues(findPairValue(args, "concerns", src), src)
}

// replayConcerns re-walks each named concern's recorded block under stack, so its routes
// are emitted once per referencing scope with that scope's prefix and controller.
//
// maxConcernDepth bounds the recursion: a concern that references itself, directly or
// through another, would otherwise never terminate. Two levels covers concerns composed
// of concerns, which is as deep as the idiom goes in practice.
const maxConcernDepth = 2

func (rw *routeWalker) replayConcerns(names []string, stack []routeScope) {
	if len(names) == 0 || rw.concernDepth >= maxConcernDepth {
		return
	}
	rw.concernDepth++
	defer func() { rw.concernDepth-- }()
	for _, n := range names {
		if body := rw.concerns[n]; body != nil {
			rw.walk(body, stack)
		}
	}
}

// --- keyword-argument helpers ---

// pairBool reports whether args contains a `key: true` pair (e.g. `shallow: true`).
func pairBool(args *sitter.Node, key string, src []byte) bool {
	v := findPairValue(args, key, src)
	return v != nil && rubyText(v, src) == "true"
}

// expandOptionalSegments expands a Rails optional route segment `foo(/:bar)` into
// the concrete paths it serves — one with the optional groups omitted and one with
// them included: `email_subscriptions(/:key)` → ["/email_subscriptions",
// "/email_subscriptions/:key"]. A path with no optional group is returned as-is.
// (Multiple optional groups collapse to the all-omitted and all-included variants,
// which covers the trailing-optional idiom without an exponential blow-up.)
func expandOptionalSegments(path string) []string {
	if !strings.Contains(path, "(") {
		return []string{path}
	}
	included := strings.NewReplacer("(", "", ")", "").Replace(path)
	var b strings.Builder
	depth := 0
	for _, r := range path {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	omitted := b.String()
	if omitted == included {
		return []string{included}
	}
	return []string{omitted, included}
}

// pairString returns the string content of a `key: "value"` pair.
func pairString(args *sitter.Node, key string, src []byte) string {
	value, _ := pairStringPresent(args, key, src)
	return value
}

// pairStringPresent distinguishes `path: ""` from no `path:` at all. Rails
// treats the empty string as a real override — `namespace :app, path: ""`
// mounts at the root and contributes no segment — so a caller that cannot tell
// the two apart falls back to the namespace name and invents a segment.
func pairStringPresent(args *sitter.Node, key string, src []byte) (string, bool) {
	v := findPairValue(args, key, src)
	if v == nil {
		return "", false
	}
	return firstStringArg(v, src), true
}

// pairSymbol returns the symbol name of a `key: :value` pair.
func pairSymbol(args *sitter.Node, key string, src []byte) string {
	if v := findPairValue(args, key, src); v != nil && kindOf(v) == "simple_symbol" {
		return strings.TrimPrefix(rubyText(v, src), ":")
	}
	return ""
}

// pairSymbolsPresent returns the symbol names of a `key: [:a, :b]` pair and
// whether the pair was written at all.
//
// The two are different questions and Rails uses the difference: `only: []`
// serves NO RESTful action, and thirteen declarations in the monolith's routes
// say exactly that to open a block of custom routes. Deciding by the size of
// what parsed — the natural way to write "was a filter given" — reads an empty
// declaration as an absent one and emits all eight actions.
func pairSymbolsPresent(args *sitter.Node, key string, src []byte) (map[string]bool, bool) {
	out := make(map[string]bool)
	v := findPairValue(args, key, src)
	if v == nil {
		return out, false
	}
	for _, name := range symbolValues(v, src) {
		out[name] = true
	}
	return out, true
}

// symbolValues returns the symbol names of a value node, which may be a single
// `:sym`, an array `[:a, :b]`, or the `%i[a b]` literal — Ruby's three spellings
// of the same list. The grammar gives `%i[]` members as bare_symbol rather than
// simple_symbol, and reading only the latter silently turns `only: %i[index show]`
// into no filter at all, which serves six routes where two are served.
func symbolValues(v *sitter.Node, src []byte) []string {
	if v == nil {
		return nil
	}
	if kindOf(v) == "simple_symbol" {
		return []string{strings.TrimPrefix(rubyText(v, src), ":")}
	}
	var out []string
	for i := uint(0); i < v.ChildCount(); i++ {
		switch c := v.Child(i); kindOf(c) {
		case "simple_symbol":
			out = append(out, strings.TrimPrefix(rubyText(c, src), ":"))
		case "bare_symbol", "bare_string":
			// %i[a b] gives bare_symbol and %w[a b] gives bare_string — Ruby's
			// second and third spellings of the same list. Reading one and not the
			// other drops the filter entirely, which serves eight routes where two
			// are served.
			out = append(out, rubyText(c, src))
		}
	}
	return out
}

// findPairValue returns the value node of a `key: value` pair in an argument_list,
// in either of Ruby's two spellings of the same hash.
//
// The grammar gives a label key (`controller:`) text that a trailing colon has to be
// trimmed from, and a hash-rocket key (`:controller =>`) text that carries a LEADING
// one. Trimming only the trailing colon matched the first spelling and left the
// second invisible — not just `:controller => "x"` but `:on => :collection`,
// `:only => [...]` and every other option, on route tables that are ordinary Ruby and
// mix the two freely.
//
// A string key is not an option name. It is the path of the hash-rocket ROUTE form
// (`get "path" => "c#a"`), which hashRocketHandler reads instead.
func findPairValue(args *sitter.Node, key string, src []byte) *sitter.Node {
	if args == nil {
		return nil
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		c := args.Child(i)
		if kindOf(c) != "pair" {
			continue
		}
		k := c.ChildByFieldName("key")
		if k == nil || kindOf(k) == "string" {
			continue
		}
		if strings.TrimPrefix(strings.TrimSuffix(rubyText(k, src), ":"), ":") == key {
			return c.ChildByFieldName("value")
		}
	}
	return nil
}

// inheritedShallow reports whether any enclosing scope declared shallow
// nesting. Rails scopes it lexically, so the flag is a property of the
// surrounding block rather than of the call that happens to read it.
func inheritedShallow(stack []routeScope) bool {
	for _, scope := range stack {
		if scope.shallow {
			return true
		}
	}
	return false
}

// firstPositionalString returns the first *direct* string argument, ignoring
// keyword pairs entirely.
//
// firstStringArg recurses into the whole argument node, so for
// `scope module: "internal"` it returns "internal" — the value of a keyword
// that names a controller namespace, not a URL segment. Reading it as a path
// prefixes every route in the block with a segment Rails never serves.
func firstPositionalString(args *sitter.Node, src []byte) string {
	text, _ := firstPositionalStringParts(args, src)
	return text
}

// firstPositionalStringParts reads the first string argument and reports
// whether the source spelled it with interpolation. The text is the literal
// part alone, which is the right answer for a route path — a path assembled at
// runtime is still worth recording as far as it is known — and the wrong one
// for anything DERIVED from the path, because deriving from a prefix invents a
// name the application never serves. Callers that derive ask for the flag.
func firstPositionalStringParts(args *sitter.Node, src []byte) (string, bool) {
	if args == nil {
		return "", false
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		child := args.Child(i)
		if kindOf(child) != "string" {
			continue
		}
		var text string
		var interpolated bool
		for j := uint(0); j < child.ChildCount(); j++ {
			switch part := child.Child(j); kindOf(part) {
			case "string_content":
				if text == "" {
					text = rubyText(part, src)
				}
			case "interpolation":
				interpolated = true
			}
		}
		return text, interpolated
	}
	return "", false
}

// positionalSymbols returns the direct symbol arguments of a call, ignoring
// keyword pairs: the `:a, :b` of `validates :a, :b`.
func positionalSymbols(args *sitter.Node, src []byte) []string {
	if args == nil {
		return nil
	}
	var out []string
	for i := uint(0); i < args.ChildCount(); i++ {
		child := args.Child(i)
		if kindOf(child) == "simple_symbol" {
			out = append(out, strings.TrimPrefix(rubyText(child, src), ":"))
		}
	}
	return out
}

// emitJsonapiRelationships emits the routes a resource class's relationships
// serve: four verbs each under /relationships/<name>, a fifth for to-many, and
// one related-resource GET whose controller is whoever serves the target.
//
// A relationship whose target does not resolve still gets its routes — the path
// is known either way — and loses only the handler prop. Saying "this endpoint
// exists and I do not know what serves it" is the whole discipline.
func (rw *routeWalker) emitJsonapiRelationships(call *sitter.Node, resourcePath, mod, declared string, singular bool, res *jsonapiResourceClass) {
	base := resourcePath
	if !singular {
		base += "/:" + singularize(declared) + "_id"
	}
	owner := rw.jsonapi.resolver.controllerFor(mod, declared)
	at := line(call)

	for _, rel := range res.relationships {
		segment := jsonapiSegment(rel.name, rw.jsonapi.format)
		props := func(action, handler string) map[string]any {
			p := map[string]any{
				"method": "", "framework": "rails", "language": "ruby",
				"resource": declared, "relationship": rel.name, "action": action,
			}
			if handler != "" {
				p["handler"] = handler + "#" + action
			}
			return p
		}

		// `immutable` guards the write half of the relationship routes as well as
		// the RESTful ones — the gem wraps update, destroy and create in a single
		// `if res.mutable?`, so a read-only resource serves show_relationship alone.
		routes := jsonapiRelationshipRoutes
		if res.immutable {
			routes = filterActions(routes, map[string]bool{"show_relationship": true}, true)
		} else if rel.toMany {
			routes = append(append([]restAction{}, routes...), restAction{name: "create_relationship", method: "POST"})
		}
		for _, a := range routes {
			p := props(a.name, owner)
			p["method"] = a.method
			rw.emit(base+"/relationships/"+segment, at, p)
		}

		related := "get_related_resource"
		if rel.toMany {
			related = "get_related_resources"
		}
		p := props(related, rw.jsonapi.resolver.handlerFor(mod, rel, res, singularize(declared)))
		p["method"] = "GET"
		rw.emit(base+"/"+segment, at, p)
	}
}

// qualifyHandler composes a handler with the controller namespace in force where the
// route is created. `to: "foo#show"` inside `scope module: "connect"` is served by
// connect/foo#show, and emitting the bare name points at a controller that does not
// exist. A name that already carries a namespace of its own is composed too:
// `to: "candidate/job_offers#show"` inside a `jobsite` module scope is served by
// jobsite/candidate/job_offers, which the booted route table says plainly.
//
// A leading slash escapes the composition. add_controller_module's first branch
// strips the slash and RETURNS, uncomposed, so `to: "/admin/exports#index"` inside
// `namespace :api` is served by admin/exports and not by api/admin/exports. It is
// applied here rather than only where `controller:` is read because Rails splits a
// `to:` string into a controller and an action and puts that controller through the
// same function — reading the marker for one spelling and not the other left the
// other composing a module Rails never applies.
//
// A handler already prefixed by the module in force is composed anyway, because
// Rails composes it: `namespace :api do get "api/sub/thing" end` is served by
// api/api/sub#thing. Declining looked safer — the doubled name belongs to no
// controller most applications have — but it picks the worse of two wrong answers.
// The doubled name dangles, and a dangling handler is visibly unresolved; the
// undoubled one names a real controller that serves other routes and cannot be
// told apart from a correct answer. Skipping the join also contradicted the
// sibling case it was meant to protect: a two-segment `api/echo` under the same
// namespace was already composed to api/api#echo, so only the longer spelling
// diverged.
func qualifyHandler(handler, module string) string {
	if strings.HasPrefix(handler, "/") {
		return strings.TrimPrefix(handler, "/")
	}
	if module == "" {
		return handler
	}
	return module + "/" + handler
}

// matchShorthandPath is using_match_shorthand?: two or more plain segments, and
// nothing else. A path parameter, a dot or an optional group all fall outside it, and
// Rails then has no action to serve the route with unless the call named one.
var matchShorthandPath = regexp.MustCompile(`^/?[-\w]+/[-\w/]+$`)

// matchShorthand derives the handler Rails reads out of the path itself when a String
// path names no endpoint of its own: get_to_from_path rewrites "billing/invoices" as
// "billing#invoices" and hands it on as the `to:`, which is why the derived name wins
// over an enclosing `controller` scope rather than deferring to it.
//
// Only a trailing "(.:format)" is stripped, exactly the group Rails strips before the
// test. Dashes become underscores across the whole derived name, as `tr` does.
//
// An empty action is declined rather than emitted. Rails accepts one — "a/b/" yields
// "a/b#" and a route with a blank action — and a handler that names no action is a
// relation to a node the graph will never hold.
func matchShorthand(path string) string {
	path = strings.TrimSuffix(path, "(.:format)")
	if !matchShorthandPath.MatchString(path) {
		return ""
	}
	trimmed := strings.TrimPrefix(path, "/")
	cut := strings.LastIndex(trimmed, "/")
	if cut <= 0 || cut == len(trimmed)-1 {
		return ""
	}
	return strings.ReplaceAll(trimmed[:cut]+"#"+trimmed[cut+1:], "-", "_")
}

// routeEndpointGiven reports whether a verb call already names where it goes, which is
// the condition get_to_from_path checks before deriving anything from the path
// (`return to if to || action`). A value this extractor cannot read — `to: redirect(...)`,
// `to: proc { ... }`, an `action:` naming a constant — still counts: Rails saw one, so
// the path is a path and not a handler.
func routeEndpointGiven(args *sitter.Node, src []byte) bool {
	return findPairValue(args, "to", src) != nil ||
		findPairValue(args, "action", src) != nil ||
		hashRocketRouteValue(args) != nil
}

// controllerOption reads a `controller:` option in either spelling and reports
// both the name and whether it escapes the enclosing module.
//
// A name beginning with "/" means something specific to Rails, which is why the
// raw text is inspected here rather than trimmed at each call site:
// add_controller_module strips the slash and returns the name UNCOMPOSED
// (`if controller&.start_with?("/") then -controller[1..-1]`), so
// `controller: "/admin/reports"` inside `namespace :api` is served by
// admin/reports and not by api/admin/reports. Trimming the slash where the
// option is read destroys the marker before the composition can see it.
func controllerOption(args *sitter.Node, src []byte) (string, bool, bool) {
	// pairStringPresent reports a symbol value as present-but-empty, since the
	// pair is there and holds no string, so the symbol spelling has to be tried
	// on an empty name rather than on an absent pair.
	raw, given := pairStringPresent(args, "controller", src)
	if raw == "" {
		if sym := pairSymbol(args, "controller", src); sym != "" {
			raw, given = sym, true
		}
	}
	if !given {
		return "", false, false
	}
	return strings.TrimPrefix(raw, "/"), strings.HasPrefix(raw, "/"), true
}

// composeController applies the module composition a controller name asks for:
// an absolute name — one written with a leading slash — is served exactly as
// written, and every other name takes the namespace in force at the route site.
func composeController(name, module string, absolute bool) string {
	if absolute {
		return name
	}
	return qualifyHandler(name, module)
}
