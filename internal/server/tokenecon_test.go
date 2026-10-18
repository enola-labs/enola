package server

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
)

func TestCapTokens(t *testing.T) {
	// Under budget: returned unchanged.
	s := "line one\nline two\n"
	if got := capTokens(s, 100, false); got != s {
		t.Errorf("under-budget input should be unchanged; got %q", got)
	}
	// Disabled cap.
	if got := capTokens(s, 0, false); got != s {
		t.Errorf("maxTokens<=0 should disable cap; got %q", got)
	}
	// Over budget: truncated on a line boundary with a notice.
	big := strings.Repeat("abcdefghij\n", 50) // 550 chars
	out := capTokens(big, 10, false)          // ~40 char budget
	if len(out) >= len(big) {
		t.Errorf("expected truncation, got len %d >= %d", len(out), len(big))
	}
	if !strings.Contains(out, "truncated") || !strings.Contains(out, "max_tokens=10") {
		t.Errorf("expected truncation notice; got:\n%s", out)
	}
	// JSON variant mentions invalid JSON.
	jsonOut := capTokens(big, 10, true)
	if !strings.Contains(jsonOut, "no longer valid JSON") {
		t.Errorf("expected JSON truncation notice; got:\n%s", jsonOut)
	}
}

func TestResolveOutputMode(t *testing.T) {
	if got := resolveOutputMode("", modeSummary); got != modeSummary {
		t.Errorf("empty should fall back to default; got %q", got)
	}
	if got := resolveOutputMode("  FULL ", modeSummary); got != modeFull {
		t.Errorf("expected normalised 'full'; got %q", got)
	}
	if !wantsSummary("summary") || wantsSummary("compact") {
		t.Error("wantsSummary mismatch")
	}
}

func TestRenderTraverseSummary(t *testing.T) {
	srv := &Server{}
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindDependency, Name: "ext.lib", Props: map[string]any{"source": "external"}})
	resp := traverseResponse{TraversalResult: facts.TraversalResult{
		Nodes: []facts.TraversalNode{
			{Name: "start", Kind: "module", Depth: 0},
			{Name: "internal/a.Foo", Kind: "symbol", File: "internal/a/foo.go", Depth: 1},
			{Name: "internal/a.Bar", Kind: "symbol", File: "internal/a/bar.go", Depth: 1},
			{Name: "ext.lib", Kind: "dependency", Depth: 1},
		},
		Edges: []facts.TraversalEdge{
			{Source: "start", Target: "internal/a.Foo", Kind: "calls"},
			{Source: "start", Target: "ext.lib", Kind: "imports"},
		},
		Stats: facts.TraversalStats{NodesVisited: 4, MaxDepthReached: 1},
	}}

	out := srv.renderTraverseSummary(store, resp, "start", "forward")
	for _, want := range []string{
		"# Traverse summary: start (forward)",
		"3 nodes across",
		"## By node kind",
		"## By relation kind",
		"## Dependency sources",
		"external: 1",
		"## Hottest modules",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("traverse summary missing %q; got:\n%s", want, out)
		}
	}
	// Summary must not list every node verbatim like compact does.
	if strings.Contains(out, "## Depth 1") {
		t.Errorf("summary should not contain per-depth node lists; got:\n%s", out)
	}
}

func TestRenderImpactSummary(t *testing.T) {
	srv := &Server{}
	resp := impactResponse{ImpactResult: facts.ImpactResult{
		Target:          "pkg.Core",
		TotalDependents: 42,
		Summary:         "42 total dependents",
		ByDepth: map[int][]facts.TraversalNode{
			1: {
				{Name: "internal/a.Foo", Kind: "symbol", File: "internal/a/foo.go", Depth: 1},
				{Name: "internal/a.Bar", Kind: "symbol", File: "internal/a/bar.go", Depth: 1},
			},
			2: {{Name: "internal/b.Baz", Kind: "symbol", File: "internal/b/baz.go", Depth: 2}},
		},
		CrossRepoImpact: []string{"go-auth"},
		Stats:           facts.TraversalStats{MaxDepthReached: 2},
	}}

	out := srv.renderImpactSummary(resp)
	for _, want := range []string{
		"# Impact summary: pkg.Core",
		"**42** total transitive dependents",
		"## Dependents by kind",
		"## By depth",
		"depth 1: 2",
		"depth 2: 1",
		"## Hotspot modules",
		"## Cross-repo impact",
		"go-auth",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("impact summary missing %q; got:\n%s", want, out)
		}
	}
}

