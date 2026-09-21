package facts

import (
	"fmt"
	"sort"
	"strings"
)

// modelSearchDepth is how far past the controller the walk looks for a model.
// Three hops covers controller -> resource -> model and controller -> service ->
// query -> model, which are the shapes this estate has; beyond that the answer
// stops being about the endpoint.
const modelSearchDepth = 3

// maxAssociated caps the association hop. One hop out of a hub is most of the
// schema — Company is pointed at by 185 models on the monolith — and a list
// that long is not an answer to a question about one endpoint.
const maxAssociated = 25

// EndpointImpact answers the question the route work was pitched on: what does
// changing this HTTP endpoint reach?
//
// Every link in the chain already exists as a fact — a route names a handler,
// a handler names a controller class, a controller's forward dependencies reach
// model classes, and a model's associations name other models — and nothing
// traversed it. Impact analysis takes a symbol, so an endpoint was not a thing
// you could ask about.
//
// Each hop is reported separately, and each may be empty. A route whose
// controller does not resolve still reports the route; a controller that
// reaches no model still reports the controller. Saying which hop ran out is
// the difference between "this endpoint touches nothing" and "I stopped here".
type EndpointImpact struct {
	Query string `json:"query"`
	// Routes are the endpoints the query matched.
	Routes []EndpointRoute `json:"routes"`
	// Controllers are the classes serving them, where the class resolves.
	Controllers []string `json:"controllers,omitempty"`
	// Models are the model classes those controllers reach.
	Models []string `json:"models,omitempty"`
	// Associated are models reachable in one association hop from Models. A hub
	// makes this most of the schema, so the list is capped and the total is
	// reported beside it.
	Associated      []string `json:"associated_models,omitempty"`
	AssociatedTotal int      `json:"associated_total,omitempty"`
	// Tables are the physical tables behind Models and Associated.
	Tables []string `json:"tables,omitempty"`
	// Callers are the client call sites that hit this endpoint, and the frontend
	// screens they belong to. This is the other half of the blast radius: the
	// models say what the endpoint writes, the callers say who notices.
	Callers []EndpointCaller `json:"callers,omitempty"`
	// StoppedAt names the first hop that produced nothing, so an empty result
	// reads as a boundary rather than as an answer.
	StoppedAt string `json:"stopped_at,omitempty"`
	Summary   string `json:"summary"`
}

// EndpointCaller is one place that calls the endpoint.
type EndpointCaller struct {
	File string `json:"file"`
	// Screen is the frontend route the calling file implements, when the file is
	// a route module and a router declaration matches its name. Empty for a
	// component or a service, which many screens may share.
	Screen string `json:"screen,omitempty"`
}

// EndpointRoute is one matched endpoint.
type EndpointRoute struct {
	Method  string `json:"method,omitempty"`
	Path    string `json:"path"`
	Handler string `json:"handler,omitempty"`
	File    string `json:"file,omitempty"`
}

// ControllerKey reduces a controller path and a class name to one spelling, so
// `connect/onboarding_dashboards` and `Connect::OnboardingDashboardsController`
// compare equal.
func ControllerKey(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "::", "/")
	name = strings.TrimSuffix(name, "controller")
	name = strings.TrimSuffix(name, "_")
	return strings.ReplaceAll(name, "_", "")
}

// CallerFinder returns the client call sites that reach any of the given server
// routes.
//
// It is a parameter rather than a method on the store because the answer must be the
// one the cross-repo linker gives, and the linker's matching rules (suffixes, format
// extensions, base-relative paths, verbs, parameter matching, service aliases) live
// above this package. This package used to re-derive the join by path suffix, and a
// call the linker had linked could then be missing from an endpoint's callers.
type CallerFinder func(servers []Fact) []Fact

