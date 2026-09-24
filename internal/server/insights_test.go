package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func sampleInsights() []facts.Insight {
	return []facts.Insight{
		{
			Source:     "unused-routes",
			Title:      "Unused endpoint candidates: 562 route(s) in golf have no caller among loaded clients",
			Confidence: 0.6,
			Evidence: []facts.Evidence{
				{Fact: "/api/settings/recommendations", File: "golf/internal/bootstrap/server.go", Detail: "no loaded client calls this route"},
			},
			Actions: []string{"Verify against out-of-snapshot consumers before removing."},
		},
		{
			Source:     "cycles",
			Title:      "Circular dependency among 3 modules in golf-ui",
			Confidence: 0.9,
			Evidence:   []facts.Evidence{{Fact: "golf-ui/src/a"}},
		},
		{
			Source:     "god-class",
			Title:      "High fan-in symbol",
			Confidence: 0.5,
		},
	}
}

func TestFilterInsights(t *testing.T) {
	all := sampleInsights()

	if got := filterInsights(all, "unused-routes", "", 0, true); len(got) != 1 || got[0].Source != "unused-routes" {
		t.Errorf("explainer filter: want 1 unused-routes insight, got %+v", got)
	}
	if got := filterInsights(all, "UNUSED-ROUTES", "", 0, true); len(got) != 1 {
		t.Errorf("explainer filter should be case-insensitive; got %d", len(got))
	}
	if got := filterInsights(all, "", "", 0.8, true); len(got) != 1 || got[0].Source != "cycles" {
		t.Errorf("min_confidence filter: want only the 0.9 cycles insight, got %+v", got)
	}
	// repo matches the cycles insight via its evidence path segment (golf-ui/src/a).
	if got := filterInsights(all, "", "golf-ui", 0, true); len(got) != 1 || got[0].Source != "cycles" {
		t.Errorf("repo filter golf-ui: want the cycles insight, got %+v", got)
	}
	if got := filterInsights(all, "", "nonexistent", 0, true); got != nil {
		t.Errorf("repo filter with no match should be empty; got %+v", got)
	}
	if got := filterInsights(all, "", "", 0, true); len(got) != 3 {
		t.Errorf("no filters should return all; got %d", len(got))
	}
}

// TestFilterInsightsNoCrossRepoLeak guards against the substring over-matching
// bug: repo="golf" must not return insights whose evidence lives under sibling
// repos that merely share the "golf" token (golf-ui, my-golf-journal-*).
func TestFilterInsightsNoCrossRepoLeak(t *testing.T) {
	all := []facts.Insight{
		{
			Source: "unused-routes",
			Title:  "Unused endpoint candidates in golf",
			Evidence: []facts.Evidence{
				{Fact: "/api/x", File: "golf/internal/bootstrap/server.go"},
			},
		},
		{
			Source:   "god-class",
			Title:    "MyGolfJournal.MyGolfJournalApp",
			Evidence: []facts.Evidence{{File: "golf-ui/fitness-functions.js"}},
		},
		{
			Source:   "cycles",
			Title:    "Circular dependency",
			Evidence: []facts.Evidence{{Fact: "my-golf-journal-ios/Sources/App"}},
		},
	}

	got := filterInsights(all, "", "golf", 0, true)
	if len(got) != 1 || got[0].Source != "unused-routes" {
		t.Fatalf("repo filter golf should return only the golf insight, got %+v", got)
	}
}

// TestFilterInsightsSingleRepoFallback verifies that single-repo snapshots
// (multiRepo=false), where evidence paths are not repo-prefixed, still match via
// the legacy title substring heuristic.
func TestFilterInsightsSingleRepoFallback(t *testing.T) {
	all := []facts.Insight{
		{
			Source:   "unused-routes",
			Title:    "562 route(s) in golf have no caller",
			Evidence: []facts.Evidence{{Fact: "/api/x", File: "internal/bootstrap/server.go"}},
		},
	}

	if got := filterInsights(all, "", "golf", 0, false); len(got) != 1 {
		t.Errorf("single-repo fallback: want the golf insight via title match, got %+v", got)
	}
	if got := filterInsights(all, "", "golf", 0, true); got != nil {
		t.Errorf("multi-repo strict match should not match unprefixed evidence; got %+v", got)
	}
}

func TestRenderInsightsSummary(t *testing.T) {
	out := renderInsightsSummary(sampleInsights())
	for _, want := range []string{"Found **3** insight(s)", "## By explainer", "unused-routes", "0.60", "Unused endpoint candidates"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q; got:\n%s", want, out)
		}
	}
}

func TestRenderInsightsCompact(t *testing.T) {
	out := renderInsightsCompact(filterInsights(sampleInsights(), "unused-routes", "", 0, true))
	for _, want := range []string{"explainer: unused-routes", "confidence: 0.60", "no loaded client calls this route", "suggested actions"} {
		if !strings.Contains(out, want) {
			t.Errorf("compact missing %q; got:\n%s", want, out)
		}
	}
}

