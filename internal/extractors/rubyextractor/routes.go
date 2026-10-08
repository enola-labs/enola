package rubyextractor

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/extcoverage"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// extractAllRoutes finds and parses every Rails route file in the repository, each
// under the URL prefix the file is actually served at.
//
// A route file's prefix comes from whichever construct pulled it in, and there are
// three:
//
//   - `draw(:pkg)` from a parent file, usually inside a scope('/api')/namespace(:vN)
//     block. Parsed standalone, the delegated file loses that prefix and its routes
//     read as "/devices" instead of "/api/v2/devices", matching no client call.
//   - `mount SomeEngine, at: '/x'`, which serves an entire engine's config/routes.rb
//     below /x. Resolving the constant to its directory is what makes solidus's six
//     engines and discourse's plugin engines reachable at all.
//   - nothing — the application's own config/routes.rb, at the root prefix.
//
// So the parse is ordered: the application root first, because it declares the draws
// and mounts; then the files those name, seeded with the prefix they were named under;
// then anything left over at the root prefix, so a route file that is genuinely loaded
// by a mechanism not modelled here still contributes its routes rather than vanishing.
func extractAllRoutes(repoPath string, files []string) []facts.Fact {
	idx := indexRouteFiles(files)
	if len(idx.all) == 0 {
		return nil
	}

	readSrc := func(relFile string) ([]byte, bool) {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[ruby-extractor] error reading route file %s: %v", relFile, err)
			return nil, false
		}
		return src, true
	}

	// A jsonapi_resources declaration names its controller through the resource
	// class it refers to, so every declaration in the repository has to be known
	// before any handler is emitted. That is the only reason this walk reads the
	// route files twice.
	jsonapi := jsonapiContext{}
	jsonapi.format, jsonapi.refusalCause = jsonapiRouteFormat(repoPath, files)
	jsonapi.resolver = newJsonapiResolver(repoPath, files)
	for _, relFile := range idx.all {
		if src, ok := readSrc(relFile); ok {
			collectJsonapiDeclarations(src, jsonapi.resolver)
		}
	}

	var allFacts []facts.Fact
	unresolvedMacros := map[string]int{}
	parsed := map[string]bool{}
	// parse reads relFile under prefix, recording it as done. Returns what it learned so
	// the caller can follow the delegations and mounts it declares.
	parse := func(relFile, prefix string) (draws map[string]string, mounts []routeMount) {
		if parsed[relFile] {
			return nil, nil
		}
		parsed[relFile] = true
		src, ok := readSrc(relFile)
		if !ok {
			return nil, nil
		}
		ff, res := parseRouteFile(src, relFile, prefix, jsonapi)
		allFacts = append(allFacts, ff...)
		mergeCounts(unresolvedMacros, res.unresolved)
		return res.draws, res.mounts
	}

	// Pass 1: the application's own routes.rb, at the root prefix.
	var pendingDraws map[string]string
	var pendingMounts []routeMount
	if idx.appRoot != "" {
		pendingDraws, pendingMounts = parse(idx.appRoot, "")
	}

	// Pass 2: follow draw(:pkg) delegations transitively — a delegated file may itself
	// draw further files (GitLab's config/routes/directs/*.rb are drawn from
	// config/routes/directs.rb). Keys are resolved against every config/routes/<key>.rb
	// in the repository, not just the root one, because GitLab's `draw` override loads
	// the CE and EE copy of a name together.
	for len(pendingDraws) > 0 {
		next := map[string]string{}
		for _, key := range sortedDrawKeys(pendingDraws) {
			prefix := pendingDraws[key]
			for _, target := range idx.drawTargets[key] {
				draws, mounts := parse(target, prefix)
				for k, v := range draws {
					if _, seen := next[k]; !seen {
						next[k] = v
					}
				}
				pendingMounts = append(pendingMounts, mounts...)
			}
		}
		pendingDraws = next
	}

	// Pass 3: mounted engines. Resolve each mount's constant to the directory whose
	// config/routes.rb it owns, and parse that file under the mount path.
	constants := engineConstants(repoPath, idx, files)
	for _, m := range pendingMounts {
		dir, ok := constants[normalizeConstant(m.constant)]
		if !ok {
			continue
		}
		target, ok := idx.engineRoots[dir]
		if !ok {
			continue
		}
		draws, mounts := parse(target, m.prefix)
		// An engine may draw and mount in turn; one extra level covers the real cases
		// (an engine mounting a sub-engine) without an unbounded fixpoint.
		for _, key := range sortedDrawKeys(draws) {
			for _, t := range idx.drawTargets[key] {
				parse(t, draws[key])
			}
		}
		for _, sub := range mounts {
			if d, ok := constants[normalizeConstant(sub.constant)]; ok {
				if t, ok := idx.engineRoots[d]; ok {
					parse(t, sub.prefix)
				}
			}
		}
	}

	// Pass 4: everything not reached above, at the root prefix. An engine whose mount
	// site is in a file enola cannot see (a plugin loader, an initializer) still serves
	// its routes; recording them un-prefixed is closer to the truth than recording none.
	for _, relFile := range idx.all {
		parse(relFile, "")
	}

	if fact, ok := routeCoverageFact(repoPath, len(allFacts), unresolvedMacros); ok {
		allFacts = append(allFacts, fact)
	}
	return allFacts
}

