package perf

import (
	"fmt"
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
	routeTraffic  map[string]traffic  // "METHOD /path" → the strongest client trigger it receives
}

// traffic is how clients run a route: the strongest trigger among the client calls
// that match it, as the linker recorded it on the route.
type traffic struct {
	trigger   string
	everyPage bool
	ms        int
	from      string // the client function behind the trigger
	route     string
}

var trafficRank = map[string]int{facts.ClientTriggerPoll: 3, facts.ClientTriggerMount: 2, facts.ClientTriggerAction: 1}

// order is a sort key: higher means more traffic. A trigger, then every page, then a
// stated, shorter poll interval.
func (t traffic) order() int {
	o := trafficRank[t.trigger] << 40
	if t.everyPage {
		o |= 1 << 39
	}
	if t.ms > 0 && t.ms < 1<<38 {
		o |= 1<<38 - t.ms
	}
	return o
}

// describe is the traffic in words, for a finding's label.
func (t traffic) describe() string {
	var what string
	switch t.trigger {
	case facts.ClientTriggerPoll:
		what = "polled"
		if t.ms > 0 {
			what += " every " + interval(t.ms)
		}
	case facts.ClientTriggerMount:
		what = "loaded with a page"
	case facts.ClientTriggerAction:
		what = "sent on a user action"
	default:
		return ""
	}
	if t.everyPage {
		if t.trigger == facts.ClientTriggerMount {
			what = "loaded on every page"
		} else {
			what += " on every page"
		}
	}
	from := t.route
	if t.from != "" {
		from = t.from + ", " + t.route
	}
	return what + " (" + from + ")"
}

// interval renders a poll interval in the largest whole unit.
func interval(ms int) string {
	switch {
	case ms%60000 == 0:
		return fmt.Sprintf("%d min", ms/60000)
	case ms%1000 == 0:
		return fmt.Sprintf("%d s", ms/1000)
	}
	return fmt.Sprintf("%d ms", ms)
}

func newReachIndex(store *facts.Store) *reachIndex {
	x := &reachIndex{
		graph:         store.Graph(),
		handlerRoutes: map[string][]string{},
		viaInterface:  map[string][]string{},
		routeTraffic:  map[string]traffic{},
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
		if t := f.PropString(facts.PropClientTrigger); trafficRank[t] > 0 {
			ms, _ := f.PropAny(facts.PropClientPollMs).(int)
			if fl, ok := f.PropAny(facts.PropClientPollMs).(float64); ok {
				ms = int(fl)
			}
			tr := traffic{trigger: t, everyPage: f.PropAny(facts.PropClientEveryPage) == true, ms: ms,
				from: f.PropString(facts.PropClientTriggerFrom), route: label}
			if prev, ok := x.routeTraffic[label]; !ok || tr.order() > prev.order() {
				x.routeTraffic[label] = tr
			}
		}
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
// at most maxRoutesShown of them, how many there are in all, and the strongest client
// traffic among them.
func (x *reachIndex) routesReaching(sym string) ([]string, int, traffic) {
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
	var best traffic
	for r := range routes {
		out = append(out, r)
		if t, ok := x.routeTraffic[r]; ok && (t.order() > best.order() || t.order() == best.order() && t.route < best.route) {
			best = t
		}
	}
	sort.Strings(out)
	total := len(out)
	if len(out) > maxRoutesShown {
		out = out[:maxRoutesShown]
	}
	return out, total, best
}

// attachRoutes sets Routes and RoutesTotal on every finding.
func attachRoutes(store *facts.Store, findings []Finding) {
	if len(findings) == 0 {
		return
	}
	x := newReachIndex(store)
	cache := map[string]struct {
		routes  []string
		total   int
		traffic traffic
	}{}
	for i := range findings {
		sym := findings[i].Symbol
		c, ok := cache[sym]
		if !ok {
			c.routes, c.total, c.traffic = x.routesReaching(sym)
			cache[sym] = c
		}
		findings[i].Routes, findings[i].RoutesTotal = c.routes, c.total
		findings[i].Traffic, findings[i].trafficOrder = c.traffic.describe(), c.traffic.order()
	}
}
