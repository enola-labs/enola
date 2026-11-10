package impact

import (
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Endpoints answers "which served endpoints does this change affect": the HTTP routes
// whose handler reaches the target. It is the line a reader of an impact report most
// wants, and the symbol walk cannot give it whole: that walk stops at max_depth and
// max_nodes, and a handler is usually several calls away.
//
// Two precisions, kept apart because they answer different questions:
//
//   - call: the handler reaches the target through calls (or by constructing or
//     injecting what it calls). That handler is affected.
//   - interface: the handler calls an interface method whose implementation reaches the
//     target. A call through an interface has no edge to its implementation, so the
//     walk crosses from an implementing method to the interface method it implements.
//     Very likely affected; another implementation could be the one called at run time.
//   - package: for a package target, the handler sits in a package that imports the
//     target's, directly or through others, and no path was found either way. The
//     package reaches the target; whether this handler does is not known.

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
	// Precision is PrecisionCall or PrecisionPackage.
	Precision string `json:"precision"`
	// Depth is how far the handler is from the target: calls for PrecisionCall, the
	// handler's package's import hops for PrecisionPackage.
	Depth int `json:"depth"`
	// Package is the handler's package, for PrecisionPackage.
	Package string `json:"package,omitempty"`
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
	}
	return false
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

// interfaceMethods maps an implementing method ("pkg.T.M") to the interface methods
// it implements ("pkg.I.M"), from the implementers an interface records (Go) and from
// implements edges (languages that write them).
func interfaceMethods(store *facts.Store, graph *facts.Graph) map[string][]string {
	implementers := map[string][]string{} // interface -> implementing types
	for _, f := range store.ByKind(facts.KindSymbol) {
		for _, prop := range []string{"implementers", "implementers_inferred"} {
			implementers[f.Name] = append(implementers[f.Name], stringList(f.PropAny(prop))...)
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImplements {
				implementers[r.Target] = append(implementers[r.Target], f.Name)
			}
		}
	}
	out := map[string][]string{}
	for iface, types := range implementers {
		for _, e := range graph.ForwardEdges(iface) {
			if e.RelKind != facts.RelHasMethod || !strings.HasPrefix(e.Target, iface+".") {
				continue
			}
			m := e.Target
			method := m[len(iface)+1:]
			for _, t := range types {
				impl := t + "." + method
				if len(store.LookupByExactName(impl)) > 0 {
					out[impl] = append(out[impl], m)
				}
			}
		}
	}
	return out
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
	direct := map[string]int{}
	for _, s := range seeds {
		direct[s] = 0
	}
	truncated = reachers(graph, seeds, 0, direct)

	// Across interfaces: an implementing method that reaches the target makes the
	// interface method it implements reach it too, and that method's callers, until
	// nothing new is reached.
	viaIface := map[string]int{}
	for k, v := range direct {
		viaIface[k] = v
	}
	if impls := interfaceMethods(store, graph); len(impls) > 0 {
		frontier := viaIface
		for round := 0; round < maxEndpointDepth && len(frontier) > 0; round++ {
			next := map[string]int{}
			for name, d := range frontier {
				for _, im := range impls[name] {
					if _, seen := viaIface[im]; !seen {
						next[im] = d
					}
				}
			}
			for name, d := range next {
				viaIface[name] = d
				reached := map[string]int{}
				if reachers(graph, []string{name}, d, reached) {
					truncated = true
				}
				for r, rd := range reached {
					if _, seen := viaIface[r]; !seen {
						viaIface[r] = rd
						next[r] = rd
					}
				}
			}
			frontier = next
		}
	}
	seen := map[string]bool{}
	key := func(rt facts.Fact, handler string) string {
		return rt.Repo + "\x00" + rt.PropString("method") + "\x00" + rt.Name + "\x00" + handler
	}
	add := func(rt facts.Fact, handler, precision string, depth int, pkg string) {
		k := key(rt, handler)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, Endpoint{
			Method: rt.PropString("method"), Path: rt.Name, Handler: handler,
			Repo: rt.Repo, File: rt.File, Line: rt.Line,
			Precision: precision, Depth: depth, Package: pkg,
		})
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
		}
	}
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
