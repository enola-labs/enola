package queryloops

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// The Go shape. A Go service reaches its database through a repository, and the
// repository through an interface the service holds, so the query is rarely in the
// loop's own body: `for _, e := range events { s.repo.GetPricing(ctx, e.ID) }` is the
// N+1, and the query is two calls and an interface away. The extractor records what
// each function's body does — db_direct and db_calls for a database round trip in it,
// calls edges for what it calls, pure_calls for a cursor read that is not a round
// trip — and, on each interface, the types stated or inferred to implement it. This
// pass walks those facts backwards from every database call to every function that
// reaches one, and reports each loop that calls such a function.
//
// It is a separate pass from the Ruby shapes because nothing about it is shared: no
// model classes, no associations, no preloads. A Go repository has none of them,
// which is why the explainer reported nothing on one before.

// Bases, weakest last: how a hop of the path to the query is known. A path is as
// strong as its weakest hop.
const (
	basisDirect   = iota // the loop's own body calls the database
	basisCalls           // through calls edges between concrete functions
	basisStated          // through an interface a constructor or an assertion ties to its implementer
	basisInferred        // through an interface matched to its implementer by method set
)

const (
	propImplementers         = "implementers"
	propImplementersInferred = "implementers_inferred"
)

// goReach is how a function reaches a database call: the next hop and the weakest
// basis on the way.
type goReach struct {
	next  string
	basis int
}

type goKey struct{ repo, name string }

// goCandidates returns the Go findings in a store.
func goCandidates(store *facts.Store) []finding {
	syms := make(map[goKey]facts.Fact)
	for _, f := range store.ByKind(facts.KindSymbol) {
		if strings.HasSuffix(f.File, ".go") && !goExcludedFile(f.File) {
			syms[goKey{f.Repo, f.Name}] = f
		}
	}
	if len(syms) == 0 {
		return nil
	}

	// What reaches what, reversed: callers of each function, and for each
	// implementer's method, the interface method a call on the interface names.
	type edge struct {
		from  goKey
		basis int
	}
	callers := make(map[goKey][]edge)
	// Methods by owner, so an interface's methods are found without a scan.
	methodsOf := make(map[goKey][]string)
	for k := range syms {
		if dot := strings.LastIndexByte(k.name, '.'); dot > 0 {
			owner := goKey{k.repo, k.name[:dot]}
			methodsOf[owner] = append(methodsOf[owner], k.name[dot+1:])
		}
	}
	for k, f := range syms {
		once := stringSet(propStrings(f.Props["calls_once"]))
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls || once[r.Target] {
				continue
			}
			callee := goKey{k.repo, r.Target}
			callers[callee] = append(callers[callee], edge{from: k, basis: basisCalls})
		}
		if f.PropString("symbol_kind") != facts.SymbolInterface {
			continue
		}
		for basis, prop := range map[int]string{basisStated: propImplementers, basisInferred: propImplementersInferred} {
			for _, impl := range propStrings(f.Props[prop]) {
				// The interface's methods are its own facts, named under it.
				for _, method := range methodsOf[k] {
					implMethod := goKey{k.repo, impl + "." + method}
					if _, ok := syms[implMethod]; ok {
						callers[implMethod] = append(callers[implMethod], edge{from: goKey{k.repo, k.name + "." + method}, basis: basis})
					}
				}
			}
		}
	}

	// Breadth first from every function whose body calls the database, so each
	// reaching function records its shortest way there.
	reach := make(map[goKey]goReach)
	var queue []goKey
	for k, f := range syms {
		if calls := propStrings(f.Props["db_calls"]); len(calls) > 0 {
			reach[k] = goReach{next: calls[0], basis: basisDirect}
			queue = append(queue, k)
		}
	}
	sortKeys(queue)
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		for _, e := range callers[k] {
			if _, seen := reach[e.from]; seen {
				continue
			}
			reach[e.from] = goReach{next: k.name, basis: max(e.basis, reach[k].basis, basisCalls)}
			queue = append(queue, e.from)
		}
	}

	var found []finding
	keys := make([]goKey, 0, len(syms))
	for k := range syms {
		keys = append(keys, k)
	}
	sortKeys(keys)
	for _, k := range keys {
		f := syms[k]
		inLoop := propStrings(f.Props["calls_in_scaling_loop"])
		if len(inLoop) == 0 {
			continue
		}
		depths := propInts(f.Props["calls_in_scaling_loop_depth"])
		direct := stringSet(propStrings(f.Props["db_calls"]))
		onElement := stringSet(propStrings(f.Props["calls_on_loop_element"]))
		_, elementKnown := f.Props["calls_on_loop_element"]
		for i, call := range inLoop {
			depth := propInt(f.Props["scaling_loop_depth"])
			if i < len(depths) && depths[i] > 0 {
				depth = depths[i]
			}
			var path []string
			basis := basisDirect
			switch {
			case direct[call]:
				path = []string{call}
			default:
				r, ok := reach[goKey{k.repo, call}]
				if !ok {
					continue
				}
				path, basis = goPath(goKey{k.repo, call}, reach, syms)
				basis = max(basis, r.basis)
			}
			found = append(found, finding{
				symbol: k.name, file: f.File, repo: k.repo, call: call, depth: max(depth, 1),
				goPath: path, goBasis: basis, invariant: elementKnown && !onElement[call],
			})
		}
	}
	return found
}

