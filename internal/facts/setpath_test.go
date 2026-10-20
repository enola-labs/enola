package facts

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

// shortestOverPairs is the second derivation the set search exists to replace,
// kept here as its oracle: FindPath over every source and every rolled-up
// target, the shortest found.
func shortestOverPairs(g *Graph, froms, tos, relKinds []string, maxDepth int) (int, bool) {
	best, found := 0, false
	for _, from := range froms {
		for _, to := range tos {
			if _, ok := g.lookup(to); !ok {
				continue
			}
			for _, seed := range g.RollupSeeds(to) {
				r := g.FindPath(from, seed, relKinds, maxDepth)
				if r.Found && (!found || len(r.Path) < best) {
					best, found = len(r.Path), true
				}
			}
		}
	}
	return best, found
}

// connectedCorpus is a seeded graph whose edges land on declared nodes and
// drift forward a few steps at a time, with an occasional long jump and every
// tenth symbol a type whose method is what callers reach. Sources taken from
// the low end and targets from a region further on need several hops, and a
// shallow search finds nothing, so the oracle is asked real questions.
func connectedCorpus() *Graph {
	rng := rand.New(rand.NewSource(7))
	const n = 600
	kinds := []string{RelCalls, RelImports, RelDependsOn}
	s := NewStore()
	for i := 0; i < n; i++ {
		f := Fact{Kind: KindSymbol, Name: corpusName(i), File: fmt.Sprintf("pkg%d/f.go", i%12), Props: map[string]any{"symbol_kind": SymbolFunc}}
		for j := 0; j < 1+rng.Intn(2); j++ {
			next := i + 1 + rng.Intn(4)
			if rng.Intn(25) == 0 {
				next = i + rng.Intn(60)
			}
			if next >= n {
				continue
			}
			target := corpusName(next)
			if next%10 == 0 {
				target += ".Run"
			}
			f.Relations = append(f.Relations, Relation{Kind: kinds[rng.Intn(len(kinds))], Target: target})
		}
		if i%10 == 0 {
			f.Props["symbol_kind"] = SymbolStruct
			f.Relations = append(f.Relations, Relation{Kind: RelHasMethod, Target: corpusName(i) + ".Run"})
			s.Add(Fact{Kind: KindSymbol, Name: corpusName(i) + ".Run", File: f.File, Props: map[string]any{"symbol_kind": SymbolMethod},
				Relations: []Relation{{Kind: RelCalls, Target: corpusName(i + 1)}}})
		}
		s.Add(f)
	}
	s.BuildGraph()
	return s.Graph()
}

func corpusName(i int) string { return fmt.Sprintf("pkg%d.N%03d", i%12, i) }

func TestFindPathBetween_IsTheShortestOverEveryPair(t *testing.T) {
	g := connectedCorpus()
	rng := rand.New(rand.NewSource(11))
	pick := func(lo, hi, k int) []string {
		seen := map[int]bool{}
		var out []string
		for len(out) < k {
			i := lo + rng.Intn(hi-lo)
			if !seen[i] {
				seen[i] = true
				out = append(out, corpusName(i))
			}
		}
		return out
	}
	froms, tos := pick(0, 120, 45), pick(160, 400, 45)
	found, multiHop := 0, 0
	for _, tc := range []struct {
		relKinds []string
		maxDepth int
	}{{nil, 2}, {nil, 6}, {nil, 12}, {nil, 20}, {[]string{RelCalls}, 20}, {[]string{RelImports, RelDependsOn}, 20}} {
		got := g.FindPathBetween(froms, tos, tc.relKinds, tc.maxDepth)
		want, reachable := shortestOverPairs(g, froms, tos, tc.relKinds, tc.maxDepth)
		t.Logf("%d sources, %d targets, rels=%v depth=%d: found=%v, %d-node path, shortest pair %d",
			len(froms), len(tos), tc.relKinds, tc.maxDepth, got.Found, len(got.Path), want)
		if got.Found != reachable {
			t.Fatalf("rels=%v depth=%d: found=%v, the pairs say %v", tc.relKinds, tc.maxDepth, got.Found, reachable)
		}
		if !got.Found {
			continue
		}
		found++
		if len(got.Path) > 3 {
			multiHop++
		}
		if len(got.Path) != want {
			t.Fatalf("rels=%v depth=%d: path of %d nodes, the shortest pair is %d", tc.relKinds, tc.maxDepth, len(got.Path), want)
		}
		if got.Path[0].Name != got.From || got.Path[len(got.Path)-1].Name != got.To {
			t.Fatalf("rels=%v depth=%d: path %v does not join %s to %s", tc.relKinds, tc.maxDepth, pathNames(got.Path), got.From, got.To)
		}
		if len(got.Edges) != len(got.Path)-1 {
			t.Fatalf("rels=%v depth=%d: %d edges for %d nodes", tc.relKinds, tc.maxDepth, len(got.Edges), len(got.Path))
		}
		again := g.FindPathBetween(froms, tos, tc.relKinds, tc.maxDepth)
		if !reflect.DeepEqual(got, again) {
			t.Fatalf("rels=%v depth=%d: a rerun returned a different path", tc.relKinds, tc.maxDepth)
		}
	}
	if found < 2 || multiHop < 2 {
		t.Fatalf("%d cases found a path, %d of them longer than two hops; the oracle compared too little", found, multiHop)
	}
}

