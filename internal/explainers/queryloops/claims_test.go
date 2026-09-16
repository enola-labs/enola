package queryloops

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/explainers/common"

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
		// A rollup stands for the findings past the cap rather than for one symbol,
		// so it carries no evidence and claims nothing. Everything else must.
		if len(in.Evidence) == 0 {
			if !strings.HasPrefix(in.Title, "Additional ") {
				t.Fatalf("insight %q carries no evidence and is not a rollup", in.Title)
			}
			continue
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

// The cap must not become a hole. A claim silences the performance analyzer on
// that symbol, so if this explainer capped its report but still claimed every
// candidate, the loops past the cap would be reported by NEITHER. The Phase 0
// fixtures were all smaller than the cap, so only a fixture larger than it can
// fail this.
func TestClaimsDoNotOutrunTheCap(t *testing.T) {
	fs := []facts.Fact{model("AccessLevel")}
	const n = common.MaxIndividualInsights * 2
	for i := 0; i < n; i++ {
		fs = append(fs, symbol(fmt.Sprintf("Helpers#run_%03d", i),
			[]string{"AccessLevel.find_by"}, 1))
	}
	s := store(fs...)

	claimed := ClaimedSymbols(s)
	rep := reported(t, s)
	if len(rep) > common.MaxIndividualInsights+1 {
		t.Fatalf("reported %d findings, want at most the cap plus one rollup", len(rep))
	}
	if len(claimed) > common.MaxIndividualInsights {
		t.Fatalf("claimed %d symbols but reports at most %d — the rest are silenced for "+
			"the performance analyzer and reported by nobody", len(claimed), common.MaxIndividualInsights)
	}
	for sym := range claimed {
		if _, ok := rep[sym]; !ok {
			t.Fatalf("claimed %q without reporting it", sym)
		}
	}
}
