package facts

import (
	"slices"
	"testing"
)

// twoAppsGraph is two repositories that each declare a module "app/models" and a
// class "Order", with their own dependents, plus a third repository that declares
// neither and names "Order" anyway.
func twoAppsGraph() *Graph {
	s := NewStore()
	s.Add(
		Fact{Kind: KindModule, Name: "app/models", File: "shop/app/models", Repo: "shop", RepoPrefixed: true},
		Fact{Kind: KindModule, Name: "app/models", File: "ledger/app/models", Repo: "ledger", RepoPrefixed: true},
		Fact{Kind: KindSymbol, Name: "Order", File: "shop/app/models/order.rb", Repo: "shop", RepoPrefixed: true,
			Props: map[string]any{"symbol_kind": SymbolClass}},
		Fact{Kind: KindSymbol, Name: "Order", File: "ledger/app/models/order.rb", Repo: "ledger", RepoPrefixed: true,
			Props: map[string]any{"symbol_kind": SymbolClass}},
		Fact{Kind: KindSymbol, Name: "Order#total", File: "shop/app/models/order.rb", Repo: "shop", RepoPrefixed: true,
			Props: map[string]any{"symbol_kind": SymbolMethod}},
		Fact{Kind: KindSymbol, Name: "CheckoutController", File: "shop/app/controllers/checkout.rb", Repo: "shop", RepoPrefixed: true,
			Relations: []Relation{{Kind: RelCalls, Target: "Order#total"}, {Kind: RelNames, Target: "Order"}}},
		Fact{Kind: KindSymbol, Name: "PostingJob", File: "ledger/app/jobs/posting.rb", Repo: "ledger", RepoPrefixed: true,
			Relations: []Relation{{Kind: RelNames, Target: "Order"}}},
		Fact{Kind: KindDependency, Name: "app/controllers -> app/models", File: "shop/app/controllers/checkout.rb", Repo: "shop", RepoPrefixed: true,
			Relations: []Relation{{Kind: RelImports, Target: "app/models"}}},
		Fact{Kind: KindModule, Name: "app/controllers", File: "shop/app/controllers", Repo: "shop", RepoPrefixed: true},
		Fact{Kind: KindSymbol, Name: "Report", File: "tools/report.rb", Repo: "tools", RepoPrefixed: true,
			Relations: []Relation{{Kind: RelNames, Target: "Order"}}},
	)
	s.BuildGraph()
	return s.Graph()
}

func dependents(r ImpactResult) []string {
	var out []string
	for _, nodes := range r.ByDepth {
		for _, n := range nodes {
			out = append(out, n.Repo+":"+n.Name)
		}
	}
	slices.Sort(out)
	return out
}

func TestSplitName_ScopedImpactStaysInItsRepository(t *testing.T) {
	g := twoAppsGraph()
	got := dependents(g.ImpactSet(ScopedName("ledger", "Order"), 3, 100, false))
	// The ledger's Order is named by its own job and by the repository that declares
	// no Order; the shop's controller names the shop's Order and is not a dependent.
	want := []string{"ledger:PostingJob", "tools:Report"}
	if !slices.Equal(got, want) {
		t.Fatalf("ledger Order dependents = %v, want %v", got, want)
	}

	got = dependents(g.ImpactSet(ScopedName("shop", "Order"), 3, 100, false))
	want = []string{"shop:CheckoutController", "tools:Report"}
	if !slices.Equal(got, want) {
		t.Fatalf("shop Order dependents = %v, want %v (its method's caller included)", got, want)
	}
}

func TestSplitName_BareNameStillCoversEveryRepository(t *testing.T) {
	g := twoAppsGraph()
	got := dependents(g.ImpactSet("Order", 3, 100, false))
	want := []string{"ledger:PostingJob", "shop:CheckoutController", "tools:Report"}
	if !slices.Equal(got, want) {
		t.Fatalf("bare Order dependents = %v, want %v", got, want)
	}
}

func TestSplitName_ModuleImportsResolveInTheImportersRepository(t *testing.T) {
	g := twoAppsGraph()
	walk := g.TraverseFrom([]string{ScopedName("ledger", "app/models")}, "reverse",
		[]string{RelImports}, []string{KindModule}, 5, 100)
	for _, n := range walk.Nodes {
		if n.Depth > 0 {
			t.Fatalf("ledger app/models has no importer, got %s in %s", n.Name, n.Repo)
		}
	}
	walk = g.TraverseFrom([]string{ScopedName("shop", "app/models")}, "reverse",
		[]string{RelImports}, []string{KindModule}, 5, 100)
	var importers []string
	for _, n := range walk.Nodes {
		if n.Depth > 0 {
			importers = append(importers, n.Repo+":"+n.Name)
		}
	}
	if !slices.Equal(importers, []string{"shop:app/controllers"}) {
		t.Fatalf("shop app/models importers = %v", importers)
	}
}

func TestSplitName_NodesKeepTheBareNameAndEdgesRoundTrip(t *testing.T) {
	g := twoAppsGraph()
	walk := g.Traverse(ScopedName("shop", "Order"), "forward", []string{RelHasMethod}, nil, 1, 10)
	if walk.Nodes[0].Name != "Order" || walk.Nodes[0].Repo != "shop" {
		t.Fatalf("origin node = %+v, want bare name in shop", walk.Nodes[0])
	}
	var keys []string
	for _, e := range g.ReverseEdges("Order#total") {
		keys = append(keys, e.Target)
	}
	slices.Sort(keys)
	// A name declared once keeps its plain key; the owner, declared in two
	// repositories, is reached by its scoped one.
	if want := []string{"CheckoutController", ScopedName("shop", "Order")}; !slices.Equal(keys, want) {
		t.Fatalf("callers of Order#total = %v, want %v", keys, want)
	}
	for _, e := range g.ReverseEdges(ScopedName("ledger", "Order")) {
		if e.Target != "PostingJob" && e.Target != "Report" {
			t.Fatalf("unexpected dependent %q of the ledger's Order", e.Target)
		}
	}
	if got := g.KeyOf(&Fact{Name: "Order", Repo: "ledger"}); got != ScopedName("ledger", "Order") {
		t.Fatalf("KeyOf = %q", got)
	}
	if got := g.KeyOf(&Fact{Name: "Report", Repo: "tools"}); got != "Report" {
		t.Fatalf("KeyOf of an unshared name = %q", got)
	}
}

func TestSplitName_SingleRepositoryGraphIsUnchanged(t *testing.T) {
	g, _ := buildTestGraph()
	if len(g.split) != 0 {
		t.Fatalf("a single-repository graph splits nothing, got %v", g.split)
	}
}
