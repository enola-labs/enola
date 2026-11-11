package godclass

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

// makeStore builds a store where each name in callers gets a symbol fact with a
// "calls" relation to hub, plus the hub symbol itself, then builds the graph.
func makeStore(hub string, callers []string, extraSymbols []string) *facts.Store {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: hub, File: "core/hub.go"})
	for _, c := range callers {
		s.Add(facts.Fact{
			Kind:      facts.KindSymbol,
			Name:      c,
			File:      "callers/" + strings.ReplaceAll(c, "/", "_") + ".go",
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: hub}},
		})
	}
	for _, x := range extraSymbols {
		s.Add(facts.Fact{Kind: facts.KindSymbol, Name: x, File: "leaf/x.go"})
	}
	s.BuildGraph()
	return s
}

func manyCallers(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = fmt.Sprintf("pkg/c%d.Call", i)
	}
	return out
}

func TestExplain_NoGraph(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "a.B"})
	// No BuildGraph -> Graph() is nil.
	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("expected 0 insights when graph is nil, got %d", len(insights))
	}
}

func TestExplain_DetectsGodClass(t *testing.T) {
	// One hub with 12 dependents, plus low-fan-in noise symbols.
	store := makeStore("core.Hub", manyCallers(12), []string{"leaf.A", "leaf.B", "leaf.C"})

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 god-class insight, got %d: %+v", len(insights), insights)
	}
	in := insights[0]
	if !strings.Contains(in.Title, "core.Hub") {
		t.Errorf("title %q should name the hub", in.Title)
	}
	if in.Confidence < 0.5 || in.Confidence > 1.0 {
		t.Errorf("confidence %v out of range", in.Confidence)
	}
	// First evidence is the hub itself; capped dependents follow.
	if in.Evidence[0].Symbol != "core.Hub" {
		t.Errorf("first evidence should be the hub, got %q", in.Evidence[0].Symbol)
	}
	if len(in.Evidence) > maxEvidence+1 {
		t.Errorf("evidence not capped: got %d", len(in.Evidence))
	}
}

// TestExplain_DedupReopenedSymbol: a constant reopened across many files (Ruby
// STI/concerns, monkey-patched framework namespaces) yields one symbol fact per
// file, all sharing a Name. Fan-in is keyed by name, so each produced an identical
// insight — the RailsAdmin::Config::Actions ×50 flood. Report the name once.
func TestExplain_DedupReopenedSymbol(t *testing.T) {
	store := makeStore("core.Hub", manyCallers(12), nil)
	// Simulate the same constant reopened in 4 more files.
	for i := 0; i < 4; i++ {
		store.Add(facts.Fact{Kind: facts.KindSymbol, Name: "core.Hub", File: fmt.Sprintf("reopen/%d.go", i)})
	}
	store.BuildGraph()

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("reopened symbol should yield 1 insight, got %d: %+v", len(insights), insights)
	}
}

// TestExplain_RubyBaseClassExcluded: a Rails framework base class (.rb) with high
// fan-in-via-inheritance is not reported as a god class, while a same-fan-in domain
// class is. Guards the base-class exclusion.
func TestExplain_RubyBaseClassExcluded(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "ApplicationRecord", File: "app/models/application_record.rb"})
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "User", File: "app/models/user.rb"})
	for i := 0; i < 12; i++ {
		// Each caller subclasses ApplicationRecord and also references User.
		s.Add(facts.Fact{
			Kind: facts.KindSymbol, Name: fmt.Sprintf("app/models/m%d.Model", i),
			File:      fmt.Sprintf("app/models/m%d.rb", i),
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "ApplicationRecord"}, {Kind: facts.RelCalls, Target: "User"}},
		})
	}
	s.BuildGraph()

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	for _, in := range insights {
		if strings.Contains(in.Title, "ApplicationRecord") {
			t.Errorf("framework base class should be excluded from god-class: %q", in.Title)
		}
	}
	foundUser := false
	for _, in := range insights {
		if strings.Contains(in.Title, "User") {
			foundUser = true
		}
	}
	if !foundUser {
		t.Errorf("real domain hotspot User should still be reported; got %v", func() []string {
			out := make([]string, len(insights))
			for i, in := range insights {
				out[i] = in.Title
			}
			return out
		}())
	}
}

