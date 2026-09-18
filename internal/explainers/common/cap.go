package common

// MaxIndividualInsights is how many findings one explainer reports individually
// before the remainder becomes a single rollup. It is the repository's one
// definition of that number.
//
// The reason is readability under a policy, not tidiness. An explainer that files
// one insight per finding can bury every other explainer in the list: uncapped,
// the performance analyzer produced 1,254 of enola's own 1,385 findings, and on a
// large Rails application dead-methods produces 955. A reader facing that does
// what anyone does with a wall of output, which is stop reading it, and
// docs/EXPLAINERS.md is titled around exactly that failure.
//
// THE CAP IS NOT FREE, and the cost decides which explainers may use it. A diff
// identifies findings by source and title (internal/diff.findingKey), so a finding
// past the cap is invisible to diff_snapshot and therefore to `check --fail-on`,
// except that the rollup's own number moves. For a CANDIDATE that is an acceptable
// trade: the reader was going to verify it anyway, and the full set is one tool
// call away. For a PROOF it is not. A repository carrying 231 dependency cycles
// would rank a newly introduced one past the cap and the gate would never see it,
// which turns a build gate into one that silently stops firing. `cycles`, declared
// `layers`, `intent` and `constraints` are therefore never capped.
//
// The budget is spent PER REPOSITORY, not per snapshot. A snapshot can hold several,
// and one budget spent in rank order is spent by whichever repository ranks first:
// on a two-repository fixture with 200 dead methods in one and 3 in the other, the
// second got nothing, so a reader of a cluster snapshot would conclude a repository
// was clean when nobody had looked at it. That is the same failure as a silent
// exclusion, one level up.
//
// An explainer that caps and still needs its findings visible to a diff has a
// second channel: annotate the facts. Props are diffed as fact attributes rather
// than as insights, so orphans stamps orphan_class on the symbol and metrics
// stamps the Martin measures on the module, and a change past the cap still shows
// up on the fact it is about.
const MaxIndividualInsights = 50

// Cap splits an already-RANKED list into the part reported individually and the
// number left over, so the caller can word its own rollup.
//
// Ranking is the caller's, deliberately: only the explainer knows which of its
// findings are worth the budget — orphans spends it on the most actionable
// candidates, performance on the most severe — and a shared function that sorted
// for them would spend it alphabetically.
func Cap[T any](ranked []T) (kept []T, omitted int) {
	if len(ranked) <= MaxIndividualInsights {
		return ranked, 0
	}
	return ranked[:MaxIndividualInsights], len(ranked) - MaxIndividualInsights
}

// CapByRepo caps a RANKED list to MaxIndividualInsights per repository, preserving
// the ranking within each, and returns how many it left over in total.
//
// A snapshot can hold several repositories, and a single budget spent in rank order
// is spent by whichever repository ranks first. Measured on a two-repository
// fixture: 200 dead methods in one and 3 in the other, and the second got NOTHING —
// a reader of a cluster snapshot would conclude a repository was clean when nobody
// had looked at it. Silence that means "not examined" is the failure this whole
// file exists to avoid, arriving one level up.
//
// repoOf returns the repository an item belongs to. A single-repo snapshot returns
// the same label (or "") for everything and degenerates to Cap.
func CapByRepo[T any](ranked []T, repoOf func(T) string) (kept []T, omitted int) {
	seen := map[string]int{}
	kept = make([]T, 0, len(ranked))
	for _, item := range ranked {
		r := repoOf(item)
		if seen[r] >= MaxIndividualInsights {
			omitted++
			continue
		}
		seen[r]++
		kept = append(kept, item)
	}
	return kept, omitted
}
