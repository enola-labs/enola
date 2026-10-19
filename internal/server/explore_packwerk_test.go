package server

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestExploreModule_PackwerkDeclaredVsReferenced(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "packs/orders",
			Props: map[string]any{
				"language": "ruby", "packwerk": true,
				facts.PropPackwerkObserved:   []string{"packs/billing", "packs/inventory"},
				facts.PropPackwerkUnused:     []string{"packs/shipping"},
				facts.PropPackwerkUndeclared: []any{"packs/inventory"},
				facts.PropPackwerkUndeclaredEvidence: map[string]any{
					"packs/inventory": map[string]any{"file": "packs/orders/app/services/orders/checkout.rb", "symbol": "Orders::Checkout#run"},
				},
			},
			Relations: []facts.Relation{
				{Kind: facts.RelDependsOn, Target: "packs/billing"},
				{Kind: facts.RelDependsOn, Target: "packs/shipping"},
			}},
		facts.Fact{Kind: facts.KindModule, Name: "packs/billing",
			Props: map[string]any{"language": "ruby", "packwerk": true, facts.PropPackwerkObserved: []string{}}},
		facts.Fact{Kind: facts.KindModule, Name: "packs/returns",
			Props: map[string]any{"language": "ruby", "packwerk": true, facts.PropPackwerkObserved: []string{"packs/orders"}}},
	)
	srv := newTestServer(store)

	var sb strings.Builder
	if !srv.exploreModule(store, "packs/orders", 1, modeSummary, &sb) {
		t.Fatal("exploreModule should find packs/orders")
	}
	out := sb.String()
	for _, want := range []string{
		"## Packwerk: declared vs referenced",
		"- Declared: 2; referenced by this package's code: 2",
		"- Declared, never referenced (1): packs/shipping",
		"  - packs/inventory — first reference: Orders::Checkout#run (packs/orders/app/services/orders/checkout.rb)",
		"- Packages whose code references this one (1): packs/returns",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// A module that is no packwerk package gets no such section.
	store.Add(facts.Fact{Kind: facts.KindModule, Name: "lib/reports", Props: map[string]any{"language": "ruby"}})
	sb.Reset()
	srv.exploreModule(store, "lib/reports", 1, modeSummary, &sb)
	if strings.Contains(sb.String(), "Packwerk") {
		t.Errorf("non-package module rendered a packwerk section:\n%s", sb.String())
	}
}