// goPath follows the recorded hops from a reaching function to the database call
// it ends at, and returns them with the weakest basis on the way.
func goPath(start goKey, reach map[goKey]goReach, syms map[goKey]facts.Fact) ([]string, int) {
	path := []string{start.name}
	basis := basisDirect
	seen := map[goKey]bool{start: true}
	for at := start; ; {
		r := reach[at]
		basis = max(basis, r.basis)
		path = append(path, r.next)
		if r.basis == basisDirect {
			return path, basis
		}
		next := goKey{at.repo, r.next}
		if seen[next] || len(path) > 32 {
			return path, basis
		}
		seen[next] = true
		at = next
		if _, ok := syms[at]; !ok {
			return path, basis
		}
	}
}

// goExcludedFile reports a Go file whose loops are not the application's: a test,
// or a test double.
func goExcludedFile(file string) bool {
	return strings.HasSuffix(file, "_test.go") || strings.Contains("/"+file, "/mocks/") ||
		strings.Contains("/"+file, "/mock/")
}

// goSurfaceOf is surfaceOf for a Go file. Go has no layout as fixed as Rails', so
// a file is shared unless it is plainly a one-off program or a migration.
func goSurfaceOf(file string) surface {
	if goExcludedFile(file) {
		return surface{name: "test", excluded: true}
	}
	for _, seg := range strings.Split(file, "/") {
		switch seg {
		case "cmd", "scripts", "tools":
			return surface{name: "task", phrase: "runs as a one-off command", oneOff: true}
		case "migrations", "migration", "modelmigration":
			return surface{name: "task", phrase: "runs once, in a migration", oneOff: true}
		}
	}
	return sharedSurface
}

func propInts(v any) []int {
	switch x := v.(type) {
	case []int:
		return x
	case []any:
		out := make([]int, 0, len(x))
		for _, e := range x {
			out = append(out, propInt(e))
		}
		return out
	}
	return nil
}

func sortKeys(keys []goKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].repo != keys[j].repo {
			return keys[i].repo < keys[j].repo
		}
		return keys[i].name < keys[j].name
	})
}

// goDescription is the statement and the fix for a Go finding.
func goDescription(f finding) (string, []string) {
	via := ""
	if len(f.goPath) > 1 {
		via = " through " + strings.Join(f.goPath, " -> ")
	}
	if f.invariant {
		return fmt.Sprintf(
				"%s calls %s inside a loop nested %d deep, and the call reaches a database query%s. Its "+
					"arguments do not depend on the loop's element, so every iteration repeats the same query.",
				f.symbol, f.call, f.depth, via), []string{
				"run the query once before the loop and reuse the result",
				"confirm with a query-count test that fails against the current code",
			}
	}
	return fmt.Sprintf(
			"%s calls %s once per element of a loop nested %d deep, and the call reaches a database "+
				"query%s. N elements issue N queries.",
			f.symbol, f.call, f.depth, via), []string{
			"read the set once before the loop (WHERE id IN (...)) and index it by key",
			"confirm with a query-count test that fails against the current code",
		}
}
