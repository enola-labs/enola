package status

import (
	"testing"
	"time"
)

func TestComputeValueUsesRecordedCredit(t *testing.T) {
	counts := map[string]int{
		"generate_snapshot": 2,
		"query_facts":       3,
	}
	saved := map[string]int{
		"generate_snapshot": 1_200_000,
		"query_facts":       7_000,
	}

	rep := ComputeValue(counts, saved)

	if len(rep.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(rep.Tools))
	}
	// Tools are sorted by name.
	if rep.Tools[0].Tool != "generate_snapshot" {
		t.Errorf("expected first tool sorted as generate_snapshot, got %q", rep.Tools[0].Tool)
	}
	if rep.TotalCalls != 5 {
		t.Errorf("TotalCalls: got %d, want 5", rep.TotalCalls)
	}
	// The report must render what was recorded, not reprice it from counts.
	if rep.TotalTokensSaved != 1_207_000 {
		t.Errorf("TotalTokensSaved: got %d, want 1207000", rep.TotalTokensSaved)
	}
	if want := TimeSaved(1_207_000); rep.TotalTimeSaved != want {
		t.Errorf("TotalTimeSaved: got %s, want %s", rep.TotalTimeSaved, want)
	}
}

// A usage file written before per-call credit existed has counts but no token
// figures. Upgrading must not blank out that history.
func TestComputeValueLegacyFallback(t *testing.T) {
	rep := ComputeValue(map[string]int{"query_facts": 3}, nil)
	want := 3 * weightFor("query_facts") * tokensPerManualOp
	if rep.TotalTokensSaved != want {
		t.Errorf("legacy query credit: got %d, want %d", rep.TotalTokensSaved, want)
	}

	// Legacy snapshots are credited ONE median corpus regardless of call count:
	// the historical file cannot say which were first builds and which were
	// refreshes, and assuming all were first builds is the overcount this model
	// exists to remove.
	a := ComputeValue(map[string]int{"generate_snapshot": 1}, nil)
	b := ComputeValue(map[string]int{"generate_snapshot": 40}, nil)
	if a.TotalTokensSaved != b.TotalTokensSaved {
		t.Errorf("legacy snapshot credit should not scale with call count: 1 call=%d, 40 calls=%d",
			a.TotalTokensSaved, b.TotalTokensSaved)
	}
}

// Recorded credit must win over the legacy path for tools that have it, while
// tools that don't still fall back.
func TestComputeValueMixedRecordedAndLegacy(t *testing.T) {
	rep := ComputeValue(
		map[string]int{"query_facts": 3, "explore": 2},
		map[string]int{"query_facts": 42},
	)
	byTool := map[string]int{}
	for _, tv := range rep.Tools {
		byTool[tv.Tool] = tv.TokensSaved
	}
	if byTool["query_facts"] != 42 {
		t.Errorf("recorded credit ignored: got %d, want 42", byTool["query_facts"])
	}
	if want := 2 * weightFor("explore") * tokensPerManualOp; byTool["explore"] != want {
		t.Errorf("legacy credit for explore: got %d, want %d", byTool["explore"], want)
	}
}

func TestComputeValueEmpty(t *testing.T) {
	rep := ComputeValue(map[string]int{}, nil)
	if len(rep.Tools) != 0 || rep.TotalCalls != 0 || rep.TotalTokensSaved != 0 {
		t.Errorf("empty counts should yield zero-value report, got %+v", rep)
	}
}

// A failed call happened, so it is counted — but it displaced no work, so it
// earns nothing. A "run generate_snapshot first" is a call, not a saving.
func TestFailedCallEarnsNothing(t *testing.T) {
	call := ToolCall{Tool: "query_insights", OK: false}
	if got := call.TokensSaved(); got != 0 {
		t.Errorf("failed call credit: got %d, want 0", got)
	}
	failedSnapshot := ToolCall{
		Tool:     "generate_snapshot",
		OK:       false,
		Snapshot: &SnapshotValue{CorpusTokens: 30_000_000},
	}
	if got := failedSnapshot.TokensSaved(); got != 0 {
		t.Errorf("failed snapshot credit: got %d, want 0", got)
	}
}