// sortedDrawKeys returns m's keys in sorted order, so route emission is deterministic.
func sortedDrawKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mergeCounts(into, from map[string]int) {
	for name, n := range from {
		into[name] += n
	}
}

// routeCoverageFact accounts for the route macros this extractor read and the
// ones it could not, in the `edge_coverage` shape the cross-repo layer already
// uses — because coverage in this codebase travels as facts.
//
// It emits nothing when there were no route files at all: an extractor that had
// nothing to look at must not report a confident zero, which is the exact
// failure this fact exists to prevent one level down.
func routeCoverageFact(repoPath string, resolved int, unresolved map[string]int) (facts.Fact, bool) {
	return extcoverage.Fact(repoPath, "ruby:routes", "rails_route_macro", resolved, unresolved)
}

// associationCoverageFact accounts for the associations this extractor read and
// the ones whose target it could not name.
func associationCoverageFact(repoPath string, resolved int, unresolved map[string]int) (facts.Fact, bool) {
	return extcoverage.Fact(repoPath, "ruby:associations", "rails_association", resolved, unresolved)
}

// callCoverageFact accounts for call edges whose target names a symbol this
// extractor emitted, against those that name something it never did.
//
// 71% of the monolith's 218,263 call edges point at a name that is not a known
// symbol — 37,913 distinct names, led by `params`, `include` and `company`.
// Those are not all misses: many are Rails DSL or local variables that were
// never calls. But until now they vanished, and a vanished edge is
// indistinguishable from one that was never there, which is the shape this
// estate has spent a day removing everywhere else.
//
// The unresolved set is counted, not materialised. Turning 37,913 names into
// nodes would put `params` in the graph as a symbol, and a node nobody can
// follow is the same defect as an edge nobody can follow — the association ADR
// settled that once and this does not reopen it.
func callCoverageFact(repoPath string, resolved int, unresolved map[string]int) (facts.Fact, bool) {
	return extcoverage.Fact(repoPath, "ruby:calls", "ruby_call", resolved, unresolved)
}

