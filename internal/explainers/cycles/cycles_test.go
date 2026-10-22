package cycles

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// --- helpers ---

func makeStore(modules []string, deps map[string][]string) *facts.Store {
	s := facts.NewStore()
	for _, m := range modules {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	for src, targets := range deps {
		for _, tgt := range targets {
			s.Add(facts.Fact{
				Kind: facts.KindDependency,
				File: src + "/file.go",
				Relations: []facts.Relation{
					{Kind: facts.RelImports, Target: tgt},
				},
			})
		}
	}
	return s
}

// makeStoreLang is makeStore with every module tagged with the language its
// extractor would record. langOf overrides the language per module.
func makeStoreLang(lang string, langOf map[string]string, modules []string, deps map[string][]string) *facts.Store {
	s := makeStore(nil, deps)
	for _, m := range modules {
		l := lang
		if o, ok := langOf[m]; ok {
			l = o
		}
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m, Props: map[string]any{"language": l}})
	}
	return s
}

// ring returns n modules joined into one cycle m0 -> m1 -> ... -> m0.
func ring(prefix string, n int) ([]string, map[string][]string) {
	modules := make([]string, n)
	deps := map[string][]string{}
	for i := 0; i < n; i++ {
		modules[i] = fmt.Sprintf("%s/m%02d", prefix, i)
	}
	for i := 0; i < n; i++ {
		deps[modules[i]] = []string{modules[(i+1)%n]}
	}
	return modules, deps
}

// --- Tarjan's SCC tests ---

func TestTarjanSCC_KnownGraphs(t *testing.T) {
	tests := []struct {
		name           string
		graph          map[string][]string
		wantCycleCount int   // SCCs with size > 1
		wantCycleSizes []int // sorted sizes of non-trivial SCCs
	}{
		{
			name:           "empty graph",
			graph:          map[string][]string{},
			wantCycleCount: 0,
		},
		{
			name:           "single node no edges",
			graph:          map[string][]string{"A": nil},
			wantCycleCount: 0,
		},
		{
			name:           "simple cycle A<->B",
			graph:          map[string][]string{"A": {"B"}, "B": {"A"}},
			wantCycleCount: 1,
			wantCycleSizes: []int{2},
		},
		{
			name:           "triangle A->B->C->A",
			graph:          map[string][]string{"A": {"B"}, "B": {"C"}, "C": {"A"}},
			wantCycleCount: 1,
			wantCycleSizes: []int{3},
		},
		{
			name: "two disjoint cycles",
			graph: map[string][]string{
				"A": {"B"}, "B": {"A"},
				"C": {"D"}, "D": {"C"},
			},
			wantCycleCount: 2,
			wantCycleSizes: []int{2, 2},
		},
		{
			name:           "chain no cycle A->B->C",
			graph:          map[string][]string{"A": {"B"}, "B": {"C"}, "C": nil},
			wantCycleCount: 0,
		},
		{
			name: "complex graph: cycle with tail",
			graph: map[string][]string{
				"A": {"B"}, "B": {"C"}, "C": {"A", "D"}, "D": nil,
			},
			wantCycleCount: 1,
			wantCycleSizes: []int{3},
		},
		{
			name: "two cycles sharing a node",
			graph: map[string][]string{
				"A": {"B"}, "B": {"A", "C"}, "C": {"B"},
			},
			wantCycleCount: 1,
			wantCycleSizes: []int{3}, // A, B, C are all in one SCC
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sccs := tarjanSCC(tt.graph)
			var cycles [][]string
			for _, scc := range sccs {
				if len(scc) > 1 {
					cycles = append(cycles, scc)
				}
			}
			if len(cycles) != tt.wantCycleCount {
				t.Errorf("got %d cycles, want %d. SCCs: %v", len(cycles), tt.wantCycleCount, sccs)
				return
			}
			if tt.wantCycleSizes != nil {
				gotSizes := make([]int, len(cycles))
				for i, c := range cycles {
					gotSizes[i] = len(c)
				}
				sort.Ints(gotSizes)
				sort.Ints(tt.wantCycleSizes)
				if len(gotSizes) != len(tt.wantCycleSizes) {
					t.Errorf("cycle sizes: got %v, want %v", gotSizes, tt.wantCycleSizes)
				} else {
					for i := range gotSizes {
						if gotSizes[i] != tt.wantCycleSizes[i] {
							t.Errorf("cycle sizes[%d]: got %d, want %d", i, gotSizes[i], tt.wantCycleSizes[i])
						}
					}
				}
			}
		})
	}
}