// AnalyzeEndpoint walks route -> controller -> model -> associated model.
// maxRoutes caps how many matched endpoints are followed, because a bare prefix
// can match hundreds and following all of them answers a different question.
// findCallers answers who calls the followed routes; nil reports no callers.
func (s *Store) AnalyzeEndpoint(query string, maxRoutes int, findCallers CallerFinder) EndpointImpact {
	if maxRoutes <= 0 {
		maxRoutes = 25
	}
	out := EndpointImpact{Query: query}
	needle := strings.ToLower(strings.TrimSpace(query))
	method := ""
	if verb, rest, found := strings.Cut(needle, " "); found {
		switch strings.ToUpper(verb) {
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
			method, needle = strings.ToUpper(verb), strings.TrimSpace(rest)
		}
	}

	// Each matched route keeps its fact, so the callers are asked for exactly the routes
	// the answer follows, after sorting and the cap.
	type matchedRoute struct {
		route EndpointRoute
		fact  Fact
	}
	var matched []matchedRoute
	for _, fact := range s.ByKind(KindRoute) {
		if role, _ := fact.PropAny("role").(string); role == "client" {
			continue
		}
		if isDouble, _ := fact.PropAny("test_double").(bool); isDouble {
			continue
		}
		factMethod, _ := fact.PropAny("method").(string)
		if method != "" && !strings.EqualFold(factMethod, method) {
			continue
		}
		if !strings.Contains(strings.ToLower(fact.Name), needle) {
			continue
		}
		handler, _ := fact.PropAny("handler").(string)
		matched = append(matched, matchedRoute{
			route: EndpointRoute{Method: factMethod, Path: fact.Name, Handler: handler, File: fact.File},
			fact:  fact,
		})
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].route.Path != matched[j].route.Path {
			return matched[i].route.Path < matched[j].route.Path
		}
		return matched[i].route.Method < matched[j].route.Method
	})
	if len(matched) == 0 {
		out.StoppedAt = "route"
		out.Summary = fmt.Sprintf("No server route matches %q.", query)
		return out
	}
	truncated := false
	if len(matched) > maxRoutes {
		matched, truncated = matched[:maxRoutes], true
	}
	servers := make([]Fact, 0, len(matched))
	for _, m := range matched {
		out.Routes = append(out.Routes, m.route)
		servers = append(servers, m.fact)
	}

	// Who calls this is independent of how far the model chain gets. Computing
	// it after the chain meant every endpoint whose controller or model did not
	// resolve reported no callers at all — an answer about the frontend withheld
	// because of a gap in the backend walk.
	if findCallers != nil {
		out.Callers = s.callersFrom(findCallers(servers))
	}

	// Hop 2: the controller class each handler names.
	classes := map[string]string{}
	for _, fact := range s.ByKind(KindSymbol) {
		if kind, _ := fact.PropAny("symbol_kind").(string); kind != SymbolClass {
			continue
		}
		classes[ControllerKey(fact.Name)] = fact.Name
	}
	controllers := map[string]bool{}
	for _, route := range out.Routes {
		controller, _, found := strings.Cut(route.Handler, "#")
		if !found || controller == "" {
			continue
		}
		if resolved, ok := classes[ControllerKey(controller)]; ok {
			controllers[resolved] = true
		}
	}
	out.Controllers = sortedKeys(controllers)
	if len(out.Controllers) == 0 {
		out.StoppedAt = "controller"
		out.Summary = summarize(out, truncated)
		return out
	}

	// Hop 3: the model classes those controllers reach. A model is a class with
	// a storage fact, which is how this graph already says "this is a model".
	models := map[string]string{}
	for _, fact := range s.ByKind(KindStorage) {
		if kind, _ := fact.PropAny("storage_kind").(string); kind != "model" {
			continue
		}
		table, _ := fact.PropAny("table").(string)
		models[fact.Name] = table
	}
	// A controller rarely names a model directly. A JSONAPI controller is a
	// three-line subclass and the model is reached through its resource class; a
	// plain Rails controller often goes through a service or a query object. One
	// hop found nothing at all on the monolith, which is the traversal working
	// and the depth being wrong.
	graph := s.Graph()
	reached := map[string]bool{}
	if graph != nil {
		// The call edges hang off the controller's METHODS, not off the class: a
		// class fact carries only `declares` and `implements`, while
		// `Controller#index` carries everything it calls. Walking from the class
		// alone found nothing on the monolith.
		seen := map[string]bool{}
		frontier := append([]string{}, out.Controllers...)
		for _, controller := range out.Controllers {
			frontier = append(frontier, s.methodsOf(controller)...)
		}
		// A JSONAPI controller declares no methods at all — it inherits them — and
		// its model is reached through the resource class. The class is located by
		// name and only used when it exists, so this is a convention checked
		// rather than a convention assumed.
		for _, resource := range s.resourceClassesFor(out.Routes, classes) {
			frontier = append(frontier, resource)
			frontier = append(frontier, s.methodsOf(resource)...)
		}
		for depth := 0; depth < modelSearchDepth && len(frontier) > 0; depth++ {
			var next []string
			for _, node := range frontier {
				for _, edge := range graph.ForwardEdges(node) {
					if seen[edge.Target] {
						continue
					}
					seen[edge.Target] = true
					if _, isModel := models[edge.Target]; isModel {
						reached[edge.Target] = true
						continue
					}
					next = append(next, edge.Target)
				}
			}
			frontier = next
		}
	}
	out.Models = sortedKeys(reached)
	if len(out.Models) == 0 {
		out.StoppedAt = "model"
		out.Summary = summarize(out, truncated)
		return out
	}

	// Hop 4: one association hop out from those models.
	associated := map[string]bool{}
	for _, fact := range s.ByKind(KindAssociation) {
		owner, _ := fact.PropAny("model").(string)
		target, _ := fact.PropAny("target").(string)
		if target == "" || !reached[owner] || reached[target] {
			continue
		}
		associated[target] = true
	}
	out.Associated = sortedKeys(associated)
	out.AssociatedTotal = len(out.Associated)
	if len(out.Associated) > maxAssociated {
		out.Associated = out.Associated[:maxAssociated]
	}

	tables := map[string]bool{}
	for name := range reached {
		if table := models[name]; table != "" {
			tables[table] = true
		}
	}
	for name := range associated {
		if table := models[name]; table != "" {
			tables[table] = true
		}
	}
	out.Tables = sortedKeys(tables)
	out.Summary = summarize(out, truncated)
	return out
}