// routeScope tracks the current route scope prefix.
type routeScope struct {
	pathPrefix string
	module     string
	// memberParam is the parent member path parameter (`:<singular>_id`) that nested
	// resources declared inside a *plural* `resources` block must nest under; empty for
	// namespace/scope/singular-resource scopes, which add no member id to their children.
	memberParam string
	// shallow records that an enclosing `resources` declared shallow: true.
	// Rails scopes the flag lexically to everything nested inside, so it has to
	// travel down the stack rather than be read off each call.
	shallow bool
	// controller is the controller that routes declared directly inside this scope are
	// served by, WITHOUT its namespace ("posts", not "api/v2/posts"). Set by
	// `resources`/`resource`, by an explicit `controller:` option and by the
	// `controller ... do` block form; empty for namespace/scope, which only contribute
	// a module segment. This is what lets `get :status, on: :member` inside a resources
	// block resolve to the same controller as the resource itself.
	//
	// It is stored bare because Rails composes the namespace where the ROUTE is created
	// rather than where the resource is declared, so a `scope module:` entered in
	// between belongs to the routes after it and not to the declaration.
	controller string
	// controllerAbsolute records that `controller` was written with a leading slash,
	// which Rails strips while declining to compose the module onto it.
	controllerAbsolute bool
	// controllerUnknown marks a scope that owns a controller this extractor could not
	// name — a singular `resource` with no explicit `controller:`, whose name only
	// ActiveSupport's inflector can pluralize correctly. It is not the same as having
	// no controller: routes inside such a block must decline too rather than inherit
	// the controller of an enclosing resource, which serves different routes.
	controllerUnknown bool
	// singularOwner marks a scope pushed by `resource` rather than `resources`.
	// A singular resource has no member id, so `on: :member` inside it addresses
	// the bare path — Rails serves /profile/settings, not /profile/:id/settings.
	singularOwner bool
	// ownParam is what this resource calls its own member segment — `:id` unless
	// the declaration renamed it with `param:`. It is what `on: :member` and a
	// `member` block address, which is a different question from memberParam:
	// that one is what CHILDREN nest under.
	ownParam string
	// dropParentMember marks a scope that suppresses the enclosing resource's
	// member param: `member`/`collection` blocks address the resource itself
	// rather than something nested under it.
	dropParentMember bool
	// resourceMethodScope marks a `member`/`collection` block: Rails' scope level
	// there is a resource method scope, so a canonical action declared by symbol
	// (`get :index`) is served at the block's own path, not at /index.
	resourceMethodScope bool
}

// canonicalResourceActions are the actions Rails' path_for_action serves at the
// scope's own path inside a resource method scope (CANONICAL_ACTIONS).
var canonicalResourceActions = map[string]bool{
	"index": true, "create": true, "new": true, "show": true, "update": true, "destroy": true,
}

// buildPrefix constructs the current URL prefix from the scope stack,
// materializing each resource's member param as it goes.
//
// The member param has to live here rather than be read off the innermost
// scope by each caller, because anything may sit between a parent resource and
// what it nests: `resources :companies do namespace :api do resources :sections
// end end` is served at /companies/:company_id/api/sections. Reading the param
// from the top of the stack loses it the moment a namespace or scope
// intervenes — which is the shape a real monolith is full of.
func buildPrefix(stack []routeScope) string {
	var b strings.Builder
	for i, s := range stack {
		b.WriteString(s.pathPrefix)
		if s.memberParam == "" {
			continue
		}
		if i+1 < len(stack) && stack[i+1].dropParentMember {
			continue
		}
		b.WriteString("/:" + s.memberParam)
	}
	return b.String()
}

// collectionPrefix is the path of the innermost resource itself rather than of
// something nested under it: what `on: :collection` and a `collection` block
// address.
func collectionPrefix(stack []routeScope) string {
	if len(stack) == 0 {
		return ""
	}
	trimmed := append([]routeScope{}, stack...)
	trimmed[len(trimmed)-1].memberParam = ""
	return buildPrefix(trimmed)
}

// actionFilter is an only:/except: declaration, carrying whether each was
// WRITTEN as well as what it named. An empty declaration is not an absent one.
type actionFilter struct {
	only        map[string]bool
	except      map[string]bool
	onlyGiven   bool
	exceptGiven bool
}

