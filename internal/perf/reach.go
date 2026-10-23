package perf

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Which endpoints reach a finding.
//
// A finding names a function; whether it matters depends on what runs it. A route
// handler is escalated already, but a query loop three calls below one was not tied to
// any route, so a reader could not tell the feed every user loads from a nightly admin
// export without tracing callers by hand. routeReach walks the call graph backwards
// from the finding to the handlers that reach it and names their routes.
//
// In Go a handler reaches a repository through a service interface, and a call on an
// interface method names the interface, not the type behind it. The walk crosses that
// hop the way the I/O analysis does: an implementation's method is reached by every
// call on the interface method it implements, for the types the extractor states or
// infers implement it (the implementers props on the interface's symbol fact).

const (
	propImplementers         = "implementers"
	propImplementersInferred = "implementers_inferred"
	// reachMaxDepth bounds the walk in call hops. A handler deeper than this above a
	// loop is rare, and the bound keeps the walk linear in practice.
	reachMaxDepth = 8
	// reachMaxNodes bounds the functions one walk visits.
	reachMaxNodes = 4000
	// maxRoutesShown is how many route labels a finding carries; RoutesTotal keeps
	// the full count.
	maxRoutesShown = 5
)

// reachIndex holds what the walk needs from the store, built once per analysis.
type reachIndex struct {
	graph         *facts.Graph
	handlerRoutes map[string][]string // handler symbol → "METHOD /path" of the routes it serves
	viaInterface  map[string][]string // "Impl.Method" → "Iface.Method" calls on which reach it
}

func newReachIndex(store *facts.Store) *reachIndex {
	x := &reachIndex{
		graph:         store.Graph(),
		handlerRoutes: map[string][]string{},
		viaInterface:  map[string][]string{},
	}
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.PropAny(facts.PropRole) == facts.RoleClient || f.Name == "" {
			continue
		}
		method := strings.ToUpper(f.PropString("method"))
		if method == "USE" {
			continue // middleware: no single route
		}
		label := strings.TrimSpace(method + " " + f.Name)
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy && r.Target != "" {
				x.handlerRoutes[r.Target] = append(x.handlerRoutes[r.Target], label)
			}
		}
	}

	// Methods by owner, so an interface's methods and an implementation's can be paired.
	methods := map[string][]string{} // owner → method short names
	for _, f := range store.ByKind(facts.KindSymbol) {
		sk := f.PropString("symbol_kind")
		if sk != facts.SymbolMethod {
			continue
		}
		if i := strings.LastIndex(f.Name, "."); i > 0 {
			methods[f.Name[:i]] = append(methods[f.Name[:i]], f.Name[i+1:])
		}
	}
	for _, f := range store.ByKind(facts.KindSymbol) {
		if f.PropString("symbol_kind") != facts.SymbolInterface {
			continue
		}
		impls := append(stringSliceProp(f.Props, propImplementers), stringSliceProp(f.Props, propImplementersInferred)...)
		if len(impls) == 0 {
			continue
		}
		ifaceMethods := map[string]bool{}
		for _, m := range methods[f.Name] {
			ifaceMethods[m] = true
		}
		for _, impl := range impls {
			for _, m := range methods[impl] {
				if ifaceMethods[m] {
					k := impl + "." + m
					x.viaInterface[k] = append(x.viaInterface[k], f.Name+"."+m)
				}
			}
		}
	}
	return x
}

// routesReaching returns the routes whose handlers reach sym through calls, sorted,
// at most maxRoutesShown of them, and how many there are in all.
func (x *reachIndex) routesReaching(sym string) ([]string, int) {
	seen := map[string]bool{sym: true}
	frontier := []string{sym}
	routes := map[string]bool{}
	for depth := 0; depth <= reachMaxDepth && len(frontier) > 0 && len(seen) < reachMaxNodes; depth++ {
		var next []string
		for _, n := range frontier {
			for _, r := range x.handlerRoutes[n] {
				routes[r] = true
			}
			if x.graph == nil {
				continue // no graph built: a function's own routes only
			}
			callees := append([]string{n}, x.viaInterface[n]...)
			for _, c := range callees {
				for _, e := range x.graph.ReverseEdges(c) {
					if e.RelKind != facts.RelCalls || seen[e.Target] {
						continue
					}
					seen[e.Target] = true
					next = append(next, e.Target)
				}
			}
		}
		frontier = next
	}
	out := make([]string, 0, len(routes))
	for r := range routes {
		out = append(out, r)
	}
	sort.Strings(out)
	total := len(out)
	if len(out) > maxRoutesShown {
		out = out[:maxRoutesShown]
	}
	return out, total
}

// attachRoutes sets Routes and RoutesTotal on every finding.
func attachRoutes(store *facts.Store, findings []Finding) {
	if len(findings) == 0 {
		return
	}
	x := newReachIndex(store)
	cache := map[string]struct {
		routes []string
		total  int
	}{}
	for i := range findings {
		sym := findings[i].Symbol
		c, ok := cache[sym]
		if !ok {
			c.routes, c.total = x.routesReaching(sym)
			cache[sym] = c
		}
		findings[i].Routes, findings[i].RoutesTotal = c.routes, c.total
	}
}