func TestTarjanSCC_SelfLoop(t *testing.T) {
	graph := map[string][]string{"A": {"A"}}
	sccs := tarjanSCC(graph)
	// Self-loop creates an SCC of size 1 — should not panic
	for _, scc := range sccs {
		if len(scc) > 1 {
			t.Errorf("self-loop should not produce SCC > 1, got %v", scc)
		}
	}
}

// Tests for the module-graph and import-resolution helpers now live in the
// common package (common_test.go), where the shared implementations moved.

// --- Integration tests for Explain ---

func TestExplain_NoCycles(t *testing.T) {
	// Use paths with slashes so isExternalImport treats them as internal
	store := makeStore(
		[]string{"src/a", "src/b", "src/c"},
		map[string][]string{
			"src/a": {"src/b"},
			"src/b": {"src/c"},
		},
	)

	e := New()
	insights, err := e.Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("expected 0 insights for acyclic graph, got %d: %+v", len(insights), insights)
	}
}

func TestExplain_WithCycle(t *testing.T) {
	store := makeStore(
		[]string{"src/a", "src/b", "src/c"},
		map[string][]string{
			"src/a": {"src/b"},
			"src/b": {"src/c"},
			"src/c": {"src/a"},
		},
	)

	e := New()
	insights, err := e.Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 cycle insight, got %d", len(insights))
	}

	insight := insights[0]
	if insight.Confidence != 1.0 {
		t.Errorf("confidence = %f, want 1.0", insight.Confidence)
	}
	if len(insight.Evidence) != 3 {
		t.Errorf("evidence count = %d, want 3 (one per module in cycle)", len(insight.Evidence))
	}
	// Verify all three modules appear in evidence
	evidenceModules := make(map[string]bool)
	for _, ev := range insight.Evidence {
		evidenceModules[ev.Fact] = true
	}
	for _, mod := range []string{"src/a", "src/b", "src/c"} {
		if !evidenceModules[mod] {
			t.Errorf("module %q missing from cycle evidence", mod)
		}
	}
}

// TestExplain_OversizedClusterSoftened: an SCC of autoloaded modules larger than
// maxCycleModules is reported as a coupling cluster rather than a fixable cycle —
// the advice differs, because "introduce an interface" means nothing for a
// 99-node component. The *title* carries that distinction so pkg/explain does not
// count it as a cycle.
//
// Confidence deliberately does NOT carry it. See TestExplain_ClusterConfidenceIsMonotone.
func TestExplain_OversizedClusterSoftened(t *testing.T) {
	modules, deps := ring("app", maxCycleModules+3)
	store := makeStoreLang("ruby", nil, modules, deps)

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 cluster insight, got %d: %+v", len(insights), insights)
	}
	in := insights[0]
	if strings.HasPrefix(in.Title, "Cyclic dependency") {
		t.Errorf("oversized SCC should not be titled as a cyclic dependency: %q", in.Title)
	}
	if !strings.HasPrefix(in.Title, "Highly coupled module cluster") {
		t.Errorf("expected a coupling-cluster title, got %q", in.Title)
	}
	if in.Confidence != 1.0 {
		t.Errorf("a cluster is as certain as the cycle it grew from; got %v", in.Confidence)
	}
	if len(in.Evidence) > maxClusterMembers {
		t.Errorf("cluster evidence not capped: got %d, want <= %d", len(in.Evidence), maxClusterMembers)
	}
}

