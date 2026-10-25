package endpoint

import (
	"path"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Screen is one screen of a client application and the endpoints of the answer it
// depends on: code the screen runs can send a request to each.
type Screen struct {
	// File declares the screen, named as facts name files.
	File string `json:"file"`
	// Symbol is the screen's own declaration, where the call graph found it.
	Symbol string `json:"symbol,omitempty"`
	// Endpoints are the matched server routes, "METHOD /path" as the server declares it.
	Endpoints []string `json:"endpoints"`
}

// screenWalkDepth and screenWalkNodes bound the walk from a client call up to the
// screens that run it. A data layer, a repository, a use case and a view model are
// four hops; the bounds leave room for a few more and stop a walk that has left the
// feature.
const (
	screenWalkDepth = 10
	screenWalkNodes = 2000
)

// screensFrom answers which screens depend on the followed routes, through the client
// calls that hit them. A web page comes from the pages the TypeScript extractor found
// reaching the call (facts.PropClientPages). Elsewhere the call graph is walked in
// reverse from the function making the call to the nearest screen on each path
// (isScreen), across the edges an interface puts in the way: a call reaches an
// interface's method, and the implementation's method is what makes the request.
//
// A call in code nothing runs adds no screen.
func screensFrom(s *facts.Store, clients, servers []facts.Fact) []Screen {
	followed := map[string]string{}
	for _, f := range servers {
		followed[f.Repo+"\x00"+f.PropString("method")+"\x00"+f.Name] = f.PropString("method") + " " + f.Name
	}
	g := s.Graph()
	byScreen := map[string]*Screen{}
	walked := map[string][]facts.Fact{}
	for _, c := range clients {
		if c.PropString(facts.PropRole) != facts.RoleClient || c.PropAny("test_double") == true ||
			c.PropAny(facts.PropClientReachable) == false {
			continue
		}
		var endpoints []string
		for _, m := range matchedRoutes(c) {
			if e, ok := followed[m.repo+"\x00"+m.method+"\x00"+m.name]; ok {
				endpoints = append(endpoints, e)
			}
		}
		if len(endpoints) == 0 {
			continue
		}
		var found []facts.Fact
		if pages := stringList(c.PropAny(facts.PropClientPages)); len(pages) > 0 {
			prefix := ""
			if c.Repo != "" && strings.HasPrefix(c.File, c.Repo+"/") {
				prefix = c.Repo + "/"
			}
			for _, p := range pages {
				found = append(found, facts.Fact{File: prefix + p})
			}
		} else if caller := c.PropString(facts.PropCaller); caller != "" && g != nil {
			if _, ok := walked[caller]; !ok {
				walked[caller] = screensAbove(s, g, caller)
			}
			found = walked[caller]
		}
		for _, f := range found {
			sc := byScreen[f.File]
			if sc == nil {
				sc = &Screen{File: f.File, Symbol: f.Name}
				byScreen[f.File] = sc
			}
			sc.Endpoints = append(sc.Endpoints, endpoints...)
		}
	}
	out := make([]Screen, 0, len(byScreen))
	for _, sc := range byScreen {
		sort.Strings(sc.Endpoints)
		sc.Endpoints = compactStrings(sc.Endpoints)
		out = append(out, *sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// screensAbove walks the call graph in reverse from a function to the nearest screen
// on each path, and returns those screens' facts.
func screensAbove(s *facts.Store, g *facts.Graph, start string) []facts.Fact {
	seen := map[string]bool{start: true}
	frontier := []string{start}
	var out []facts.Fact
	for depth := 0; depth < screenWalkDepth && len(frontier) > 0 && len(seen) < screenWalkNodes; depth++ {
		var next []string
		visit := func(n string) {
			if seen[n] || len(seen) >= screenWalkNodes {
				return
			}
			seen[n] = true
			if f, ok := screenFact(s, n); ok {
				out = append(out, f)
				return
			}
			next = append(next, n)
		}
		for _, n := range frontier {
			for _, e := range g.ReverseEdges(n) {
				switch e.RelKind {
				case "calls", "instantiates", "names":
					visit(e.Target)
				}
			}
			// The interface's method is what callers call; the walk goes on from there.
			if dot := strings.LastIndexByte(n, '.'); dot > 0 {
				for _, e := range g.ForwardEdges(n[:dot]) {
					if e.RelKind == "implements" {
						visit(e.Target + n[dot:])
					}
				}
			}
		}
		frontier = next
	}
	return out
}

// screenFact returns the symbol fact name declares when it is a screen.
func screenFact(s *facts.Store, name string) (facts.Fact, bool) {
	for _, f := range s.LookupByExactName(name) {
		if f.Kind == facts.KindSymbol && isScreen(f) {
			return f, true
		}
	}
	return facts.Fact{}, false
}

// isScreen reports a declaration a user navigates to, by the conventions of its
// platform: a Jetpack Compose function, an activity or a fragment named for it
// (ProfileScreen, SettingsActivity), and a SwiftUI view declared under a Screens
// folder or named a screen. A view a screen is built from, a card or a row, is not one;
// the walk stops at the nearest screen, so a screen's own components never stand in
// for it unless they are named as screens.
func isScreen(f facts.Fact) bool {
	last := f.Name[strings.LastIndexAny(f.Name, ".#")+1:]
	switch f.PropString("language") {
	case "kotlin", "java":
		return strings.HasSuffix(last, "Screen") || strings.HasSuffix(last, "Activity") || strings.HasSuffix(last, "Fragment")
	case "swift":
		switch f.PropString("symbol_kind") {
		case facts.SymbolStruct, facts.SymbolClass:
		default:
			return false
		}
		if strings.HasSuffix(last, "Screen") {
			return true
		}
		return strings.HasSuffix(last, "View") && strings.Contains("/"+path.Dir(f.File)+"/", "/Screens/")
	}
	return false
}

type matchedRoute struct{ repo, method, name string }

// matchedRoutes reads facts.PropMatchedRoutes, in the shape a fact has whether the
// linker set it or it was read back from disk.
func matchedRoutes(f facts.Fact) []matchedRoute {
	var out []matchedRoute
	add := func(m map[string]any) {
		r, _ := m["repo"].(string)
		meth, _ := m["method"].(string)
		n, _ := m["name"].(string)
		out = append(out, matchedRoute{r, meth, n})
	}
	switch v := f.PropAny(facts.PropMatchedRoutes).(type) {
	case []any:
		for _, e := range v {
			if m, ok := e.(map[string]any); ok {
				add(m)
			}
		}
	case []map[string]any:
		for _, m := range v {
			add(m)
		}
	}
	return out
}

func stringList(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, e := range l {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func compactStrings(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}