// The response an agent has to read is subtracted, which is what makes the
// output_mode ladder visible: a summary genuinely saves more than a full dump.
func TestResponseCostIsSubtracted(t *testing.T) {
	summary := ToolCall{Tool: "query_facts", OK: true, ResponseBytes: 400}
	full := ToolCall{Tool: "query_facts", OK: true, ResponseBytes: 240_000}

	if summary.TokensSaved() <= full.TokensSaved() {
		t.Errorf("summary (%d) should save more than full (%d)",
			summary.TokensSaved(), full.TokensSaved())
	}
	// A response larger than what it displaced nets zero, never negative.
	if got := full.TokensSaved(); got < 0 {
		t.Errorf("credit must not go negative, got %d", got)
	}
}

// The same question over a bigger haystack displaces more searching, so query
// credit rises with the size of the graph it ran against — sub-linearly, and
// bounded, so a huge graph cannot run away with it.
func TestQueryCreditScalesWithGraphSize(t *testing.T) {
	q := func(corpus int) int {
		return ToolCall{Tool: "query_insights", OK: true, CorpusTokens: corpus}.TokensSaved()
	}
	small, mid, large, kernel := q(1_800_000), q(8_450_000), q(32_770_000), q(218_150_000)

	if small != weightFor("query_insights")*tokensPerManualOp {
		t.Errorf("at the reference corpus the weight should stand unscaled: got %d", small)
	}
	if small >= mid || mid >= large || large >= kernel {
		t.Errorf("credit must rise with graph size: %d %d %d %d", small, mid, large, kernel)
	}
	// Sub-linear, and bounded: the kernel is 121x the reference corpus but earns
	// under 8x the credit — it lands just below the cap, which binds at ~230M.
	ratio := float64(kernel) / float64(small)
	if ratio > maxQueryCorpusScale {
		t.Errorf("scaling exceeded its cap: %.2fx", ratio)
	}
	if ratio < 7 {
		t.Errorf("kernel-scale query should approach the cap, got %.2fx", ratio)
	}
}

// No single query can displace more work than reading everything it searched.
// Without this a query on a small repo out-earns the repo's entire source.
func TestQueryCreditCappedByGraphSize(t *testing.T) {
	corpus := 17_906
	tiny := ToolCall{Tool: "query_insights", OK: true, CorpusTokens: corpus}
	if got, ceiling := tiny.TokensSaved(), int(float64(corpus)*rediscoveryFactor); got > ceiling {
		t.Errorf("query credit %d exceeds the cost of reading the whole graph (%d)", got, ceiling)
	}
}

// An unknown corpus must not silently zero the credit via the cap.
func TestQueryCreditUnknownCorpusIsUnscaled(t *testing.T) {
	got := ToolCall{Tool: "query_facts", OK: true}.TokensSaved()
	if want := weightFor("query_facts") * tokensPerManualOp; got != want {
		t.Errorf("unknown corpus: got %d, want the unscaled weight %d", got, want)
	}
}

func TestQueryCorpusScaleBounds(t *testing.T) {
	if got := queryCorpusScale(0); got != 1 {
		t.Errorf("unknown corpus scale: got %v, want 1", got)
	}
	if got := queryCorpusScale(queryScaleReferenceCorpus / 100); got != 1 {
		t.Errorf("small corpus must floor at 1, got %v", got)
	}
	if got := queryCorpusScale(queryScaleReferenceCorpus * 2); got != 2 {
		t.Errorf("doubling the corpus should add one scale step: got %v, want 2", got)
	}
	if got := queryCorpusScale(1 << 60); got != maxQueryCorpusScale {
		t.Errorf("scale must cap: got %v, want %v", got, maxQueryCorpusScale)
	}
}

func TestSnapshotCreditScalesWithCorpus(t *testing.T) {
	small := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: 72_000, ChangedFraction: 1}}
	large := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: 33_000_000, ChangedFraction: 1}}

	if small.TokensSaved() >= large.TokensSaved() {
		t.Fatalf("credit must scale with corpus: small=%d large=%d",
			small.TokensSaved(), large.TokensSaved())
	}
	// The whole point: these are not within an order of magnitude of each other,
	// which the old flat per-call weight made them.
	if ratio := large.TokensSaved() / small.TokensSaved(); ratio < 100 {
		t.Errorf("corpus ratio collapsed to %dx, expected the ~460x spread to survive", ratio)
	}
}