// TestExplain_AssociationEdgesExcluded: a two-model "cycle" formed solely by
// ActiveRecord associations (Order has_many LineItems, LineItem belongs_to Order)
// is bidirectional by nature, not a load-order cycle, and must not be reported.
func TestExplain_AssociationEdgesExcluded(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "app/models/order"})
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "app/models/line_item"})
	// Synthetic association edges both ways (as emitEdges would produce).
	s.Add(facts.Fact{
		Kind: facts.KindDependency, File: "app/models/order/_coupling.rb",
		Props:     map[string]any{facts.PropCouplingKind: facts.CouplingAssociation},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/models/line_item"}},
	})
	s.Add(facts.Fact{
		Kind: facts.KindDependency, File: "app/models/line_item/_coupling.rb",
		Props:     map[string]any{facts.PropCouplingKind: facts.CouplingAssociation},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/models/order"}},
	})

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("association-only 2-cycle should not be reported, got %d: %+v", len(insights), insights)
	}
}

// TestExplain_Deterministic guards BUG-2: the cycle path, evidence order, and
// multi-cycle insight order used to depend on Go's randomized map iteration
// (tarjanSCC ranged the graph map directly and never sorted). Each Explain call
// re-ranges the maps, so 50 runs exercise many iteration orders; the fully
// rendered output (title + description + evidence facts) must be byte-identical
// every time — enola's core determinism promise for insights.json.
func TestExplain_Deterministic(t *testing.T) {
	store := makeStore(
		[]string{"src/a", "src/b", "src/c", "src/d", "src/e", "src/f"},
		map[string][]string{
			"src/a": {"src/b"},
			"src/b": {"src/c"},
			"src/c": {"src/a"}, // cycle 1: a,b,c
			"src/d": {"src/e"},
			"src/e": {"src/f"},
			"src/f": {"src/d"}, // cycle 2: d,e,f
		},
	)

	render := func() string {
		insights, err := New().Explain(context.Background(), store)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		var b strings.Builder
		for _, in := range insights {
			b.WriteString(in.Title)
			b.WriteByte('\n')
			b.WriteString(in.Description)
			b.WriteByte('\n')
			for _, ev := range in.Evidence {
				b.WriteString(ev.Fact)
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		return b.String()
	}

	want := render()
	for i := 0; i < 50; i++ {
		if got := render(); got != want {
			t.Fatalf("non-deterministic output on iteration %d:\nwant:\n%s\ngot:\n%s", i, want, got)
		}
	}
}

// TestExplain_EvidenceCanonicalOrder locks that a cycle's evidence lists its
// members in sorted order (the canonicalization behind the determinism fix).
func TestExplain_EvidenceCanonicalOrder(t *testing.T) {
	store := makeStore(
		[]string{"src/c", "src/a", "src/b"},
		map[string][]string{
			"src/a": {"src/b"},
			"src/b": {"src/c"},
			"src/c": {"src/a"},
		},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d", len(insights))
	}
	var got []string
	for _, ev := range insights[0].Evidence {
		got = append(got, ev.Fact)
	}
	want := []string{"src/a", "src/b", "src/c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("evidence order = %v, want sorted %v", got, want)
	}
}

// TestExplain_SelfLoopNoInsight runs a self-importing module through the full
// Explain (not just tarjanSCC): a size-1 SCC is not a cycle, so no insight and
// no panic.
func TestExplain_SelfLoopNoInsight(t *testing.T) {
	store := makeStore(
		[]string{"src/a"},
		map[string][]string{"src/a": {"src/a"}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("self-loop should produce no cycle insight, got %d", len(insights))
	}
}

// TestExplain_SharedNodeSingleInsight: two cycles sharing a node collapse into
// one SCC, so Explain emits a single insight covering all three modules.
func TestExplain_SharedNodeSingleInsight(t *testing.T) {
	store := makeStore(
		[]string{"src/a", "src/b", "src/c"},
		map[string][]string{
			"src/a": {"src/b"},
			"src/b": {"src/a", "src/c"},
			"src/c": {"src/b"},
		},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight for two cycles sharing a node, got %d", len(insights))
	}
	if len(insights[0].Evidence) != 3 {
		t.Errorf("expected all 3 modules as evidence, got %d", len(insights[0].Evidence))
	}
}

func TestExplain_EmptyStore(t *testing.T) {
	insights, err := New().Explain(context.Background(), facts.NewStore())
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("empty store should yield no insights, got %d", len(insights))
	}
}

func TestExplain_MultipleCycles(t *testing.T) {
	store := makeStore(
		[]string{"src/a", "src/b", "src/c", "src/d"},
		map[string][]string{
			"src/a": {"src/b"},
			"src/b": {"src/a"},
			"src/c": {"src/d"},
			"src/d": {"src/c"},
		},
	)

	e := New()
	insights, err := e.Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 2 {
		t.Errorf("expected 2 cycle insights for 2 disjoint cycles, got %d", len(insights))
	}
}

// TestIntraCompilationUnitCycle_IsNotABuildDefect pins the distinction that C# and
// Rust need. MSBuild rejects a circular ProjectReference and Cargo a circular crate
// dependency, so any cycle enola finds in those languages is necessarily WITHIN one
// unit — where mutual references between namespaces or submodules are legal and
// ordinary. Reporting it as something that "can cause initialization issues" is
// simply untrue, and jellyfin had six such findings at confidence 1.0.
func TestIntraCompilationUnitCycle_IsNotABuildDefect(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"Emby.Naming/Audio", "Emby.Naming/Video"} {
		s.Add(facts.Fact{
			Kind:  facts.KindModule,
			Name:  m,
			Props: map[string]any{"language": "csharp", "project": "Emby.Naming"},
		})
	}
	s.Add(facts.Fact{
		Kind: facts.KindDependency, File: "Emby.Naming/Audio/A.cs",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "Emby.Naming/Video"}},
	})
	s.Add(facts.Fact{
		Kind: facts.KindDependency, File: "Emby.Naming/Video/V.cs",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "Emby.Naming/Audio"}},
	})

	ins, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 {
		t.Fatalf("expected one cycle insight, got %d", len(ins))
	}
	got := ins[0]
	// The cycle is still reported, and still at confidence 1.0: it is a structural
	// fact, and lowering it would move the finding under min_confidence filters and
	// the check gate.
	if !strings.HasPrefix(got.Title, "Cyclic dependency detected") {
		t.Errorf("title = %q, want the stable cycle prefix", got.Title)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0", got.Confidence)
	}
	// What changes is the interpretation.
	if strings.Contains(got.Description, "can cause initialization issues") {
		t.Errorf("intra-unit cycle still claims an initialization hazard: %q", got.Description)
	}
	if !strings.Contains(got.Description, "Emby.Naming") ||
		!strings.Contains(got.Description, "NOT a build-order problem") {
		t.Errorf("description should name the shared unit and correct the claim: %q", got.Description)
	}
}

