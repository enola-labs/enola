package packwerk

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestUndeclaredAndUnusedArePerPackageFindings(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "packs/orders", File: "packs/orders/package.yml",
		Props: map[string]any{
			"packwerk":                   true,
			facts.PropPackwerkUndeclared: []string{"packs/inventory", "packs/tax"},
			facts.PropPackwerkTodo:       []string{"packs/tax"},
			facts.PropPackwerkUnused:     []string{"packs/shipping"},
			facts.PropPackwerkObserved:   []string{"packs/billing", "packs/inventory", "packs/tax"},
			facts.PropPackwerkUndeclaredEvidence: map[string]any{
				"packs/inventory": map[string]any{"file": "packs/orders/app/services/orders/checkout.rb", "symbol": "Orders::Checkout#run", "line": 12},
			},
		}})
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "packs/billing", File: "packs/billing/package.yml",
		Props: map[string]any{"packwerk": true, facts.PropPackwerkObserved: []string{}}})
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "app/models", Props: map[string]any{facts.PropPackwerkUnused: []string{"x"}}})

	got, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want an undeclared and an unused finding for orders only, got %d: %+v", len(got), got)
	}

	und := got[0]
	if und.Title != "Undeclared packwerk dependencies: packs/orders → packs/inventory" {
		t.Errorf("title = %q", und.Title)
	}
	if und.Confidence >= 1 || len(und.Evidence) != 1 {
		t.Fatalf("confidence %.2f, evidence %+v", und.Confidence, und.Evidence)
	}
	if ev := und.Evidence[0]; ev.File != "packs/orders/app/services/orders/checkout.rb" || ev.Symbol != "Orders::Checkout#run" || ev.Line != 12 {
		t.Errorf("evidence = %+v, want the referencing symbol at its file and line", ev)
	}
	if !strings.Contains(und.Description, "package_todo.yml") {
		t.Errorf("an acknowledged dependency should be counted in the description: %s", und.Description)
	}

	un := got[1]
	if un.Title != "Unused packwerk dependencies: packs/orders declares packs/shipping" || un.Confidence >= und.Confidence {
		t.Errorf("unused = %q at %.2f", un.Title, un.Confidence)
	}
	if un.Evidence[0].File != "packs/orders/package.yml" {
		t.Errorf("unused evidence should point at package.yml: %+v", un.Evidence[0])
	}
}

// A restored snapshot delivers list props as []any and numbers as float64.
func TestRoundTrippedPropsAreRead(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "packs/orders", File: "packs/orders/package.yml",
		Props: map[string]any{
			"packwerk":                   true,
			facts.PropPackwerkUndeclared: []any{"packs/inventory"},
			facts.PropPackwerkUndeclaredEvidence: map[string]any{
				"packs/inventory": map[string]any{"file": "a.rb", "symbol": "A#b", "line": float64(7)},
			},
		}})
	got, _ := New().Explain(context.Background(), s)
	if len(got) != 1 || got[0].Evidence[0].Line != 7 {
		t.Fatalf("got %+v", got)
	}
}

func TestTitleCapsTheListItNames(t *testing.T) {
	if got := sample([]string{"a", "b", "c", "d", "e"}); got != "a, b, c (+2 more)" {
		t.Errorf("sample = %q", got)
	}
}
