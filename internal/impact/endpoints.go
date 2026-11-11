package impact

import (
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Endpoints answers "which served endpoints does this change affect": the HTTP routes
// whose handler reaches the target. It is the line a reader of an impact report most
// wants, and the symbol walk cannot give it whole: that walk stops at max_depth and
// max_nodes, and a handler is usually several calls away.
//
// Three precisions, kept apart because they answer different questions:
//
//   - call: the handler reaches the target through calls (or by constructing or
//     injecting what it calls). That handler is affected. A call through an interface
//     method counts when the snapshot holds one implementation of it outside tests:
//     there is nothing else it could call.
//   - interface: the handler calls an interface method whose implementation reaches the
//     target, and that method has other implementations. A call through an interface
//     has no edge to its implementation, so the walk crosses from an implementing
//     method to the interface method it implements. Very likely affected; another
//     implementation could be the one called at run time.
//   - package: for a package target, the handler sits in a package that imports the
//     target's, directly or through others, and no path was found either way. The
//     package reaches the target; whether this handler does is not known. A served
//     route whose handler the extractor could not resolve to a symbol (a handler held
//     in a variable, such as one wrapped in middleware) is placed by the package it is
//     registered in, and says its handler is unresolved.

// Endpoint precisions, strongest first.
const (
	PrecisionCall      = "call"
	PrecisionInterface = "interface"
	PrecisionPackage   = "package"
)

// Endpoint is a served route a change reaches.
type Endpoint struct {
	Method  string `json:"method,omitempty"`
	Path    string `json:"path"`
	Handler string `json:"handler"`
	Repo    string `json:"repo,omitempty"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	// Precision is PrecisionCall, PrecisionInterface or PrecisionPackage.
	Precision string `json:"precision"`
	// Depth is how far the handler is from the target: calls for PrecisionCall, the
	// handler's package's import hops for PrecisionPackage.
	Depth int `json:"depth"`
	// Package is the handler's package, for PrecisionPackage.
	Package string `json:"package,omitempty"`
	// HandlerUnresolved marks a route whose handler is no symbol of the snapshot:
	// Handler is the expression it was registered with, and Package the package
	// registering it.
	HandlerUnresolved bool `json:"handler_unresolved,omitempty"`
}

// endpointRelations are the relations by which code reaches code. Declares and
// imports are left out: walked back from a package they reach every symbol of every
// package importing it, which would make a package-level answer look call-precise.
var endpointRelations = []string{facts.RelCalls, facts.RelInstantiates, facts.RelInjects}

// The endpoint walk's bounds: well above a real call depth, and a cap that only a
// graph-wide hub reaches, in which case the answer says it is incomplete.
const (
	maxEndpointDepth = 20
	maxEndpointNodes = 50000
)

// endpointsOf returns the served routes whose handlers reach seeds, and, when
// packages is non-nil (a package target: the package and its dependents with their
// hops), the routes whose handler's package is one of them. truncated reports a walk
// cut at its cap.
// hasServedRoutes reports whether store holds any route with a handler, so an empty
// endpoint list can say "none reach it" rather than "none known".
func hasServedRoutes(store *facts.Store) bool {
	for _, rt := range store.ByKind(facts.KindRoute) {
		for _, rel := range rt.Relations {
			if rel.Kind == facts.RelHandledBy {
				return true
			}
		}
		if unresolvedHandler(rt) != "" {
			return true
		}
	}
	return false
}

// unresolvedHandler returns the handler expression of a served route bound to no
// symbol, "" for any other route: one with a handler edge, a client call, or a
// navigation route.
func unresolvedHandler(rt facts.Fact) string {
	if rt.PropString(facts.PropRole) == facts.RoleClient {
		return ""
	}
	if t := rt.PropString("type"); t != "" && t != "route" {
		return ""
	}
	for _, rel := range rt.Relations {
		if rel.Kind == facts.RelHandledBy {
			return ""
		}
	}
	return rt.PropString("handler")
}

// reachers walks back from seeds over endpointRelations and returns every symbol that
// reaches them, with its distance.
func reachers(graph *facts.Graph, seeds []string, base int, into map[string]int) bool {
	reach, truncated := graph.ReverseReach(seeds, endpointRelations, maxEndpointDepth, maxEndpointNodes)
	for name, d := range reach {
		if old, ok := into[name]; !ok || base+d < old {
			into[name] = base + d
		}
	}
	return truncated
}

// InterfaceDispatch maps each implementing method outside test and mock code to the
// interface methods it implements, including a method an implementer promotes from
// a type it embeds: the hops a call through an interface can take.
func InterfaceDispatch(store *facts.Store, graph *facts.Graph) map[string][]string {
	impls, _ := interfaceMethods(store, graph)
	return impls
}

// interfaceMethods maps an implementing method ("pkg.T.M") to the interface methods
// it implements ("pkg.I.M"), from the implementers an interface records (Go) and from
// implements edges (languages that write them). An implementer's method may be one it
// promotes from a type it embeds (methodOf). Implementations in test code are left
// out: a test double is not what the served code calls.
//
// sole reports whether an interface method can call one implementation only. The
// recorded implementers cannot say so: the Go extractor records a type it infers by
// method set only where that is unambiguous, and none beside a stated one. So every
// type outside test and mock code whose methods cover the interface's method names
// counts; matching by name alone counts too many, never too few.
func interfaceMethods(store *facts.Store, graph *facts.Graph) (impls map[string][]string, sole func(string) bool) {
	implementers := map[string][]string{} // interface -> implementing types
	for _, f := range store.ByKind(facts.KindSymbol) {
		for _, prop := range []string{"implementers", "implementers_inferred"} {
			if types := stringList(f.PropAny(prop)); len(types) > 0 {
				implementers[f.Name] = append(implementers[f.Name], types...)
			}
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImplements {
				implementers[r.Target] = append(implementers[r.Target], f.Name)
			}
		}
	}
	impls = map[string][]string{}
	ifaceOf := map[string]string{} // interface method -> its interface
	for iface, types := range implementers {
		slices.Sort(types)
		types = slices.Compact(types)
		for _, e := range graph.ForwardEdges(iface) {
			if e.RelKind != facts.RelHasMethod || !strings.HasPrefix(e.Target, iface+".") {
				continue
			}
			m := e.Target
			method := m[len(iface)+1:]
			for _, t := range types {
				impl := methodOf(store, graph, t, method)
				if impl == "" || slices.Contains(impls[impl], m) {
					continue
				}
				impls[impl] = append(impls[impl], m)
				ifaceOf[m] = iface
			}
		}
	}
	var owners map[string]map[string]bool // built on the first question
	memo := map[string]bool{}
	sole = func(im string) bool {
		iface := ifaceOf[im]
		if v, ok := memo[iface]; ok {
			return v
		}
		if owners == nil {
			// Only the methods of interfaces an implementation reaches through.
			want := map[string]bool{}
			for m, i := range ifaceOf {
				want[m[len(i)+1:]] = true
			}
			owners = methodOwners(store, want)
		}
		var cover map[string]bool
		for _, e := range graph.ForwardEdges(iface) {
			if e.RelKind != facts.RelHasMethod || !strings.HasPrefix(e.Target, iface+".") {
				continue
			}
			types := owners[e.Target[len(iface)+1:]]
			if cover == nil {
				cover = maps.Clone(types)
				continue
			}
			maps.DeleteFunc(cover, func(t string, _ bool) bool { return !types[t] })
		}
		memo[iface] = len(cover) == 1
		return memo[iface]
	}
	return impls, sole
}

// maxEmbedDepth bounds the embedded types methodOf looks through.
const maxEmbedDepth = 4

// methodOf is the method a value of type t runs for name, outside test and mock code:
// t's own, or else one t promotes from a type it embeds (an implements edge), the
// shallowest first, as Go selects it. A type that implements an interface only
// through what it embeds, such as a struct composing one handler per endpoint, has no
// method of its own by that name. Two at the same depth are an ambiguous selector Go
// rejects, and "" is returned, as it is when there is none.
func methodOf(store *facts.Store, graph *facts.Graph, t, name string) string {
	frontier := []string{t}
	seen := map[string]bool{t: true}
	for depth := 0; depth <= maxEmbedDepth && len(frontier) > 0; depth++ {
		var found, next []string
		for _, typ := range frontier {
			if m := typ + "." + name; servedCode(store.LookupByExactName(m)) {
				found = append(found, m)
				continue
			}
			for _, e := range graph.ForwardEdges(typ) {
				if e.RelKind == facts.RelImplements && !seen[e.Target] {
					seen[e.Target] = true
					next = append(next, e.Target)
				}
			}
		}
		switch len(found) {
		case 0:
			frontier = next
		case 1:
			return found[0]
		default:
			return ""
		}
	}
	return ""
}

// methodOwners maps each wanted method name to the types declaring a method of that
// name, leaving out interfaces and test and mock code.
func methodOwners(store *facts.Store, want map[string]bool) map[string]map[string]bool {
	owners := map[string]map[string]bool{}
	for _, f := range store.ByKind(facts.KindSymbol) {
		i := strings.LastIndexAny(f.Name, ".#")
		if i <= 0 || !want[f.Name[i+1:]] ||
			f.PropString("symbol_kind") != facts.SymbolMethod || !servedCode([]facts.Fact{f}) || isInterface(store, f.Name[:i]) {
			continue
		}
		m := f.Name[i+1:]
		if owners[m] == nil {
			owners[m] = map[string]bool{}
		}
		owners[m][f.Name[:i]] = true
	}
	return owners
}

// isInterface reports whether name is an interface's symbol.
func isInterface(store *facts.Store, name string) bool {
	for _, f := range store.LookupByExactName(name) {
		if f.Kind == facts.KindSymbol && f.PropString("symbol_kind") == facts.SymbolInterface {
			return true
		}
	}
	return false
}

// servedCode reports whether a name is declared outside test and mock code.
func servedCode(ff []facts.Fact) bool {
	for _, f := range ff {
		if !facts.IsTestPath(f.File) && !mockPath(f.File) {
			return true
		}
	}
	return false
}

// mockPath reports a file generated mocks are conventionally written to.
func mockPath(file string) bool {
	dir, base := path.Split(file)
	return strings.Contains("/"+dir, "/mocks/") || strings.Contains("/"+dir, "/mock/") ||
		strings.HasPrefix(base, "mock_") || strings.HasSuffix(base, "_mock.go") || strings.HasSuffix(base, "_mocks.go")
}

// crossInterfaces extends reach, in place, across the interface methods its
// implementing methods implement, where cross allows it, and on to their callers,
// until nothing new is reached. It reports a walk cut at its cap.
func crossInterfaces(graph *facts.Graph, impls map[string][]string, cross func(string) bool, reach map[string]int) bool {
	if len(impls) == 0 {
		return false
	}
	truncated := false
	frontier := maps.Clone(reach)
	for round := 0; round < maxEndpointDepth && len(frontier) > 0; round++ {
		crossed := map[string]int{}
		for name, d := range frontier {
			for _, im := range impls[name] {
				if _, seen := reach[im]; seen || !cross(im) {
					continue
				}
				if old, ok := crossed[im]; !ok || d < old {
					crossed[im] = d
				}
			}
		}
		next := map[string]int{}
		for im, d := range crossed {
			reach[im] = d
			next[im] = d
		}
		for im, d := range crossed {
			reached := map[string]int{}
			if reachers(graph, []string{im}, d, reached) {
				truncated = true
			}
			for r, rd := range reached {
				if old, seen := reach[r]; !seen || rd < old {
					reach[r] = rd
					next[r] = rd
				}
			}
		}
		frontier = next
	}
	return truncated
}

func stringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func endpointsOf(store *facts.Store, graph *facts.Graph, seeds []string, packages map[string]int) (out []Endpoint, truncated bool) {
	impls, sole := interfaceMethods(store, graph)
	return endpointsReaching(store, graph, seeds, packages, impls, sole)
}

// EndpointFinder returns the served endpoints whose handlers reach any of seeds, by
// calls or through an interface, as impact lists them.
type EndpointFinder func(seeds []string) []Endpoint

// NewEndpointFinder is an EndpointFinder over one snapshot. The implementations
// behind each interface are worked out once, for every question it is asked.
func NewEndpointFinder(store *facts.Store, graph *facts.Graph) EndpointFinder {
	impls, sole := interfaceMethods(store, graph)
	return func(seeds []string) []Endpoint {
		out, _ := endpointsReaching(store, graph, seeds, nil, impls, sole)
		return out
	}
}

// endpointsReaching is endpointsOf with the interface implementations worked out.
func endpointsReaching(store *facts.Store, graph *facts.Graph, seeds []string, packages map[string]int, impls map[string][]string, sole func(string) bool) (out []Endpoint, truncated bool) {
	direct := map[string]int{}
	for _, s := range seeds {
		direct[s] = 0
	}
	truncated = reachers(graph, seeds, 0, direct)

	// Across interfaces: an implementing method that reaches the target makes the
	// interface method it implements reach it too, and that method's callers, until
	// nothing new is reached. An interface method with one implementation can call
	// nothing else, so its callers reach the target as surely as direct callers do.
	if crossInterfaces(graph, impls, sole, direct) {
		truncated = true
	}
	viaIface := map[string]int{}
	for k, v := range direct {
		viaIface[k] = v
	}
	if crossInterfaces(graph, impls, func(string) bool { return true }, viaIface) {
		truncated = true
	}
	seen := map[string]bool{}
	key := func(rt facts.Fact, handler string) string {
		return rt.Repo + "\x00" + rt.PropString("method") + "\x00" + rt.Name + "\x00" + handler
	}
	add := func(rt facts.Fact, handler, precision string, depth int, pkg string) bool {
		k := key(rt, handler)
		if seen[k] {
			return false
		}
		seen[k] = true
		out = append(out, Endpoint{
			Method: rt.PropString("method"), Path: rt.Name, Handler: handler,
			Repo: rt.Repo, File: rt.File, Line: rt.Line,
			Precision: precision, Depth: depth, Package: pkg,
		})
		return true
	}
	for h, d := range direct {
		for _, rt := range graph.ReverseFacts(h, facts.RelHandledBy) {
			if rt.Kind == facts.KindRoute {
				add(rt, h, PrecisionCall, d, "")
			}
		}
	}
	for h, d := range viaIface {
		if _, ok := direct[h]; ok {
			continue
		}
		for _, rt := range graph.ReverseFacts(h, facts.RelHandledBy) {
			if rt.Kind == facts.KindRoute {
				add(rt, h, PrecisionInterface, d, "")
			}
		}
	}
	if packages != nil {
		for _, rt := range store.ByKind(facts.KindRoute) {
			for _, rel := range rt.Relations {
				if rel.Kind != facts.RelHandledBy || seen[key(rt, rel.Target)] {
					continue
				}
				pkg := handlerPackage(store, rt, rel.Target)
				if d, ok := packages[pkg]; ok {
					add(rt, rel.Target, PrecisionPackage, d, pkg)
				}
			}
			// A handler held in a variable has no symbol to reach: the package
			// registering the route stands in for the handler's.
			if h := unresolvedHandler(rt); h != "" {
				pkg := handlerPackage(store, rt, "")
				if d, ok := packages[pkg]; ok && add(rt, h, PrecisionPackage, d, pkg) {
					out[len(out)-1].HandlerUnresolved = true
				}
			}
		}
	}
	out = dropCoveredMounts(out)
	rank := map[string]int{PrecisionCall: 0, PrecisionInterface: 1, PrecisionPackage: 2}
	slices.SortFunc(out, func(a, b Endpoint) int {
		if a.Precision != b.Precision {
			return rank[a.Precision] - rank[b.Precision]
		}
		if a.Depth != b.Depth {
			return a.Depth - b.Depth
		}
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if c := strings.Compare(a.Method, b.Method); c != 0 {
			return c
		}
		return strings.Compare(a.Handler, b.Handler)
	})
	return out, truncated
}

// dropCoveredMounts leaves out a mount listed by package alone, whose handler is no
// symbol (a router held in a variable, mounted at "/" or a prefix), when an endpoint
// under its path in the same repository is already listed as reached by calls or
// through an interface. The mount is how those requests arrive; listing it again
// with the weakest precision says nothing the precise entries do not.
func dropCoveredMounts(out []Endpoint) []Endpoint {
	covered := map[int]bool{}
	for i, m := range out {
		if !m.HandlerUnresolved || m.Precision != PrecisionPackage {
			continue
		}
		for _, e := range out {
			if e.Repo == m.Repo && e.Precision != PrecisionPackage && underPath(e.Path, m.Path) {
				covered[i] = true
				break
			}
		}
	}
	kept := out[:0]
	for i, e := range out {
		if !covered[i] {
			kept = append(kept, e)
		}
	}
	return kept
}

// underPath reports whether path is mount or lies under it, segment by segment:
// "/api/items" is under "/api" and "/", "/apix" is not under "/api".
func underPath(path, mount string) bool {
	mount = strings.TrimSuffix(mount, "/")
	return mount == "" || path == mount || strings.HasPrefix(path, mount+"/")
}

// handlerPackage is the package that declares a route's handler, or, when the handler
// is no symbol of the snapshot, the package the route is registered in.
func handlerPackage(store *facts.Store, route facts.Fact, handler string) string {
	for _, f := range store.LookupByExactName(handler) {
		if f.Kind != facts.KindSymbol || (route.Repo != "" && f.Repo != route.Repo) {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelDeclares {
				return r.Target
			}
		}
	}
	for _, r := range route.Relations {
		if r.Kind == facts.RelDeclares {
			return r.Target
		}
	}
	return ""
}