// Re-snapshotting an unchanged repo is not waste — it establishes that nothing
// moved — but it is worth far less than a first build.
func TestUnchangedRefreshEarnsConfirmationOnly(t *testing.T) {
	first := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: 3_000_000, ChangedFraction: 1}}
	refresh := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: 3_000_000, Unchanged: true}}

	if refresh.TokensSaved() != refreshConfirmCredit {
		t.Errorf("unchanged refresh: got %d, want %d", refresh.TokensSaved(), refreshConfirmCredit)
	}
	if refresh.TokensSaved() >= first.TokensSaved()/100 {
		t.Errorf("unchanged refresh (%d) should be far below a first build (%d)",
			refresh.TokensSaved(), first.TokensSaved())
	}
}

// A snapshot whose id moved but whose files are byte-identical (a version or
// config bump) re-derives nothing, so it earns confirmation credit — not the
// full-rebuild credit a naive "fraction defaults to 1" would hand it.
func TestZeroChangeEarnsConfirmationOnly(t *testing.T) {
	call := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: 10_000_000, ChangedFraction: 0}}
	if got := call.TokensSaved(); got != refreshConfirmCredit {
		t.Errorf("zero-change refresh: got %d, want %d", got, refreshConfirmCredit)
	}
}

// A first build has no previous snapshot to compare against, signalled by a
// negative fraction, and must be priced as re-deriving the whole corpus.
func TestNegativeFractionIsFirstBuild(t *testing.T) {
	first := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: 1_000_000, ChangedFraction: -1}}
	want := int(1_000_000 * rediscoveryFactor)
	if got := first.TokensSaved(); got != want {
		t.Errorf("first build: got %d, want %d", got, want)
	}
}

func TestPartialRefreshScalesWithChange(t *testing.T) {
	corpus := 3_000_000
	small := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: corpus, ChangedFraction: 0.01}}
	big := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: corpus, ChangedFraction: 0.5}}
	full := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: corpus, ChangedFraction: 1}}

	if small.TokensSaved() >= big.TokensSaved() || big.TokensSaved() >= full.TokensSaved() {
		t.Errorf("refresh credit should rise with changed fraction: %d < %d < %d",
			small.TokensSaved(), big.TokensSaved(), full.TokensSaved())
	}
}

func TestAppendEarnsCrossRepoPremium(t *testing.T) {
	alone := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: 500_000, ChangedFraction: 1}}
	appended := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{
			CorpusTokens: 500_000, ChangedFraction: 1,
			Append: true, PriorCorpusTokens: 10_000_000,
		}}

	if appended.TokensSaved() <= alone.TokensSaved() {
		t.Errorf("append should out-earn an independent snapshot: %d vs %d",
			appended.TokensSaved(), alone.TokensSaved())
	}
}

// A small repo joining a large graph must not out-earn its own source. Reading
// every line of it is the ceiling on what an agent could have ingested for it.
func TestCreditNeverExceedsOwnCorpus(t *testing.T) {
	tiny := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{
			CorpusTokens: 4_072, ChangedFraction: -1,
			Append: true, PriorCorpusTokens: 50_000_000,
		}}
	if got := tiny.TokensSaved(); got > 4_072 {
		t.Errorf("credit %d exceeds the repo's own corpus of 4072", got)
	}
}

// The property that ceiling buys: across a whole graph, cumulative snapshot
// credit never claims more tokens than the code actually contains.
func TestClusterCreditStaysUnderCorpus(t *testing.T) {
	corpora := []int{935_633, 1_518_104, 622_337, 415_054, 2_593_805, 2_129_228, 4_072, 229_116}
	total, prior := 0, 0
	for i, c := range corpora {
		call := ToolCall{Tool: "generate_snapshot", OK: true,
			Snapshot: &SnapshotValue{
				CorpusTokens: c, ChangedFraction: -1,
				Append: i > 0, PriorCorpusTokens: prior,
			}}
		total += call.TokensSaved()
		prior += c
	}
	if total > prior {
		t.Errorf("cluster credit %d exceeds cluster corpus %d", total, prior)
	}
}