// TestCrossCompilationUnitCycle_KeepsBuildDefectWording is the control: two modules
// in DIFFERENT crates really are separately built, so the original wording stands.
func TestCrossCompilationUnitCycle_KeepsBuildDefectWording(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "crates/a/src",
		Props: map[string]any{"language": "rust", "crate": "a"}})
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "crates/b/src",
		Props: map[string]any{"language": "rust", "crate": "b"}})
	s.Add(facts.Fact{Kind: facts.KindDependency, File: "crates/a/src/l.rs",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "crates/b/src"}}})
	s.Add(facts.Fact{Kind: facts.KindDependency, File: "crates/b/src/l.rs",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "crates/a/src"}}})

	ins, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 {
		t.Fatalf("expected one cycle insight, got %d", len(ins))
	}
	if !strings.Contains(ins[0].Description, "can cause initialization issues") {
		t.Errorf("a cross-unit cycle must keep the build-defect wording: %q", ins[0].Description)
	}
}

// TestUnknownCompilationUnit_KeepsBuildDefectWording covers the language that
// models no unit at all (Go): the softened wording must not fire, because a cycle
// there really does span separately compiled packages.
func TestUnknownCompilationUnit_KeepsBuildDefectWording(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"internal/a", "internal/b"} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m, Props: map[string]any{"language": "go"}})
	}
	s.Add(facts.Fact{Kind: facts.KindDependency, File: "internal/a/x.go",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "internal/b"}}})
	s.Add(facts.Fact{Kind: facts.KindDependency, File: "internal/b/y.go",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "internal/a"}}})

	ins, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || !strings.Contains(ins[0].Description, "can cause initialization issues") {
		t.Errorf("a language with no compilation-unit prop must keep the original wording: %+v", ins)
	}
}

