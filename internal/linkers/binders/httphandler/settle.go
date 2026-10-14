package httphandler

import (
	"log"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// settleTargets makes every handled_by relation of a route name a fact the snapshot
// holds, or removes it.
//
// An extractor writes the relation from one file, and names the handler as that file
// does. Two spellings name a real symbol the file cannot spell in full:
//
//   - A bare type name. A Dart route file writes `HomeScreen()`, and the widget is the
//     symbol `lib/screens.HomeScreen`.
//   - A package and a name. An Angular route file imports a component from a package's
//     entry point, `libs/components/src`, and the component is declared in a directory
//     below it, `libs/components/src/anon-layout.AnonLayoutWrapperComponent`.
//
// Each is resolved to the one type that fits. A name that fits none, or more than one,
// has its relation removed: a handler that is not in the graph counts as a bound route
// wherever relations are counted and leads nowhere wherever they are followed.
func settleTargets(store *facts.Store) {
	// Routes are few and symbols are many, so the routes are read first and the
	// symbols are scanned only for the names the routes left unresolved.
	type pending struct{ repo, short string }
	wanted := map[pending][]string{} // -> candidate type symbols
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.PropString(facts.PropRouteType) == facts.RouteTypeGRPC {
			continue // grpcimpl's to bind, as in Bind
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy && len(store.ByName(r.Target)) == 0 {
				wanted[pending{f.Repo, facts.ShortName(r.Target)}] = nil
			}
		}
	}
	if len(wanted) == 0 {
		return
	}
	for _, s := range store.ByKind(facts.KindSymbol) {
		switch s.PropString("symbol_kind") {
		case facts.SymbolClass, facts.SymbolInterface:
		default:
			continue
		}
		key := pending{s.Repo, facts.ShortName(s.Name)}
		if _, ok := wanted[key]; ok {
			wanted[key] = append(wanted[key], s.Name)
		}
	}
	for key, names := range wanted {
		sort.Strings(names)
		wanted[key] = dedupe(names)
	}

	rebound, removed := 0, 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindRoute || f.PropString(facts.PropRouteType) == facts.RouteTypeGRPC {
			return
		}
		kept := f.Relations[:0:0]
		changed := false
		for _, r := range f.Relations {
			if r.Kind != facts.RelHandledBy {
				kept = append(kept, r)
				continue
			}
			candidates, unresolved := wanted[pending{f.Repo, facts.ShortName(r.Target)}]
			if !unresolved || hasName(candidates, r.Target) {
				kept = append(kept, r)
				continue
			}
			changed = true
			if t := pickTarget(r.Target, f.File, candidates); t != "" {
				r.Target = t
				kept = append(kept, r)
				rebound++
				continue
			}
			removed++
		}
		if changed {
			f.Relations = kept
		}
	})
	if rebound+removed > 0 {
		log.Printf("[binder:http-handler] settled handler names: %d resolved to their symbol, %d removed", rebound, removed)
	}
}

// pickTarget returns the one candidate a handler name means, or "".
func pickTarget(target, routeFile string, candidates []string) string {
	dot := strings.LastIndexByte(target, '.')
	if dot < 0 {
		// A bare name is the only type of that name in the repository. When there
		// are several, it is the one declared nearest the route file, provided one
		// is nearer than all the others: two example apps in one repository each
		// have a HomePage, and each routes to its own.
		if len(candidates) == 1 {
			return candidates[0]
		}
		dir := factpath.Dir(routeFile)
		best, bestShared, tie := "", 0, false
		for _, c := range candidates {
			// The route file's own directory outranks one below it that shares
			// every segment.
			declaredIn := c[:strings.LastIndexByte(c, '.')]
			shared := 2 * sharedSegments(dir, declaredIn)
			if declaredIn == dir {
				shared++
			}
			switch {
			case shared > bestShared:
				best, bestShared, tie = c, shared, false
			case shared == bestShared:
				tie = true
			}
		}
		if tie {
			return ""
		}
		return best
	}
	// A qualified name is the only type of that name below the package it names.
	below := target[:dot] + "/"
	found := ""
	for _, c := range candidates {
		if !strings.HasPrefix(c, below) {
			continue
		}
		if found != "" {
			return ""
		}
		found = c
	}
	return found
}

func hasName(names []string, name string) bool {
	i := sort.SearchStrings(names, name)
	return i < len(names) && names[i] == name
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// sharedSegments counts the leading path segments a and b have in common.
func sharedSegments(a, b string) int {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return n
}