// apply narrows a resource's action set. only: wins over except: when both are
// written, matching Rails.
func (f actionFilter) apply(all []restAction) []restAction {
	if f.onlyGiven {
		return filterActions(all, f.only, true)
	}
	if f.exceptGiven {
		return filterActions(all, f.except, false)
	}
	return all
}

// memberSegment is what `on: :member` and a `member` block add to the innermost
// resource's path: its member param, or nothing at all when that resource is
// singular.
func memberSegment(stack []routeScope) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].ownParam == "" && !stack[i].singularOwner {
			continue
		}
		if stack[i].singularOwner {
			return ""
		}
		return "/:" + stack[i].ownParam
	}
	return "/:id"
}

// buildModule joins the controller-namespace segments contributed by the scope stack
// ("api", "v2" -> "api/v2"). `namespace :api` and `scope module: :api` both add one;
// a plain `scope '/api'` adds none, because it changes the URL and not the controller
// lookup.
func buildModule(stack []routeScope) string {
	var parts []string
	for _, s := range stack {
		if s.module != "" {
			parts = append(parts, s.module)
		}
	}
	return strings.Join(parts, "/")
}

// currentController returns the innermost controller a route site inherits, and
// whether that name escapes the module composition.
//
// A scope that owns a controller this extractor could not name stops the search
// instead of letting it continue outward. `resources :companies do resource
// :profile do get :settings end end` is served by profiles#settings, and
// companies is not a second-best answer to that question but a different
// controller — one that exists, which is what makes the wrong answer unreadable
// as an error.
func currentController(stack []routeScope) (string, bool) {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].controllerUnknown {
			return "", false
		}
		if stack[i].controller != "" {
			return stack[i].controller, stack[i].controllerAbsolute
		}
	}
	return "", false
}

// controllerSymbol converts a Rails handler string ("api/v2/posts#index") into the
// fully-qualified Ruby symbol the ruby AST walker emits for that action
// ("Api::V2::PostsController#index"), so a route can carry a handled_by relation to a
// node that actually exists in the graph.
//
// Returns "" when the handler names no action (a bare "posts") or points at something
// that is not a controller method — a redirect, a Rack app, a lambda — because a
// relation to a node that will never exist is worse than none.
func controllerSymbol(handler string) string {
	path, action, ok := strings.Cut(handler, "#")
	if !ok || path == "" || action == "" {
		return ""
	}
	// A `to:` value can be a full class path already ("Api::V2::PostsController#index")
	// or a Rack-style constant; leave those alone rather than re-camelizing them.
	if strings.Contains(path, "::") {
		if !strings.HasSuffix(path, "Controller") {
			return ""
		}
		return path + "#" + action
	}
	if strings.ContainsAny(path, " /()#") && !isControllerPath(path) {
		return ""
	}
	var segs []string
	for _, seg := range strings.Split(path, "/") {
		if seg == "" {
			continue
		}
		segs = append(segs, snakeToCamel(seg))
	}
	if len(segs) == 0 {
		return ""
	}
	return strings.Join(segs, "::") + "Controller#" + action
}

// firstPositionalName returns the first positional argument of a verb call when it
// names a single action — `get :status` or `get 'status'` — and "" when it is a path
// with segments or params (`get 'reports/:id/export'`), where the action cannot be
// derived from the path alone.
func firstPositionalName(args *sitter.Node, src []byte) string {
	p := firstPositionalPath(args, src)
	if p == "" || !isControllerPath(strings.TrimPrefix(p, "/")) {
		return ""
	}
	return strings.TrimPrefix(p, "/")
}

// isControllerPath reports whether s is a plain Rails controller path — lowercase
// identifiers separated by slashes, nothing else.
func isControllerPath(s string) bool {
	if s == "" {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" {
			return false
		}
		for _, r := range seg {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
				return false
			}
		}
	}
	return true
}

