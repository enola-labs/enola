package orphans

import (
	"testing"

	"github.com/enola-labs/enola/internal/explainers/deadmethods"
	"github.com/enola-labs/enola/internal/facts"
)

func rubyMethod(name, file string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Repo: "app",
		Props: map[string]any{"language": "ruby", "symbol_kind": "method"}}
}

// The rule itself, isolated from how dead-methods decides what to claim.
func TestClaimedSymbolIsNotReportedAsAnOrphan(t *testing.T) {
	sym := symInput{Name: "BillingService#charge", File: "app/services/billing_service.rb",
		Kind: facts.SymbolFunc, Package: "app/services"}

	unclaimed := classify([]symInput{sym}, make(refIndex),
		options{Mode: "both", Visibility: "all"})
	if len(unclaimed) == 0 {
		t.Fatal("unclaimed: expected an orphan, so the claimed case below means something")
	}

	sym.Claimed = true
	if got := classify([]symInput{sym}, make(refIndex),
		options{Mode: "both", Visibility: "all"}); len(got) != 0 {
		t.Fatalf("claimed: dead-methods reports this one, so it should be deferred; got %+v", got)
	}
}

// The wiring: collect populates Claimed from dead-methods itself.
func TestCollectTakesItsClaimsFromDeadMethods(t *testing.T) {
	store := facts.NewStore()
	store.Add(rubyMethod("BillingService#charge", "app/services/billing_service.rb"))
	// dead-methods refuses to speak against an empty call index: a graph that saw
	// no calls at all would make every method look dead. One unrelated caller is
	// what makes the index real, and leaves `charge` genuinely unnamed.
	store.Add(facts.Fact{Kind: facts.KindSymbol, Name: "app/services/other.rb",
		File: "app/services/other.rb", Repo: "app",
		Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "refund"}}})

	if _, ok := deadmethods.ClaimedSymbols(store)["BillingService#charge"]; !ok {
		t.Fatal("fixture no longer produces a dead-methods claim, so it proves nothing")
	}
	syms, _ := collect(store)
	for _, s := range syms {
		if s.Name == "BillingService#charge" {
			if !s.Claimed {
				t.Fatal("dead-methods claims this symbol but collect did not mark it")
			}
			return
		}
	}
	t.Fatal("collect dropped the symbol entirely")
}
