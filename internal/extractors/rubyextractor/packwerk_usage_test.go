package rubyextractor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func packFact(name string, enforce bool, deps ...string) facts.Fact {
	f := modFact(name)
	f.Props = map[string]any{"packwerk": true, "enforce_dependencies": enforce}
	for _, d := range deps {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: d})
	}
	return f
}

func packByName(t *testing.T, ff []facts.Fact, name string) facts.Fact {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindModule && f.Name == name {
			return f
		}
	}
	t.Fatalf("no module fact %q", name)
	return facts.Fact{}
}

func propStrings(f facts.Fact, key string) []string {
	v, _ := f.PropAny(key).([]string)
	return v
}

// Orders declares billing (used) and shipping (unused), and reaches into inventory
// without declaring it. A reference between two directories of orders is no
// dependency, and the reference from a root-level directory lands on root.
func TestPackwerkUsage_DeclaredVsObserved(t *testing.T) {
	ff := []facts.Fact{
		packFact("packs/orders", true, "packs/billing", "packs/shipping/"),
		packFact("packs/billing", true),
		packFact("packs/shipping", true),
		packFact("packs/inventory", false),
		packFact("root", true, "packs/orders"),
		symFact("Orders::Checkout#run", "packs/orders/app/services/orders", facts.SymbolMethod,
			facts.Relation{Kind: facts.RelCalls, Target: "Billing::Invoice.create"},
			facts.Relation{Kind: facts.RelCalls, Target: "Inventory::Stock.reserve"},
			facts.Relation{Kind: facts.RelCalls, Target: "Orders::Order.find"}),
		symFact("Orders::Order", "packs/orders/app/models/orders", facts.SymbolClass),
		symFact("Billing::Invoice", "packs/billing/app/models/billing", facts.SymbolClass),
		symFact("Inventory::Stock", "packs/inventory/app/models/inventory", facts.SymbolClass),
		symFact("Inventory::Restock#run", "packs/inventory/app/services/inventory", facts.SymbolMethod,
			facts.Relation{Kind: facts.RelCalls, Target: "Billing::Invoice.void"}),
		symFact("HomeController#index", "app/controllers", facts.SymbolMethod,
			facts.Relation{Kind: facts.RelCalls, Target: "Orders::Order.recent"}),
	}
	resolveImports(ff, true)

	orders := packByName(t, ff, "packs/orders")
	if got, want := propStrings(orders, facts.PropPackwerkObserved), []string{"packs/billing", "packs/inventory"}; !slices.Equal(got, want) {
		t.Errorf("observed = %v, want %v", got, want)
	}
	if got, want := propStrings(orders, facts.PropPackwerkUnused), []string{"packs/shipping"}; !slices.Equal(got, want) {
		t.Errorf("unused = %v, want %v", got, want)
	}
	if got, want := propStrings(orders, facts.PropPackwerkUndeclared), []string{"packs/inventory"}; !slices.Equal(got, want) {
		t.Errorf("undeclared = %v, want %v", got, want)
	}
	ev, _ := orders.PropAny(facts.PropPackwerkUndeclaredEvidence).(map[string]any)
	sample, _ := ev["packs/inventory"].(map[string]any)
	if sample["symbol"] != "Orders::Checkout#run" || sample["file"] != "packs/orders/app/services/orders/checkout#run.rb" {
		t.Errorf("undeclared evidence = %v, want the referencing method and its file", sample)
	}

	// inventory does not enforce dependencies: what it references undeclared is no finding.
	inventory := packByName(t, ff, "packs/inventory")
	if got := propStrings(inventory, facts.PropPackwerkObserved); !slices.Equal(got, []string{"packs/billing"}) {
		t.Errorf("inventory observed = %v", got)
	}
	if _, ok := inventory.Prop(facts.PropPackwerkUndeclared); ok {
		t.Error("a package that does not enforce dependencies must carry no undeclared list")
	}

	// A root-level directory belongs to the root package, which uses what it declares.
	root := packByName(t, ff, "root")
	if got := propStrings(root, facts.PropPackwerkObserved); !slices.Equal(got, []string{"packs/orders"}) {
		t.Errorf("root observed = %v", got)
	}
	if _, ok := root.Prop(facts.PropPackwerkUnused); ok {
		t.Errorf("root uses its only dependency; unused = %v", root.PropAny(facts.PropPackwerkUnused))
	}

	billing := packByName(t, ff, "packs/billing")
	if got := propStrings(billing, facts.PropPackwerkObserved); len(got) != 0 {
		t.Errorf("billing references nothing; observed = %v", got)
	}
}

// Without a root package.yml, a directory under no package has no owner, and its
// edges are left out rather than credited to a package that does not exist.
func TestPackwerkUsage_NoRootPackage(t *testing.T) {
	ff := []facts.Fact{
		packFact("packs/orders", true),
		symFact("Orders::Order", "packs/orders/app/models/orders", facts.SymbolClass),
		symFact("Reports::Daily#run", "lib/reports", facts.SymbolMethod,
			facts.Relation{Kind: facts.RelCalls, Target: "Orders::Order.recent"}),
	}
	resolveImports(ff, true)
	orders := packByName(t, ff, "packs/orders")
	if got := propStrings(orders, facts.PropPackwerkObserved); len(got) != 0 {
		t.Errorf("observed = %v, want none", got)
	}
}

func TestReadPackageTodo_DependencyViolationsOnly(t *testing.T) {
	dir := t.TempDir()
	todo := `---
packs/inventory:
  "::Inventory::Stock":
    violations:
    - dependency
    - privacy
    files:
    - packs/orders/app/services/orders/checkout.rb
packs/billing:
  "::Billing::Ledger":
    violations:
    - privacy
    files:
    - packs/orders/app/services/orders/checkout.rb
".":
  "::Setting":
    violations:
    - dependency
    files:
    - packs/orders/app/models/orders/order.rb
`
	path := filepath.Join(dir, "package_todo.yml")
	if err := os.WriteFile(path, []byte(todo), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := readPackageTodo(path), []string{"packs/inventory", "root"}; !slices.Equal(got, want) {
		t.Errorf("todo = %v, want %v (a privacy-only entry is no dependency)", got, want)
	}
	if got := readPackageTodo(filepath.Join(dir, "missing.yml")); got != nil {
		t.Errorf("missing todo = %v, want nil", got)
	}
}