// A larger cycle must never be reported as less severe than a smaller one.
//
// This is the defect a colleague found on a real Rails app: adding an edge that
// grew an 8-module cycle to 10 resolved a confidence-1.0 finding and replaced it
// with a 0.4 advisory, so a strictly worse graph reported as an improvement and
// passed a `--min-confidence=0.8` gate it had previously failed. Confidence
// means "is this real"; encoding "is this easy to fix" in it made the gate blind
// exactly where the problem was worst.
func TestExplain_ClusterConfidenceIsMonotone(t *testing.T) {
	ring := func(n int) facts.Insight {
		t.Helper()
		modules := make([]string, n)
		deps := map[string][]string{}
		for i := 0; i < n; i++ {
			modules[i] = fmt.Sprintf("app/m%02d", i)
		}
		for i := 0; i < n; i++ {
			deps[modules[i]] = []string{modules[(i+1)%n]}
		}
		insights, err := New().Explain(context.Background(), makeStore(modules, deps))
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if len(insights) != 1 {
			t.Fatalf("expected exactly one insight for a %d-module ring, got %d", n, len(insights))
		}
		return insights[0]
	}

	small := ring(maxCycleModules)
	grown := ring(maxCycleModules + 2)

	if grown.Confidence < small.Confidence {
		t.Fatalf("growing a cycle from %d to %d modules lowered confidence %v -> %v; "+
			"a worse graph must never report as an improvement",
			maxCycleModules, maxCycleModules+2, small.Confidence, grown.Confidence)
	}
}

// assertArrowsAreEdges fails when a printed `a -> b` is not an edge of deps.
func assertArrowsAreEdges(t *testing.T, path string, deps map[string][]string) {
	t.Helper()
	hops := strings.Split(path, " -> ")
	if len(hops) < 3 || hops[0] != hops[len(hops)-1] {
		t.Fatalf("%q is not a closed cycle", path)
	}
	for i := 0; i+1 < len(hops); i++ {
		found := false
		for _, to := range deps[hops[i]] {
			found = found || to == hops[i+1]
		}
		if !found {
			t.Errorf("%q prints %s -> %s, and no such import exists", path, hops[i], hops[i+1])
		}
	}
}

// TestExplain_CyclePathFollowsEdges: five modules joined by two loops are one
// component, and their names in order are not a path through it.
func TestExplain_CyclePathFollowsEdges(t *testing.T) {
	deps := map[string][]string{
		"app/Http/Controllers": {"app/Services"},
		"app/Services":         {"app/Repositories", "app/Gateways"},
		"app/Repositories":     {"app/Http/Controllers"},
		"app/Gateways":         {"app/Models"},
		"app/Models":           {"app/Services"},
	}
	modules := []string{"app/Gateways", "app/Http/Controllers", "app/Models", "app/Repositories", "app/Services"}
	insights, err := New().Explain(context.Background(), makeStore(modules, deps))
	if err != nil || len(insights) != 1 {
		t.Fatalf("insights = %v, err = %v; want the one component", insights, err)
	}
	got := insights[0]
	if got.Title != "Cyclic dependency detected (5 modules)" || len(got.Evidence) != 5 {
		t.Errorf("title = %q with %d evidence rows; membership and title must not change", got.Title, len(got.Evidence))
	}
	const marker = "One of them: "
	at := strings.Index(got.Description, marker)
	if at < 0 {
		t.Fatalf("description names no cycle: %s", got.Description)
	}
	path := got.Description[at+len(marker):]
	path = path[:strings.Index(path, ". ")]
	assertArrowsAreEdges(t, path, deps)
	if path != "app/Gateways -> app/Models -> app/Services -> app/Gateways" {
		t.Errorf("cycle = %q, want the shortest one through the first member", path)
	}
	for _, m := range modules {
		if !strings.Contains(got.Description[:at], m) {
			t.Errorf("description does not list member %s: %s", m, got.Description)
		}
	}
}