// TestExplain_CapsInsightCount: no more than maxInsights findings are emitted even
// when many distinct symbols exceed the outlier threshold.
func TestExplain_CapsInsightCount(t *testing.T) {
	s := facts.NewStore()
	// 40 distinct hubs, each with 12 dependents -> all well above the floor.
	for h := 0; h < 40; h++ {
		hub := fmt.Sprintf("core.Hub%d", h)
		s.Add(facts.Fact{Kind: facts.KindSymbol, Name: hub, File: fmt.Sprintf("core/hub%d.go", h)})
		for i := 0; i < 12; i++ {
			s.Add(facts.Fact{
				Kind: facts.KindSymbol, Name: fmt.Sprintf("caller%d_%d.Fn", h, i),
				File:      fmt.Sprintf("callers/%d_%d.go", h, i),
				Relations: []facts.Relation{{Kind: facts.RelCalls, Target: hub}},
			})
		}
	}
	s.BuildGraph()

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) > maxInsights {
		t.Errorf("insight count not capped: got %d, want <= %d", len(insights), maxInsights)
	}
}

func TestExplain_BelowFloor(t *testing.T) {
	// Hub has only 5 dependents — below minFanIn even if it's the max.
	store := makeStore("core.Hub", manyCallers(5), nil)

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("expected 0 insights below fan-in floor, got %d", len(insights))
	}
}

// TestExplain_FloorBoundary: a hub with exactly minFanIn (8) dependents amid
// low-fan-in noise is reported (it clears both the floor and the outlier test);
// one with 7 is below the floor and silent.
func TestExplain_FloorBoundary(t *testing.T) {
	at := makeStore("core.Hub", manyCallers(minFanIn), []string{"leaf.A", "leaf.B", "leaf.C"})
	got, err := New().Explain(context.Background(), at)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("fan-in == minFanIn (%d) should be reported, got %d insights", minFanIn, len(got))
	}

	below := makeStore("core.Hub", manyCallers(minFanIn-1), []string{"leaf.A", "leaf.B", "leaf.C"})
	got, err = New().Explain(context.Background(), below)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("fan-in == minFanIn-1 (%d) is below the floor, want 0 insights, got %d", minFanIn-1, len(got))
	}
}

// TestExplain_EvidenceCappedAndSorted: the hub is evidence[0]; its dependents
// follow, sorted and capped at maxEvidence.
func TestExplain_EvidenceCappedAndSorted(t *testing.T) {
	store := makeStore("core.Hub", manyCallers(15), nil)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d", len(insights))
	}
	ev := insights[0].Evidence
	if len(ev) != maxEvidence+1 {
		t.Fatalf("evidence should be hub + %d dependents, got %d", maxEvidence, len(ev))
	}
	deps := ev[1:]
	for i := 1; i < len(deps); i++ {
		if deps[i-1].Symbol > deps[i].Symbol {
			t.Errorf("dependents not sorted at %d: %q > %q", i, deps[i-1].Symbol, deps[i].Symbol)
		}
	}
}

// TestExplain_MultipleHubsOrderedByFanIn: the higher-fan-in hub ranks first.
func TestExplain_MultipleHubsOrderedByFanIn(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "core.Big", File: "core/big.go"})
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "core.Small", File: "core/small.go"})
	for i := 0; i < 20; i++ {
		s.Add(facts.Fact{Kind: facts.KindSymbol, Name: fmt.Sprintf("a/c%d.Fn", i), File: "a/c.go",
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "core.Big"}}})
	}
	for i := 0; i < 10; i++ {
		s.Add(facts.Fact{Kind: facts.KindSymbol, Name: fmt.Sprintf("b/c%d.Fn", i), File: "b/c.go",
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "core.Small"}}})
	}
	s.BuildGraph()

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) < 2 {
		t.Fatalf("expected both hubs reported, got %d", len(insights))
	}
	if !strings.Contains(insights[0].Title, "core.Big") {
		t.Errorf("higher fan-in hub should rank first, got %q", insights[0].Title)
	}
}