// restAction describes a single RESTful action.
type restAction struct {
	name   string
	method string
	suffix string
}

// restfulActions returns the set of REST actions for a resources declaration,
// honoring only:/except: filters parsed from the declaration's arguments.
func restfulActions(filter actionFilter) []restAction {
	all := []restAction{
		{name: "index", method: "GET", suffix: ""},
		{name: "create", method: "POST", suffix: ""},
		{name: "new", method: "GET", suffix: "/new"},
		{name: "show", method: "GET", suffix: "/:id"},
		// Rails routes BOTH PATCH and PUT to the update action, so emit both — a
		// client calling either verb must resolve to the same served endpoint.
		{name: "update", method: "PATCH", suffix: "/:id"},
		{name: "update", method: "PUT", suffix: "/:id"},
		{name: "edit", method: "GET", suffix: "/:id/edit"},
		{name: "destroy", method: "DELETE", suffix: "/:id"},
	}

	return filter.apply(all)
}

// restfulActionsSingular returns the REST actions for a singular `resource`
// declaration. A singular resource has no index and no `:id` member segment — every
// action acts on the single resource at its base path.
func restfulActionsSingular(filter actionFilter) []restAction {
	all := []restAction{
		{name: "create", method: "POST", suffix: ""},
		{name: "new", method: "GET", suffix: "/new"},
		{name: "show", method: "GET", suffix: ""},
		// Rails routes BOTH PATCH and PUT to update (see restfulActions).
		{name: "update", method: "PATCH", suffix: ""},
		{name: "update", method: "PUT", suffix: ""},
		{name: "edit", method: "GET", suffix: "/edit"},
		{name: "destroy", method: "DELETE", suffix: ""},
	}

	return filter.apply(all)
}

// JSONAPI::Resources rewrites each resource's URL segment through a route
// formatter, so `jsonapi_resources :career_sites` serves /career-sites rather
// than /career_sites. The formatter is repo configuration, and a repo that
// installs its own is running Ruby this extractor cannot read.
const (
	jsonapiRouteDasherized  = "dasherized"
	jsonapiRouteUnderscored = "underscored"
	jsonapiRouteUnknown     = "unknown"
)

var (
	jsonapiRouteFormatPattern = regexp.MustCompile(`route_format\s*=\s*(\S+)`)
	formatterDeclaration      = regexp.MustCompile(`class\s+(\w*)RouteFormatter\s*<\s*([\w:]+)`)
	formatOverride            = regexp.MustCompile(`(?m)^\s*def\s+(self\.)?format\b`)
)

// Refusal causes. A count with no cause reads as "this extractor cannot expand
// jsonapi macros", which was true until today and is now false everywhere
// except the repositories that configure their own formatter. Naming what was
// found turns a suspected extractor limitation into a located line of
// configuration.
const (
	refusalFormatterOverrides = "route_formatter_overrides_format"
	refusalFormatterUnknown   = "route_formatter_unknown"
)

// jsonapiContext is what a jsonapi_resources declaration needs before it can be
// expanded: the repository's route-segment format, the located cause when that
// format could not be read, and the resolver from a declaration to its resource
// class. It travels as one value because all three are properties of the
// repository rather than of the file being parsed.
type jsonapiContext struct {
	format       string
	refusalCause string
	resolver     *jsonapiResolver
}

// jsonapiRouteFormat reports how a repository formats JSONAPI::Resources route
// segments. An unconfigured repository gets the gem's own default, which is
// dasherized; a repository naming a formatter this extractor does not recognise
// gets jsonapiRouteUnknown, on which the declarations are counted rather than
// expanded — the segment is unknowable without running the formatter, and a
// plausible-looking wrong path is worse than a counted miss.
func jsonapiRouteFormat(repoPath string, files []string) (string, string) {
	for _, relFile := range files {
		if factpath.Dir(relFile) != "config/initializers" || !isRubyFile(relFile) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			continue
		}
		m := jsonapiRouteFormatPattern.FindSubmatch(src)
		if m == nil {
			continue
		}
		switch strings.Trim(string(m[1]), `":`) {
		case "dasherized_route", "Dasherized":
			return jsonapiRouteDasherized, ""
		case "underscored_route", "Underscored":
			return jsonapiRouteUnderscored, ""
		}
		return classifyFormatter(src)
	}
	return jsonapiRouteDasherized, ""
}

