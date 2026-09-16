package common

import "testing"

type item struct {
	repo string
	n    int
}

func items(repo string, n int) []item {
	out := make([]item, n)
	for i := range out {
		out[i] = item{repo: repo, n: i}
	}
	return out
}

func repoOf(i item) string { return i.repo }

func TestCapKeepsEverythingUnderTheLimit(t *testing.T) {
	kept, omitted := Cap(items("a", 3))
	if len(kept) != 3 || omitted != 0 {
		t.Fatalf("kept %d omitted %d, want 3 and 0", len(kept), omitted)
	}
}

func TestCapTruncatesAndCountsTheRest(t *testing.T) {
	kept, omitted := Cap(items("a", MaxIndividualInsights+7))
	if len(kept) != MaxIndividualInsights || omitted != 7 {
		t.Fatalf("kept %d omitted %d, want %d and 7", len(kept), omitted, MaxIndividualInsights)
	}
}

// Cap2 shares ONE budget with an already-capped first list, so an explainer
// reporting two shapes of the same finding reports one list's worth in total rather
// than one per shape.
func TestCap2SharesTheBudget(t *testing.T) {
	first, _ := Cap(items("a", 30))
	second, omitted := Cap2(items("a", 40), len(first))
	if len(first)+len(second) != MaxIndividualInsights {
		t.Fatalf("two shapes reported %d, want the shared budget of %d",
			len(first)+len(second), MaxIndividualInsights)
	}
	if omitted != 40-(MaxIndividualInsights-30) {
		t.Fatalf("omitted %d, want %d", omitted, 40-(MaxIndividualInsights-30))
	}
}

func TestCap2WithNoBudgetLeftKeepsNothing(t *testing.T) {
	kept, omitted := Cap2(items("b", 5), MaxIndividualInsights)
	if len(kept) != 0 || omitted != 5 {
		t.Fatalf("kept %d omitted %d, want 0 and 5", len(kept), omitted)
	}
}

// The failure this exists to prevent: a snapshot holding several repositories spends
// one budget in rank order, so whichever repository ranks first takes all of it and
// the rest are reported as clean when nobody looked at them. Silence that means "not
// examined" is indistinguishable from silence that means "nothing found".
func TestCapByRepoDoesNotLetOneRepoStarveAnother(t *testing.T) {
	ranked := append(items("noisy", 200), items("quiet", 3)...)

	kept, omitted := CapByRepo(ranked, repoOf)

	got := map[string]int{}
	for _, i := range kept {
		got[i.repo]++
	}
	if got["quiet"] != 3 {
		t.Errorf("the quiet repo kept %d of its 3 findings; a noisy sibling consumed the budget", got["quiet"])
	}
	if got["noisy"] != MaxIndividualInsights {
		t.Errorf("the noisy repo kept %d, want its full budget of %d", got["noisy"], MaxIndividualInsights)
	}
	if omitted != 200-MaxIndividualInsights {
		t.Errorf("omitted %d, want %d", omitted, 200-MaxIndividualInsights)
	}
}

// A single-repo snapshot labels everything the same way (or not at all), so the
// per-repo form must degenerate to the plain one rather than behave differently.
func TestCapByRepoMatchesCapForOneRepo(t *testing.T) {
	for _, label := range []string{"", "solo"} {
		ranked := items(label, MaxIndividualInsights+9)
		a, aOmit := Cap(ranked)
		b, bOmit := CapByRepo(ranked, repoOf)
		if len(a) != len(b) || aOmit != bOmit {
			t.Errorf("repo %q: Cap kept %d/omit %d, CapByRepo kept %d/omit %d",
				label, len(a), aOmit, len(b), bOmit)
		}
	}
}