func TestRenderQuerySummary_SurfacesUnmatchedFlag(t *testing.T) {
	results := []facts.Fact{
		{Kind: facts.KindRoute, Name: "/a", Props: map[string]any{"unmatched_by_clients": true}},
		{Kind: facts.KindRoute, Name: "/b", Props: map[string]any{"unmatched_by_clients": true}},
		{Kind: facts.KindRoute, Name: "/c"},
	}
	out := renderQuerySummary(results, len(results))
	if !strings.Contains(out, "## Flags") || !strings.Contains(out, "unmatched_by_clients=true: 2") {
		t.Errorf("summary should surface the unmatched_by_clients flag count; got:\n%s", out)
	}
}

// TestRenderInsightsSummary_HeadlineReflectsTheFilter composes the filter with the
// summary, which no test in either repo did before. It pins the invariant that the
// analyze_performance tool violated (bug new/56): a tool that filters its
// results and then summarizes must count the FILTERED set, not the corpus. There,
// the summary was built before the filter ran, so package="androidTest" printed the
// repo-wide total (61 findings) above an empty table — a real number, and the answer
// to a question the caller never asked.
//
// query_insights is correct by construction — renderInsightsSummary is handed only
// the matched slice and does not have snap.Insights in scope, so it cannot count the
// wrong thing. This test is what keeps that true.
func TestRenderInsightsSummary_HeadlineReflectsTheFilter(t *testing.T) {
	all := sampleInsights() // 3 insights: unused-routes, cycles, god-class

	tests := []struct {
		name         string
		explainer    string
		minConf      float64
		wantHeadline string
		wantAbsent   string
	}{
		{"unfiltered", "", 0, "Found **3** insight(s)", ""},
		{"by explainer", "cycles", 0, "Found **1** insight(s)", "unused-routes"},
		{"by confidence", "", 0.8, "Found **1** insight(s)", "god-class"},
		{"explainer with no matches", "layers", 0, "Found **0** insight(s)", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched := filterInsights(all, tt.explainer, "", tt.minConf, false)
			out := renderInsightsSummary(matched)

			if !strings.Contains(out, tt.wantHeadline) {
				t.Errorf("headline does not reflect the filter: want %q in\n%s", tt.wantHeadline, out)
			}
			// The by-explainer tally must be over the filtered set too — a breakdown
			// computed over the corpus is the same defect one line lower.
			if tt.wantAbsent != "" && strings.Contains(out, tt.wantAbsent) {
				t.Errorf("filtered-out explainer %q leaked into the summary:\n%s", tt.wantAbsent, out)
			}
		})
	}
}

// TestRenderQuerySummary_HeadlineIsTheFilteredTotal pins the same invariant for
// query_facts, whose headline takes `total` as an argument — so unlike
// query_insights it *could* be handed the wrong number. store.QueryAdvanced returns
// the post-filter match count; this asserts the renderer reports that, and not the
// size of the store.
func TestRenderQuerySummary_HeadlineIsTheFilteredTotal(t *testing.T) {
	results := []facts.Fact{
		{Kind: facts.KindRoute, Name: "GET /a", File: "a.go"},
		{Kind: facts.KindRoute, Name: "GET /b", File: "b.go"},
	}
	// The filtered total is 2, even though the store holds far more.
	out := renderQuerySummary(results, len(results))
	if !strings.Contains(out, "Found **2** matching facts.") {
		t.Errorf("headline must be the filtered total, got:\n%s", out)
	}
	if strings.Contains(out, "Found **500**") || strings.Contains(out, "Found **0**") {
		t.Errorf("headline reported something other than the filtered total:\n%s", out)
	}
}

// The store pages at 500; summary mode must count every match, or a breakdown
// reads "route: 500" beside "Found 1200 matching facts".
func TestQueryAllPages_CountsEveryMatch(t *testing.T) {
	store := facts.NewStore()
	for i := 0; i < 1200; i++ {
		store.Add(facts.Fact{Kind: facts.KindRoute, Name: fmt.Sprintf("/r%d", i), File: "api.py"})
	}
	results, total := queryAllPages(store, facts.QueryOpts{Kind: facts.KindRoute})
	if total != 1200 || len(results) != 1200 {
		t.Fatalf("got %d of %d, want 1200 of 1200", len(results), total)
	}
	out := renderQuerySummary(results, total)
	if !strings.Contains(out, "- route: 1200") || strings.Contains(out, "sample") {
		t.Errorf("summary should count all 1200 without a sample note; got:\n%s", out)
	}
	if results, _ := queryAllPages(store, facts.QueryOpts{Kind: facts.KindRoute, Offset: 1000}); len(results) != 200 {
		t.Errorf("from offset 1000: got %d, want 200", len(results))
	}
}
