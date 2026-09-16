package deadmethods

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func storeOf(fs ...facts.Fact) *facts.Store {
	s := facts.NewStore()
	for _, f := range fs {
		s.Add(f)
	}
	return s
}

// reported is the set of methods Explain actually files a finding against. Both
// shapes this explainer emits carry the method as their first evidence entry.
func reported(t *testing.T, s *facts.Store) map[string]struct{} {
	t.Helper()
	got, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]struct{}{}
	for _, in := range got {
		if len(in.Evidence) == 0 {
			t.Fatalf("insight %q carries no evidence, so nothing can be claimed for it", in.Title)
		}
		out[in.Evidence[0].Symbol] = struct{}{}
	}
	return out
}

func sameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// The claim set and the reported set are one pass, so they cannot disagree. The
// orphans analyzer stays silent on a method this explainer claims, so a claim it
// does not report is a method nobody calls dead, and a report it does not claim
// is the same method reported twice.
func TestClaimedSymbolsAreExactlyTheReportedOnes(t *testing.T) {
	s := storeOf(
		method("BillingService#charge", "app/services/billing_service.rb"),
		method("BillingService#refund", "app/services/billing_service.rb"),
		ref(facts.KindSymbol, "spec/services/billing_service_spec.rb", "refund"),
	)

	claimed := ClaimedSymbols(s)
	if !sameSet(claimed, reported(t, s)) {
		t.Fatalf("claimed %v, reported %v", claimed, reported(t, s))
	}
	if len(claimed) == 0 {
		t.Fatal("the fixture reported nothing, so the comparison proves nothing")
	}
}

// Nothing to say means nothing claimed: a repository with no Ruby leaves the
// whole question to the broad analyzer rather than silencing it everywhere.
func TestNoFindingsMeansNoClaims(t *testing.T) {
	if got := ClaimedSymbols(storeOf()); len(got) != 0 {
		t.Fatalf("an empty store claimed %v", got)
	}
}