// TestExplain_ExcludesTestRefFanIn: a hub referenced by production symbols AND by
// test_ref/file_ref facts (spec files, initializers) must report a fan-in that
// counts only the architectural (symbol) dependents. Reference-only facts carry
// RelCalls edges into production code but are not symbols; counting them inflates
// the fan-in and drifts the outlier threshold (GAP-XL-15).
func TestExplain_ExcludesTestRefFanIn(t *testing.T) {
	s := facts.NewStore()
	const hub = "core.Hub"
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: hub, File: "core/hub.go"})
	// 8 real symbol dependents — clears the minFanIn floor on symbols alone.
	for i := 0; i < 8; i++ {
		s.Add(facts.Fact{
			Kind: facts.KindSymbol, Name: fmt.Sprintf("pkg/c%d.Call", i),
			File:      fmt.Sprintf("pkg/c%d.go", i),
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: hub}},
		})
	}
	// 5 reference-only dependents that also call the hub — must NOT count.
	for i := 0; i < 3; i++ {
		s.Add(facts.Fact{
			Kind: facts.KindTestRef, Name: fmt.Sprintf("spec/c%d_spec.rb", i),
			File:      fmt.Sprintf("spec/c%d_spec.rb", i),
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: hub}},
		})
	}
	for i := 0; i < 2; i++ {
		s.Add(facts.Fact{
			Kind: facts.KindFileRef, Name: fmt.Sprintf("config/init%d.rb", i),
			File:      fmt.Sprintf("config/init%d.rb", i),
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: hub}},
		})
	}
	// Low-fan-in noise so the outlier threshold is meaningful.
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "leaf.A", File: "leaf/a.go"})
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "leaf.B", File: "leaf/b.go"})
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "leaf.C", File: "leaf/c.go"})
	s.BuildGraph()

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	var hubInsight *facts.Insight
	for i := range insights {
		if strings.Contains(insights[i].Title, hub) {
			hubInsight = &insights[i]
			break
		}
	}
	if hubInsight == nil {
		t.Fatalf("hub %q not reported as a god-class; got %v", hub, insights)
	}
	// Fan-in must be 8 (symbols), not 13 (symbols + 3 test_ref + 2 file_ref).
	if !strings.Contains(hubInsight.Title, "(8 dependents)") {
		t.Errorf("fan-in should exclude reference-only facts; title = %q, want it to report 8 dependents", hubInsight.Title)
	}
	if strings.Contains(hubInsight.Title, "(13 dependents)") {
		t.Errorf("fan-in wrongly includes test_ref/file_ref facts; title = %q", hubInsight.Title)
	}
	// No spec file or initializer should appear as a dependent in the evidence.
	for _, ev := range hubInsight.Evidence[1:] {
		if strings.HasPrefix(ev.Symbol, "spec/") || strings.HasPrefix(ev.Symbol, "config/") {
			t.Errorf("reference-only fact %q leaked into god-class evidence", ev.Symbol)
		}
	}
}

// TestConfidenceMath locks the scaling and its clamps.
//
// The upper clamp is common.MaxHeuristicConfidence, NOT 1.0, and that is the point of
// the case at 40: a fan-in outlier is a statistical candidate however extreme it gets,
// and 1.0 is reserved for the one thing enola computes with certainty. Saturating here
// used to publish 14 of enola's own 25 god-class findings as structural facts.
func TestConfidenceMath(t *testing.T) {
	tests := []struct {
		value, threshold, want float64
	}{
		{10, 10, 0.5},  // at threshold
		{15, 10, 0.75}, // halfway to 2x
		{20, 10, common.MaxHeuristicConfidence},
		{40, 10, common.MaxHeuristicConfidence}, // however far past, still a heuristic
		{5, 10, 0.5},                            // below threshold clamps at 0.5
		{5, 0, 0.6},                             // degenerate threshold
	}
	for _, tt := range tests {
		if got := confidence(tt.value, tt.threshold); got != tt.want {
			t.Errorf("confidence(%v, %v) = %v, want %v", tt.value, tt.threshold, got, tt.want)
		}
	}
}

