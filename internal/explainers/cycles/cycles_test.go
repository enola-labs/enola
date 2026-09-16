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

// TestExplain_OversizedClusterSoftened: an SCC larger than maxCycleModules is
// reported as a coupling cluster rather than a fixable cycle — the advice
// differs, because "introduce an interface" means nothing for a 99-node
// component. The *title* carries that distinction so pkg/explain does not count
// it as a cycle.
//
// Confidence deliberately does NOT carry it. See TestExplain_ClusterConfidenceIsMonotone.
func TestExplain_OversizedClusterSoftened(t *testing.T) {
	n := maxCycleModules + 3
	modules := make([]string, n)
	deps := map[string][]string{}
	for i := 0; i < n; i++ {
		modules[i] = fmt.Sprintf("app/m%02d", i)
	}
	// One big ring: m0 -> m1 -> ... -> m(n-1) -> m0, so all n form a single SCC.
	for i := 0; i < n; i++ {
		deps[modules[i]] = []string{modules[(i+1)%n]}
	}
	store := makeStore(modules, deps)

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

// TestExplain_TypeOnlyEdgesExcluded mirrors TestExplain_AssociationEdgesExcluded:
// two TS modules that reference only each other's types form a cycle in the type
// graph, but `import type` is erased by the compiler and never runs, so there is
// no load-order edge for the cycle explainer to report.
func TestExplain_TypeOnlyEdgesExcluded(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "src/a"})
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "src/b"})
	s.Add(facts.Fact{
		Kind: facts.KindDependency, File: "src/a/index.ts",
		Props:     map[string]any{facts.PropDependencyPhase: facts.DependencyPhaseTypeOnly},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "src/b"}},
	})
	s.Add(facts.Fact{
		Kind: facts.KindDependency, File: "src/b/index.ts",
		Props:     map[string]any{facts.PropDependencyPhase: facts.DependencyPhaseTypeOnly},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "src/a"}},
	})

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("type-only 2-cycle should not be reported, got %d: %+v", len(insights), insights)
	}
}

// TestExplain_RuntimeEdgeStillCyclesAlongsideTypeOnly guards against the phase
// exclusion swallowing a real cycle: a genuine runtime a<->b cycle is still
// reported even when a's import of b also carries a redundant type-only edge
// (e.g. the module imports both a value and, separately, one of its types).
func TestExplain_RuntimeEdgeStillCyclesAlongsideTypeOnly(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "src/a"})
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "src/b"})
	s.Add(facts.Fact{
		Kind: facts.KindDependency, File: "src/a/index.ts",
		Props:     map[string]any{facts.PropDependencyPhase: facts.DependencyPhaseRuntime},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "src/b"}},
	})
	s.Add(facts.Fact{
		Kind: facts.KindDependency, File: "src/a/types.ts",
		Props:     map[string]any{facts.PropDependencyPhase: facts.DependencyPhaseTypeOnly},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "src/b"}},
	})
	s.Add(facts.Fact{
		Kind: facts.KindDependency, File: "src/b/index.ts",
		Props:     map[string]any{facts.PropDependencyPhase: facts.DependencyPhaseRuntime},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "src/a"}},
	})

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 cycle insight, got %d: %+v", len(insights), insights)
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