// classifyFormatter reads what the repository's own formatter class says about
// itself. A subclass that never overrides the formatting method formats exactly
// as its parent does — that is what inheritance means, and reading the class
// body is reading the source rather than reasoning about it. A subclass that
// does override is running Ruby this extractor cannot, and no amount of reading
// the override changes that: one measured application's is provably harmless
// for resource segments and there is nothing in that repository able to check
// the result.
func classifyFormatter(src []byte) (string, string) {
	declaration := formatterDeclaration.FindSubmatch(src)
	if declaration == nil {
		return jsonapiRouteUnknown, refusalFormatterUnknown
	}
	parent := parentFormatterFormat(string(declaration[2]))
	if parent == "" {
		return jsonapiRouteUnknown, refusalFormatterUnknown
	}
	if formatOverride.Match(src) {
		return jsonapiRouteUnknown, refusalFormatterOverrides
	}
	return parent, ""
}

func parentFormatterFormat(superclass string) string {
	switch strings.TrimPrefix(superclass, "::") {
	case "DasherizedRouteFormatter", "JSONAPI::DasherizedRouteFormatter":
		return jsonapiRouteDasherized
	case "UnderscoredRouteFormatter", "JSONAPI::UnderscoredRouteFormatter":
		return jsonapiRouteUnderscored
	}
	return ""
}

// jsonapiSegment formats a resource name as the URL segment the gem serves it
// under.
func jsonapiSegment(name, format string) string {
	if format == jsonapiRouteUnderscored {
		return name
	}
	return strings.ReplaceAll(name, "_", "-")
}

// jsonapiRestfulActions returns the REST actions a `jsonapi_resources`
// declaration serves. JSONAPI::Resources is an API-only gem: it serves no `new`
// and no `edit`, since both exist in Rails only to render HTML forms. Reusing
// restfulActions here would fabricate two routes per declaration.
func jsonapiRestfulActions(filter actionFilter) []restAction {
	all := []restAction{
		{name: "index", method: "GET", suffix: ""},
		{name: "create", method: "POST", suffix: ""},
		{name: "show", method: "GET", suffix: "/:id"},
		// PATCH and PUT both reach update, as in restfulActions.
		{name: "update", method: "PATCH", suffix: "/:id"},
		{name: "update", method: "PUT", suffix: "/:id"},
		{name: "destroy", method: "DELETE", suffix: "/:id"},
	}

	return filter.apply(all)
}

// jsonapiRestfulActionsSingular returns the REST actions a singular
// `jsonapi_resource` declaration serves: no index, no `:id` member segment, and
// — as with the plural form — no new and no edit.
func jsonapiRestfulActionsSingular(filter actionFilter) []restAction {
	all := []restAction{
		{name: "create", method: "POST", suffix: ""},
		{name: "show", method: "GET", suffix: ""},
		{name: "update", method: "PATCH", suffix: ""},
		{name: "update", method: "PUT", suffix: ""},
		{name: "destroy", method: "DELETE", suffix: ""},
	}

	return filter.apply(all)
}

// filterActions returns actions filtered by an allow or deny list.
func filterActions(all []restAction, names map[string]bool, isAllow bool) []restAction {
	var result []restAction
	for _, a := range all {
		if isAllow {
			if names[a.name] {
				result = append(result, a)
			}
		} else {
			if !names[a.name] {
				result = append(result, a)
			}
		}
	}
	return result
}