// callersFrom turns the client call sites a CallerFinder returned into callers: one per
// file, mocks excluded, each with the frontend screen it belongs to where the file is a
// route module.
//
// A mock server calling an endpoint is not a caller that notices a change, whatever the
// matching says. The screen comes from the Ember convention that app/routes/<name>
// implements the route declared as <name>, checked against the router's own
// declarations rather than assumed.
func (s *Store) callersFrom(clients []Fact) []EndpointCaller {
	// Ember route NAMES are what the file layout mirrors, and they are not the
	// URL: `this.route("admin-company-linking", { path: "/admin/company-linking" })`
	// is served at one and implemented at app/routes/admin-company-linking.
	// Matching on the path's last segment finds nothing whenever a route
	// overrides its path, which is most of the interesting ones.
	screens := map[string]string{}
	for _, fact := range s.ByKind(KindRoute) {
		if framework, _ := fact.PropAny("framework").(string); framework != "ember" {
			continue
		}
		name, _ := fact.PropAny("ember_route_name").(string)
		if name == "" {
			continue
		}
		screens[strings.ReplaceAll(name, ".", "/")] = fact.Name
	}

	seen := map[string]bool{}
	var out []EndpointCaller
	for _, fact := range clients {
		if role, _ := fact.PropAny("role").(string); role != "client" {
			continue
		}
		if isDouble, _ := fact.PropAny("test_double").(bool); isDouble {
			continue
		}
		if seen[fact.File] {
			continue
		}
		seen[fact.File] = true
		out = append(out, EndpointCaller{File: fact.File, Screen: screenFor(fact.File, screens)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// screenFor maps a route-module file to the router declaration it implements.
func screenFor(file string, screens map[string]string) string {
	slash := strings.ReplaceAll(file, "\\", "/")
	idx := strings.Index(slash, "/routes/")
	if idx < 0 {
		return ""
	}
	name := slash[idx+len("/routes/"):]
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		name = name[:dot]
	}
	if declared, ok := screens[name]; ok {
		return declared
	}
	return ""
}

func summarize(out EndpointImpact, truncated bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d route(s)", len(out.Routes))
	if truncated {
		b.WriteString(" (capped)")
	}
	fmt.Fprintf(&b, " -> %d controller(s) -> %d model(s)", len(out.Controllers), len(out.Models))
	if out.AssociatedTotal > 0 {
		fmt.Fprintf(&b, " -> %d associated model(s)", out.AssociatedTotal)
		if out.AssociatedTotal > len(out.Associated) {
			fmt.Fprintf(&b, " (%d shown)", len(out.Associated))
		}
	}
	if len(out.Tables) > 0 {
		fmt.Fprintf(&b, ", touching %d table(s)", len(out.Tables))
	}
	b.WriteString(".")
	if len(out.Callers) > 0 {
		screens := 0
		for _, caller := range out.Callers {
			if caller.Screen != "" {
				screens++
			}
		}
		fmt.Fprintf(&b, " Called from %d file(s)", len(out.Callers))
		if screens > 0 {
			fmt.Fprintf(&b, ", %d of them a frontend screen", screens)
		}
		b.WriteString(".")
	}
	if out.AssociatedTotal > maxAssociated {
		b.WriteString(" That association count is dominated by a hub model that most of the schema points" +
			" at, so read it as reach rather than as a list of things this endpoint changes.")
	}
	switch out.StoppedAt {
	case "controller":
		b.WriteString(" The chain stops at the controller: no class in this repository answers to the" +
			" handler these routes name, so what they reach is unknown rather than nothing.")
	case "model":
		b.WriteString(" The chain stops at the controller's dependencies: it reaches no class carrying a" +
			" storage fact, so either it touches no model directly or the call is one this extractor" +
			" does not resolve.")
	}
	return b.String()
}

// resourceClassesFor finds the JSONAPI resource class a route's declaration
// implies, when such a class exists.
func (s *Store) resourceClassesFor(routes []EndpointRoute, classes map[string]string) []string {
	found := map[string]bool{}
	for _, route := range routes {
		controller, _, ok := strings.Cut(route.Handler, "#")
		if !ok || controller == "" {
			continue
		}
		namespace, last := "", controller
		if idx := strings.LastIndex(controller, "/"); idx >= 0 {
			namespace, last = controller[:idx+1], controller[idx+1:]
		}
		for _, singular := range singularCandidates(last) {
			key := ControllerKey(namespace + singular + "_resource")
			if resolved, ok := classes[key]; ok {
				found[resolved] = true
				break
			}
		}
	}
	return sortedKeys(found)
}

// singularCandidates offers the spellings a plural controller name might
// singularize to. Only one is ever used, and only if a class answers to it —
// this repository does not grow an English inflector, it checks.
func singularCandidates(plural string) []string {
	out := []string{plural}
	switch {
	case strings.HasSuffix(plural, "ies"):
		out = append(out, strings.TrimSuffix(plural, "ies")+"y")
	case strings.HasSuffix(plural, "sses"), strings.HasSuffix(plural, "shes"), strings.HasSuffix(plural, "ches"):
		out = append(out, strings.TrimSuffix(plural, "es"))
	case strings.HasSuffix(plural, "s") && !strings.HasSuffix(plural, "ss"):
		out = append(out, strings.TrimSuffix(plural, "s"))
	}
	return out
}

// methodsOf returns the `Class#method` symbols declared on a class.
func (s *Store) methodsOf(class string) []string {
	prefix := class + "#"
	var out []string
	for _, fact := range s.ByKind(KindSymbol) {
		if strings.HasPrefix(fact.Name, prefix) {
			out = append(out, fact.Name)
		}
	}
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