func TestFindPathBetween_NamesTheGraphDoesNotHoldAreListed(t *testing.T) {
	g, _ := buildTestGraph()
	got := g.FindPathBetween([]string{"A", "nowhere.From"}, []string{"D", "nowhere.To", "nowhere.To"}, nil, 10)
	if !got.Found || got.From != "A" || got.To != "D" {
		t.Fatalf("got %+v, want A reaching D with the unknown names set aside", got.PathResult)
	}
	if !reflect.DeepEqual(got.UnresolvedFrom, []string{"nowhere.From"}) || !reflect.DeepEqual(got.UnresolvedTo, []string{"nowhere.To"}) {
		t.Fatalf("unresolved from=%v to=%v, want each unknown name listed once", got.UnresolvedFrom, got.UnresolvedTo)
	}
	none := g.FindPathBetween([]string{"nowhere.From"}, []string{"D"}, nil, 10)
	if none.Found || !reflect.DeepEqual(none.UnresolvedFrom, []string{"nowhere.From"}) {
		t.Fatalf("got %+v, want not found with the source listed", none)
	}
}

// A path ending at a type's method reaches the type, as find_path's single
// target does through RollupSeeds.
func TestFindPathBetween_TargetsRollUpToTheirMethods(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "app.Handler", File: "app/handler.go", Props: map[string]any{"symbol_kind": SymbolFunc},
			Relations: []Relation{{Kind: RelCalls, Target: "store.Repo.Save"}}},
		Fact{Kind: KindSymbol, Name: "store.Repo", File: "store/repo.go", Props: map[string]any{"symbol_kind": SymbolStruct},
			Relations: []Relation{{Kind: RelHasMethod, Target: "store.Repo.Save"}}},
		Fact{Kind: KindSymbol, Name: "store.Repo.Save", File: "store/repo.go", Props: map[string]any{"symbol_kind": SymbolMethod}},
	)
	s.BuildGraph()
	got := s.Graph().FindPathBetween([]string{"app.Handler"}, []string{"store.Repo"}, nil, 10)
	if !got.Found || got.To != "store.Repo.Save" || len(got.Path) != 2 {
		t.Fatalf("got %+v, want the call into the type's method", got.PathResult)
	}
}

// Among equally short paths the one found first in name order is returned: the
// sources are seeded in name order and each node's neighbours are read in name
// order, whatever order the edges were added in.
func TestFindPathBetween_TiesGoByName(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "src.B", Relations: []Relation{{Kind: RelCalls, Target: "dst.X"}}},
		Fact{Kind: KindSymbol, Name: "src.A", Relations: []Relation{{Kind: RelCalls, Target: "dst.Z"}, {Kind: RelCalls, Target: "dst.Y"}}},
		Fact{Kind: KindSymbol, Name: "dst.X"},
		Fact{Kind: KindSymbol, Name: "dst.Y"},
		Fact{Kind: KindSymbol, Name: "dst.Z"},
	)
	s.BuildGraph()
	got := s.Graph().FindPathBetween([]string{"src.B", "src.A"}, []string{"dst.Z", "dst.X", "dst.Y"}, nil, 10)
	if got.From != "src.A" || got.To != "dst.Y" {
		t.Fatalf("path %s -> %s, want src.A -> dst.Y", got.From, got.To)
	}
}
