package queryloops

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// reported is the set of symbols Explain actually files a finding against. The
// symbol is the first evidence entry on every insight this explainer emits.
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

// The claim set and the reported set are one pass, so they cannot disagree. This
// is the test that keeps them that way: the performance analyzer stays silent on
// a symbol this explainer claims, so a claim this explainer does not report is a
// finding nobody makes, and a report it does not claim is the same loop reported
// twice.
func TestClaimedSymbolsAreExactlyTheReportedOnes(t *testing.T) {
	inSpec := facts.Fact{Kind: facts.KindSymbol, Name: "AccessLevelSpec#setup", Repo: "app",
		File: "spec/models/access_level_spec.rb",
		Props: map[string]any{"language": "ruby", "loop_depth": 1,
			"calls_in_loop": []string{"AccessLevel.find_by"}}}

	s := store(model("AccessLevel"),
		symbol("Helpers#create_defaults", []string{"AccessLevel.find_or_create_by"}, 1),
		symbol("Report#rows", []string{"AccessLevel.find_by"}, 2),
		inSpec)

	claimed := ClaimedSymbols(s)
	if !sameSet(claimed, reported(t, s)) {
		t.Fatalf("claimed %v, reported %v", claimed, reported(t, s))
	}
	if len(claimed) == 0 {
		t.Fatal("the fixture reported nothing, so the comparison proves nothing")
	}
}

// A surface this explainer refuses to report is not one it claims. Otherwise a
// loop in a spec file would be silenced for the broad analyzer too, and neither
// would ever say anything about it.
func TestAnExcludedSurfaceIsNotClaimed(t *testing.T) {
	s := store(model("AccessLevel"),
		facts.Fact{Kind: facts.KindSymbol, Name: "AccessLevelSpec#setup", Repo: "app",
			File: "spec/models/access_level_spec.rb",
			Props: map[string]any{"language": "ruby", "loop_depth": 1,
				"calls_in_loop": []string{"AccessLevel.find_by"}}})

	if _, ok := ClaimedSymbols(s)["AccessLevelSpec#setup"]; ok {
		t.Fatal("a spec-file loop is excluded from the findings but was still claimed")
	}
}

// Nothing to say means nothing claimed: a repository with no models leaves the
// whole question to the broad analyzer rather than silencing it everywhere.
func TestNoFindingsMeansNoClaims(t *testing.T) {
	if got := ClaimedSymbols(store()); len(got) != 0 {
		t.Fatalf("an empty store claimed %v", got)
	}
}