// TestExplain_SingleCycleIsPrintedInImportOrder: c imports b imports a imports c, so
// the names in order run against every edge.
func TestExplain_SingleCycleIsPrintedInImportOrder(t *testing.T) {
	deps := map[string][]string{"x/a": {"x/c"}, "x/c": {"x/b"}, "x/b": {"x/a"}}
	insights, err := New().Explain(context.Background(), makeStore([]string{"x/a", "x/b", "x/c"}, deps))
	if err != nil || len(insights) != 1 {
		t.Fatalf("insights = %v, err = %v", insights, err)
	}
	const want = "The following modules form a dependency cycle: x/a -> x/c -> x/b -> x/a."
	if !strings.HasPrefix(insights[0].Description, want) {
		t.Errorf("description = %q, want it to start %q", insights[0].Description, want)
	}
}

// TestExplain_EveryPrintedArrowIsAnEdge runs random graphs through the explainer and
// checks each cycle it prints against the edges it was given.
func TestExplain_EveryPrintedArrowIsAnEdge(t *testing.T) {
	seed := uint32(7)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>16) % n
	}
	for round := 0; round < 200; round++ {
		size := 3 + next(8)
		modules := make([]string, size)
		for i := range modules {
			modules[i] = fmt.Sprintf("p/m%02d", i)
		}
		deps := map[string][]string{}
		for e := 0; e < size+next(size*2); e++ {
			from, to := modules[next(size)], modules[next(size)]
			if from != to {
				deps[from] = append(deps[from], to)
			}
		}
		insights, err := New().Explain(context.Background(), makeStore(modules, deps))
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range insights {
			if !strings.HasPrefix(in.Title, "Cyclic dependency") {
				continue
			}
			path := in.Description
			for _, marker := range []string{"One of them: ", "form a dependency cycle: "} {
				if at := strings.Index(path, marker); at >= 0 {
					path = path[at+len(marker):]
				}
			}
			assertArrowsAreEdges(t, path[:strings.Index(path, ". ")], deps)
		}
	}
}

// TestExplain_LargeCycleOutsideAutoloadIsACycle: a TypeScript or Kotlin import is a
// load instruction, so a component of a dozen folders in either is a real cycle. It
// keeps the cycle title, so pkg/explain counts it, names one walkable cycle, and caps
// its evidence; it must not borrow the autoload note.
func TestExplain_LargeCycleOutsideAutoloadIsACycle(t *testing.T) {
	n := maxClusterMembers + 3
	mixed, _ := ring("src", n)
	cases := []struct {
		name   string
		lang   string
		langOf map[string]string
	}{
		{"typescript", "typescript", nil},
		{"kotlin", "kotlin", nil},
		{"ruby with one typescript module", "ruby", map[string]string{mixed[0]: "typescript"}},
		{"unknown language", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			modules, deps := ring("src", n)
			insights, err := New().Explain(context.Background(), makeStoreLang(tc.lang, tc.langOf, modules, deps))
			if err != nil {
				t.Fatal(err)
			}
			if len(insights) != 1 {
				t.Fatalf("want one insight, got %d: %+v", len(insights), insights)
			}
			in := insights[0]
			if want := fmt.Sprintf("Cyclic dependency detected (%d modules)", n); in.Title != want {
				t.Errorf("title = %q, want %q", in.Title, want)
			}
			if !strings.Contains(in.Description, "real cycle") || strings.Contains(in.Description, "resolve lazily") {
				t.Errorf("description should say this is a real cycle, not autoload coupling: %q", in.Description)
			}
			if len(in.Evidence) != maxClusterMembers {
				t.Errorf("evidence = %d entries, want capped at %d", len(in.Evidence), maxClusterMembers)
			}
			at := strings.Index(in.Description, "One of them: ")
			if at < 0 {
				t.Fatalf("no example cycle in %q", in.Description)
			}
			path := in.Description[at+len("One of them: "):]
			assertArrowsAreEdges(t, path[:strings.Index(path, ". ")], deps)
			if in.Confidence != 1.0 {
				t.Errorf("confidence = %v, want 1.0", in.Confidence)
			}
		})
	}
}
