package facts

import (
	"fmt"
	"reflect"
	"testing"
)

// orderFixture is a hub with more dependents than a tight node cap admits, and a
// name two facts declare. Both are answered by position in a list, which is where
// the store's order used to leak into the answer.
func orderFixture() []Fact {
	ff := []Fact{
		{Kind: KindSymbol, Name: "core.Util", File: "core/util.go", Line: 3, Relations: []Relation{
			{Kind: RelDeclares, Target: "core"},
			{Kind: RelCalls, Target: "log.Print"},
			{Kind: RelCalls, Target: "fmt.Sprint"},
		}},
		{Kind: KindModule, Name: "core", File: "core"},
		// One name, reopened in two files, the way a Ruby module is.
		{Kind: KindSymbol, Name: "app.Shared", File: "app/z.rb", Line: 9, Relations: []Relation{{Kind: RelCalls, Target: "core.Util"}}},
		{Kind: KindSymbol, Name: "app.Shared", File: "app/a.rb", Line: 2, Relations: []Relation{{Kind: RelInstantiates, Target: "core.Util"}}},
	}
	for i := range 8 {
		ff = append(ff, Fact{
			Kind: KindSymbol, Name: fmt.Sprintf("svc.Caller%d", i), File: fmt.Sprintf("svc/c%d.go", i), Line: i + 1,
			Relations: []Relation{
				{Kind: RelCalls, Target: "core.Util"},
				{Kind: RelDeclares, Target: "svc"},
			},
		})
	}
	return ff
}

// graphOf builds a graph over ff as given, or over ff with the facts and every fact's
// relations reversed: one graph, held in the two orders a store can hold it in.
func graphOf(ff []Fact, reverse bool) *Graph {
	s := NewStore()
	if !reverse {
		s.Add(ff...)
	} else {
		for i := len(ff) - 1; i >= 0; i-- {
			f := ff[i]
			rels := make([]Relation, len(f.Relations))
			for j, r := range f.Relations {
				rels[len(rels)-1-j] = r
			}
			f.Relations = rels
			s.Add(f)
		}
	}
	s.BuildGraph()
	return s.Graph()
}

func TestGraph_AnswersDoNotDependOnStoreOrder(t *testing.T) {
	ff := orderFixture()
	a, b := graphOf(ff, false), graphOf(ff, true)

	// A cap of four leaves room for the target and three of its ten dependents, so
	// WHICH three is the question.
	impactA, impactB := a.ImpactSet("core.Util", 3, 4, true), b.ImpactSet("core.Util", 3, 4, true)
	if !reflect.DeepEqual(impactA, impactB) {
		t.Errorf("ImpactSet differs with the store's order:\n%+v\n%+v", impactA, impactB)
	}
	var shown []string
	for _, n := range impactA.ByDepth[1] {
		shown = append(shown, n.Name+"@"+n.File)
	}
	// By name; and app.Shared is reported from the first file that declares it.
	if want := []string{"app.Shared@app/a.rb", "svc.Caller0@svc/c0.go", "svc.Caller1@svc/c1.go"}; !reflect.DeepEqual(shown, want) {
		t.Errorf("dependents under the cap = %v, want %v", shown, want)
	}

	for _, direction := range []string{"forward", "reverse"} {
		ta := a.Traverse("core.Util", direction, nil, nil, 2, 3)
		tb := b.Traverse("core.Util", direction, nil, nil, 2, 3)
		if !reflect.DeepEqual(ta, tb) {
			t.Errorf("Traverse %s differs with the store's order:\n%+v\n%+v", direction, ta, tb)
		}
	}
	if pa, pb := a.FindPath("svc.Caller7", "log.Print", nil, 5), b.FindPath("svc.Caller7", "log.Print", nil, 5); !reflect.DeepEqual(pa, pb) {
		t.Errorf("FindPath differs with the store's order:\n%+v\n%+v", pa, pb)
	}
}

// The kinds at one depth come out of a map; they are listed sorted.
func TestImpactSet_SummaryListsKindsInOneOrder(t *testing.T) {
	g, _ := buildTestGraph()
	want := "3 total dependents — depth 1: 1 module, 1 symbol; depth 2: 1 symbol"
	for range 20 {
		if got := g.ImpactSet("C", 10, 100, false).Summary; got != want {
			t.Fatalf("Summary = %q, want %q", got, want)
		}
	}
}