// TestExplain_TestSupportSymbolExcluded pins the fix for the case where a repo's
// tests are indexed as ordinary symbol facts — every Swift/Xcode repo, every
// Gradle repo with an androidTest source set, every Python repo with a tests/
// tree. An XCTest assertion helper is SUPPOSED to have a thousand callers, so
// ranking it as the repo's #1 god class is noise that outranks (and, given the
// maxInsights cap, evicts) real production findings.
//
// facts.ArchitecturalReverse only drops reference-only KINDS (test_ref/file_ref),
// which is how the Ruby/Go/TS case is handled — their test files are ignored by
// config and re-enter only as test_ref nodes. A Swift symbol under Tests/ is an
// ordinary symbol fact and passes straight through, so the path gate is needed too.
//
// The production hub must still be reported with its fan-in INTACT: the gate is on
// the candidate, not on the edges. Excluding test callers from the count instead
// was measured against the corpus and rewrites the production report — on
// python/superset it collapses the outlier threshold from 17.23 to 4.82.
func TestExplain_TestSupportSymbolExcluded(t *testing.T) {
	s := facts.NewStore()
	// A test helper with high fan-in from other test files.
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "Tests/Testability/Sources.Assert",
		File: "Tests/Testability/Sources/Assert.swift"})
	// A production hub with high fan-in, some of it from tests.
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "Sources/Core.APIService",
		File: "Sources/Core/APIService.swift"})
	for i := 0; i < 12; i++ {
		s.Add(facts.Fact{
			Kind: facts.KindSymbol,
			Name: fmt.Sprintf("Tests/CoreTests.SpecCase%d", i),
			File: fmt.Sprintf("Tests/CoreTests/SpecCase%d.swift", i),
			Relations: []facts.Relation{
				{Kind: facts.RelCalls, Target: "Tests/Testability/Sources.Assert"},
				{Kind: facts.RelCalls, Target: "Sources/Core.APIService"},
			},
		})
	}
	s.BuildGraph()

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	for _, in := range insights {
		if strings.Contains(in.Title, "Assert") {
			t.Errorf("test-support symbol reported as god class: %q", in.Title)
		}
	}
	var sawAPI bool
	for _, in := range insights {
		if strings.Contains(in.Title, "APIService") {
			sawAPI = true
			// Fan-in must be the FULL count (12), not the production-only count.
			if !strings.Contains(in.Title, "(12 dependents)") {
				t.Errorf("production hub fan-in was altered by the test gate: %q", in.Title)
			}
		}
	}
	if !sawAPI {
		t.Errorf("production hub should still be reported")
	}
}

// TestExplain_ProductionABTestNotExcluded is the fixed/28 guard. A production
// feature named like a test ("FeatureRolloutABTest.swift") must stay in the
// report — the test gate keys off the directory, never the filename.
func TestExplain_ProductionABTestNotExcluded(t *testing.T) {
	s := makeStore("Sources/Foundation.FeatureRolloutABTest", manyCallers(12), nil)
	// Re-file the hub onto its real production path.
	s2 := facts.NewStore()
	for _, f := range s.All() {
		if f.Name == "Sources/Foundation.FeatureRolloutABTest" {
			f.File = "Sources/ExampleFoundation/Components/FeatureRolloutABTest/FeatureRolloutABTest.swift"
		}
		s2.Add(f)
	}
	s2.BuildGraph()

	insights, err := New().Explain(context.Background(), s2)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	var saw bool
	for _, in := range insights {
		if strings.Contains(in.Title, "FeatureRolloutABTest") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("production A/B-test feature was wrongly gated out as test code")
	}
}

// A ubiquitous DATA struct constructed at many sites (RelInstantiates fan-in
// only) is not a god class — those edges must be excluded from fan-in.
func TestExplain_ExcludesInstantiateFanIn(t *testing.T) {
	s := facts.NewStore()
	const hub = "facts.Fact"
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: hub, File: "facts/fact.go",
		Props: map[string]any{"symbol_kind": facts.SymbolStruct}})
	// 12 sources that only CONSTRUCT the struct — must NOT count as fan-in.
	for i := 0; i < 12; i++ {
		s.Add(facts.Fact{
			Kind: facts.KindSymbol, Name: fmt.Sprintf("pkg/c%d.build", i),
			File:      fmt.Sprintf("pkg/c%d.go", i),
			Relations: []facts.Relation{{Kind: facts.RelInstantiates, Target: hub}},
		})
	}
	// Low-fan-in noise so the outlier threshold is meaningful.
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "leaf.A", File: "leaf/a.go"})
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "leaf.B", File: "leaf/b.go"})
	s.Add(facts.Fact{Kind: facts.KindSymbol, Name: "leaf.C", File: "leaf/c.go"})
	s.BuildGraph()

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	for _, ins := range insights {
		if strings.Contains(ins.Title, hub) {
			t.Errorf("data struct with only instantiate fan-in must not be a god-class; got %q", ins.Title)
		}
	}
}

// A generated symbol with outsized fan-in, an API client's request helper, is not
// reported; the hand-written hub is.
func TestExplain_GeneratedSymbolExcluded(t *testing.T) {
	syms := ringGraph(200, 4, 60)
	syms[0].Props = map[string]any{"generated": true}
	insights, err := New().Explain(context.Background(), storeInOrder(syms))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	for _, in := range insights {
		if strings.Contains(in.Title, "pkg/s0.Fn") {
			t.Errorf("a generated symbol was reported: %q", in.Title)
		}
	}
	plain, err := New().Explain(context.Background(), storeInOrder(ringGraph(200, 4, 60)))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	found := false
	for _, in := range plain {
		found = found || strings.Contains(in.Title, "pkg/s0.Fn")
	}
	if !found {
		t.Fatalf("fixture: the hub should be reported when it is not generated")
	}
}