// Past a context window the counterfactual is not expensive, it is impossible —
// renderers flag those rows rather than presenting the number alone.
func TestBeyondContext(t *testing.T) {
	within := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: 500_000}}
	beyond := ToolCall{Tool: "generate_snapshot", OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: 600_000, PriorCorpusTokens: 10_000_000}}

	if within.BeyondContext() {
		t.Error("a corpus under the window must not be flagged")
	}
	if !beyond.BeyondContext() {
		t.Error("a combined corpus over the window must be flagged")
	}
	// A query tool never carries the flag.
	if (ToolCall{Tool: "query_facts", OK: true}).BeyondContext() {
		t.Error("query tools must never be flagged beyond-context")
	}
}

// The repo-local .enola metadata outlives the usage ledger: it survives clearing
// ~/.enola and arrives with a fresh clone. So a snapshot the server reports as
// "unchanged" must still be priced as a first build when this ledger has never
// credited that repo — otherwise a stale meta file suppresses the credit forever.
func TestStaleRepoMetaDoesNotSuppressFirstBuild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()

	tr := NewTracker(repo)
	tr.SetStartTime(time.Now())

	// What the server reports when a leftover meta matches the current content.
	unchanged := ToolCall{
		Tool: "generate_snapshot", Repo: repo, OK: true,
		Snapshot: &SnapshotValue{CorpusTokens: 5_000_000, Unchanged: true},
	}
	tr.Record(unchanged)

	first := tr.GetStatus(repo).TokensSaved[snapshotTool]
	if want := int(5_000_000 * rediscoveryFactor); first != want {
		t.Fatalf("first build against an empty ledger: got %d, want %d", first, want)
	}

	// The SECOND identical call is a genuine refresh — the ledger has now seen
	// this repo — so it earns confirmation credit, not another corpus.
	tr.Record(unchanged)
	total := tr.GetStatus(repo).TokensSaved[snapshotTool]
	if got := total - first; got != refreshConfirmCredit {
		t.Errorf("second call credit: got %d, want %d", got, refreshConfirmCredit)
	}
}

func TestWeightForKnownTools(t *testing.T) {
	// Every query tool in the catalog should have an explicit (non-default)
	// weight so the value model is intentional rather than falling through.
	// generate_snapshot is deliberately absent: its value is corpus-derived.
	for _, tool := range []string{
		"explore", "query_facts", "show_symbol", "traverse",
		"find_path", "impact_analysis", "coverage_report", "query_insights",
		"set_baseline", "diff_snapshot", "snapshot_receipt", "compare_receipts",
	} {
		if _, ok := toolWeights[tool]; !ok {
			t.Errorf("tool %q has no explicit weight in toolWeights", tool)
		}
	}
	if _, ok := toolWeights["generate_snapshot"]; ok {
		t.Error("generate_snapshot must not have a flat weight — its value is corpus-derived")
	}
}

// A tool with no entry in the table falls back to defaultWeight rather than
// scoring zero, so an unpriced tool is never counted as worthless.
func TestUnpricedToolFallsBackToDefaultWeight(t *testing.T) {
	if got := weightFor("not_a_registered_tool"); got != defaultWeight {
		t.Errorf("unpriced tool weight: got %d, want defaultWeight %d", got, defaultWeight)
	}
	if got := weightFor("query_insights"); got != 30 {
		t.Errorf("query_insights weight: got %d, want 30", got)
	}
}

// Time is derived from tokens in exactly one place, so the two headline figures
// can never drift apart.
func TestTimeSavedTracksTokens(t *testing.T) {
	if TimeSaved(0) != 0 {
		t.Errorf("zero tokens must be zero time, got %s", TimeSaved(0))
	}
	if TimeSaved(2_000_000) <= TimeSaved(1_000_000) {
		t.Error("time must rise with tokens")
	}
}