func TestInsightsFor(t *testing.T) {
	eng, err := engine.New(config.Default())
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	eng.SetSnapshot(&facts.Snapshot{Insights: []facts.Insight{
		{Title: "Layer violation: domain -> http", Description: "internal/domain imports internal/http",
			Evidence: []facts.Evidence{{File: "internal/domain/user.go", Detail: "import"}}},
		{Title: "Circular dependency", Description: "a <-> b",
			Evidence: []facts.Evidence{{Fact: "internal/a"}}},
	}})
	srv := &Server{eng: eng}

	got := srv.insightsFor("internal/domain")
	if len(got) != 1 || !strings.Contains(got[0].Title, "Layer violation") {
		t.Errorf("expected the layer-violation insight for internal/domain; got %+v", got)
	}
	if n := srv.insightsFor("internal/a"); len(n) != 1 {
		t.Errorf("expected the cycle insight matched via evidence.Fact; got %+v", n)
	}
	if n := srv.insightsFor("nonexistent"); n != nil {
		t.Errorf("expected no insights for unmatched focus; got %+v", n)
	}
	// Nil-safe when no engine.
	if n := (&Server{}).insightsFor("x"); n != nil {
		t.Errorf("expected nil for engineless server; got %+v", n)
	}
}

func TestExploreModule_Depth2_SummaryInsights(t *testing.T) {
	store := populateTestStore()
	srv := newTestServer(store)

	var sb strings.Builder
	found := srv.exploreModule(store, "internal/server", 2, modeSummary, &sb)
	if !found {
		t.Fatal("exploreModule should find 'internal/server'")
	}
	out := sb.String()
	if !strings.Contains(out, "## Insights") {
		t.Errorf("depth=2 summary mode should emit an Insights section; got:\n%s", out)
	}
	if !strings.Contains(out, "Size metrics") {
		t.Errorf("Insights should include size metrics; got:\n%s", out)
	}
	// Summary mode replaces the raw per-symbol relations dump.
	if strings.Contains(out, "## Symbol Relations") {
		t.Errorf("summary mode should not emit the raw Symbol Relations dump; got:\n%s", out)
	}
}

// TestSummariesKeyAModuleNodeByItself: a module node's File is the module's own
// path, so taking its directory filed internal/facts and internal/factpath under
// "internal", and every extractor under "internal/extractors". The traverse summary
// then counted one module where there were two, and both summaries listed directories
// that are not modules as "hottest" or "hotspot" modules.
func TestSummariesKeyAModuleNodeByItself(t *testing.T) {
	nodes := []facts.TraversalNode{
		{Name: "internal/a", Kind: facts.KindModule, File: "internal/a", Depth: 1},
		{Name: "internal/b", Kind: facts.KindModule, File: "internal/b", Depth: 2},
		{Name: "internal/a.Foo", Kind: facts.KindSymbol, File: "internal/a/foo.go", Depth: 1},
	}
	srv := &Server{}
	traverse := srv.renderTraverseSummary(facts.NewStore(), traverseResponse{TraversalResult: facts.TraversalResult{
		Nodes: append([]facts.TraversalNode{{Name: "start", Kind: facts.KindModule, File: "start", Depth: 0}}, nodes...),
		Stats: facts.TraversalStats{NodesVisited: 4, MaxDepthReached: 2},
	}}, "start", "forward")
	impact := srv.renderImpactSummary(impactResponse{ImpactResult: facts.ImpactResult{
		Target: "start", TotalDependents: 3, Summary: "3 total dependents",
		ByDepth: map[int][]facts.TraversalNode{1: nodes[:1], 2: nodes[1:]},
		Stats:   facts.TraversalStats{MaxDepthReached: 2},
	}})

	if !strings.Contains(traverse, "3 nodes across 2 modules") {
		t.Errorf("traverse summary must count internal/a and internal/b as two modules; got:\n%s", traverse)
	}
	for name, out := range map[string]string{"traverse": traverse, "impact": impact} {
		for _, want := range []string{"- internal/a (2 ", "- internal/b (1 "} {
			if !strings.Contains(out, want) {
				t.Errorf("%s summary missing %q; got:\n%s", name, want, out)
			}
		}
		if strings.Contains(out, "- internal (") {
			t.Errorf("%s summary lists the parent directory as a module; got:\n%s", name, out)
		}
	}
}
