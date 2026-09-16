package perf

import (
	"testing"

	"github.com/enola-labs/enola/internal/explainers/queryloops"
	"github.com/enola-labs/enola/internal/facts"
)

// The rule itself, isolated from how query-loops decides what to claim: the same
// function, reported when unclaimed and silent when claimed.
func TestClaimedFunctionReportsNoCallInLoop(t *testing.T) {
	storage := map[string]bool{"app/data.LoadUser": true}
	fn := func(claimed bool) []funcInfo {
		return []funcInfo{{Name: "app.Handler", File: "app/h.go", Line: 5, LoopDepth: 1, LoopCount: 1,
			Claimed: claimed,
			Calls:   []string{"app/data.LoadUser"}, CallsInLoop: []string{"app/data.LoadUser"}}}
	}

	if _, ok := findFinding(analyze(fn(false), storage, nil, nil), "app.Handler", "call-in-loop"); !ok {
		t.Fatal("unclaimed: expected a call-in-loop finding, so the claimed case below means something")
	}
	if f, ok := findFinding(analyze(fn(true), storage, nil, nil), "app.Handler", "call-in-loop"); ok {
		t.Fatalf("claimed: query-loops reports this one, so it should be deferred; got %+v", f)
	}
}

// Deference is scoped to the one question both ask. A nested loop is not a query
// per iteration, and query-loops never claims to have looked at it.
func TestClaimStillReportsOtherFindingKinds(t *testing.T) {
	funcs := []funcInfo{{Name: "app.Handler", File: "app/h.go", Line: 5, LoopDepth: 2, LoopCount: 2,
		Claimed: true,
		Calls:   []string{"app/data.LoadUser"}, CallsInLoop: []string{"app/data.LoadUser"}}}

	if _, ok := findFinding(analyze(funcs, map[string]bool{"app/data.LoadUser": true}, nil, nil),
		"app.Handler", "nested-loop"); !ok {
		t.Fatal("a claimed symbol should still report its nested loop")
	}
}

// The wiring: collect populates Claimed from query-loops itself, so the two cannot
// be connected in the test and disconnected in the product.
func TestCollectTakesItsClaimsFromQueryLoops(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindStorage, Name: "AccessLevel", Repo: "app",
		Props: map[string]any{"storage_kind": "model", "language": "ruby"}})
	store.Add(facts.Fact{Kind: facts.KindSymbol, Name: "Helpers#create_defaults", Repo: "app",
		File: "app/services/helpers.rb",
		Props: map[string]any{"language": "ruby", "symbol_kind": facts.SymbolFunc,
			"loop_depth": 1, "loop_count": 1,
			"calls_in_loop": []string{"AccessLevel.find_or_create_by"}}})

	if _, ok := queryloops.ClaimedSymbols(store)["Helpers#create_defaults"]; !ok {
		t.Fatal("fixture no longer produces a query-loops claim, so it proves nothing")
	}
	funcs, _, _, _ := collect(store)
	for _, f := range funcs {
		if f.Name == "Helpers#create_defaults" {
			if !f.Claimed {
				t.Fatal("query-loops claims this symbol but collect did not mark it")
			}
			return
		}
	}
	t.Fatal("collect dropped the symbol entirely")
}
