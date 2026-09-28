package facts

import (
	"reflect"
	"strings"
	"testing"
)

// buildTestGraph creates a graph from a set of facts for testing.
// The topology is:
//
//	A --calls--> B --calls--> C --calls--> D
//	A --imports-> E
//	E --calls--> C
//	F (disconnected)
func buildTestGraph() (*Graph, *Store) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "A", File: "a.go", Line: 1, Relations: []Relation{
			{Kind: RelCalls, Target: "B"},
			{Kind: RelImports, Target: "E"},
		}},
		Fact{Kind: KindSymbol, Name: "B", File: "b.go", Line: 10, Relations: []Relation{
			{Kind: RelCalls, Target: "C"},
		}},
		Fact{Kind: KindModule, Name: "C", File: "c.go", Line: 20, Relations: []Relation{
			{Kind: RelCalls, Target: "D"},
		}},
		Fact{Kind: KindSymbol, Name: "D", File: "d.go", Line: 30},
		Fact{Kind: KindModule, Name: "E", File: "e.go", Line: 40, Relations: []Relation{
			{Kind: RelCalls, Target: "C"},
		}},
		Fact{Kind: KindSymbol, Name: "F", File: "f.go", Line: 50}, // disconnected
	)
	s.BuildGraph()
	return s.Graph(), s
}

// buildCyclicGraph creates a graph with a cycle: A -> B -> C -> A
func buildCyclicGraph() (*Graph, *Store) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindModule, Name: "A", File: "a.go", Relations: []Relation{
			{Kind: RelImports, Target: "B"},
		}},
		Fact{Kind: KindModule, Name: "B", File: "b.go", Relations: []Relation{
			{Kind: RelImports, Target: "C"},
		}},
		Fact{Kind: KindModule, Name: "C", File: "c.go", Relations: []Relation{
			{Kind: RelImports, Target: "A"},
		}},
	)
	s.BuildGraph()
	return s.Graph(), s
}

func TestNewGraph_BuildsAdjacencyLists(t *testing.T) {
	g, _ := buildTestGraph()

	if g.NodeCount() != 6 {
		t.Errorf("NodeCount = %d, want 6", g.NodeCount())
	}
	if g.EdgeCount() != 5 {
		t.Errorf("EdgeCount = %d, want 5", g.EdgeCount())
	}

	// Check forward adjacency for A
	aEdges := g.ForwardEdges("A")
	if len(aEdges) != 2 {
		t.Errorf("A forward edges = %d, want 2", len(aEdges))
	}

	// Check reverse adjacency for C (B and E both call C)
	cEdges := g.ReverseEdges("C")
	if len(cEdges) != 2 {
		t.Errorf("C reverse edges = %d, want 2", len(cEdges))
	}
}

func TestTraverse_ForwardFromA(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("A", "forward", nil, nil, 10, 100)

	// A -> B -> C -> D, A -> E -> C (C already visited)
	// Should visit: A, B, C, D, E
	if result.Stats.NodesVisited != 5 {
		t.Errorf("NodesVisited = %d, want 5", result.Stats.NodesVisited)
	}

	// Nodes in result (including start)
	names := nodeNames(result.Nodes)
	for _, want := range []string{"A", "B", "C", "D", "E"} {
		if !contains(names, want) {
			t.Errorf("missing node %q in traverse result", want)
		}
	}
	if contains(names, "F") {
		t.Error("F should not be reachable from A")
	}
}

func TestTraverse_ReverseFromD(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("D", "reverse", nil, nil, 10, 100)

	// D is called by C, C is called by B and E, B is called by A, E is imported by A
	// Reverse from D: D <- C <- B <- A, C <- E <- A (A already visited)
	names := nodeNames(result.Nodes)
	for _, want := range []string{"D", "C", "B", "A", "E"} {
		if !contains(names, want) {
			t.Errorf("missing node %q in reverse traverse from D", want)
		}
	}
}

func TestTraverse_DepthLimit(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("A", "forward", nil, nil, 1, 100)

	// Depth 1: A -> B, A -> E (only direct neighbors)
	names := nodeNames(result.Nodes)
	if !contains(names, "A") || !contains(names, "B") || !contains(names, "E") {
		t.Errorf("depth-1 should include A, B, E; got %v", names)
	}
	if contains(names, "C") || contains(names, "D") {
		t.Errorf("depth-1 should NOT include C or D; got %v", names)
	}
}

func TestTraverse_MaxNodesLimit(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("A", "forward", nil, nil, 10, 3)

	// Should only return at most 3 nodes
	if len(result.Nodes) > 3 {
		t.Errorf("maxNodes=3 but got %d nodes", len(result.Nodes))
	}
	if !result.Stats.Truncated {
		t.Error("should be truncated with maxNodes=3")
	}
}

func TestTraverse_EdgesConsistentWhenTruncated(t *testing.T) {
	g, _ := buildTestGraph()

	// maxNodes=2 truncates the result; every returned edge must still reference
	// only nodes present in result.Nodes (no dangling edges to capped-out nodes).
	result := g.Traverse("A", "forward", nil, nil, 10, 2)
	if !result.Stats.Truncated {
		t.Fatal("expected truncation with maxNodes=2")
	}
	inSet := map[string]bool{}
	for _, n := range result.Nodes {
		inSet[n.Name] = true
	}
	for _, e := range result.Edges {
		if !inSet[e.Source] || !inSet[e.Target] {
			t.Errorf("edge %s -> %s references a node absent from result.Nodes %v", e.Source, e.Target, nodeNames(result.Nodes))
		}
	}
}

// buildChainGraph creates a linear chain N00 -> N01 -> ... -> N20 (21 nodes,
// 20 hops), which is exactly the traverseFrom maxDepth ceiling. A chain isolates
// the BFS frontier: every node is reachable only through its predecessor, so any
// node dropped from the queue silently hides the whole remaining tail.
func buildChainGraph(n int) *Graph {
	s := NewStore()
	for i := 0; i < n; i++ {
		f := Fact{Kind: KindSymbol, Name: chainName(i), File: "chain.go", Line: i + 1}
		if i < n-1 {
			f.Relations = []Relation{{Kind: RelCalls, Target: chainName(i + 1)}}
		}
		s.Add(f)
	}
	s.BuildGraph()
	return s.Graph()
}

func chainName(i int) string {
	return "N" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// TestTraverse_MaxNodesCapsOutputNotFrontier pins that maxNodes bounds what is
// RETURNED, not how far the BFS walks. The node-kind filter path already queues
// nodes it excludes from results ("Still traverse through this node but don't
// include it in results"); the maxNodes path must do the same, or every node
// reachable only through a capped-out node becomes invisible and the reported
// stats describe a truncated walk rather than the graph.
func TestTraverse_MaxNodesCapsOutputNotFrontier(t *testing.T) {
	g := buildChainGraph(21)

	result := g.Traverse("N00", "forward", nil, nil, 20, 2)

	if !result.Stats.Truncated {
		t.Fatal("expected Truncated with maxNodes=2")
	}
	if len(result.Nodes) != 2 {
		t.Errorf("len(Nodes) = %d, want 2 (maxNodes caps the returned set)", len(result.Nodes))
	}
	// The frontier must survive the cap: the whole 21-node chain is walked.
	if result.Stats.NodesVisited != 21 {
		t.Errorf("NodesVisited = %d, want 21 — the BFS frontier stopped at the cap instead of walking the chain", result.Stats.NodesVisited)
	}
	if result.Stats.MaxDepthReached != 20 {
		t.Errorf("MaxDepthReached = %d, want 20 — depth is reported from a truncated walk, understating the graph", result.Stats.MaxDepthReached)
	}
}

// TestTraverse_MaxNodesReturnsSamePrefixAsUncapped pins the no-collateral-damage
// property: raising the frontier past the cap must not change WHICH nodes are
// returned. The capped result must stay the exact BFS-order prefix of the
// uncapped one, so the cap remains a pure output bound.
func TestTraverse_MaxNodesReturnsSamePrefixAsUncapped(t *testing.T) {
	g := buildChainGraph(21)

	full := g.Traverse("N00", "forward", nil, nil, 20, 500)
	if len(full.Nodes) != 21 {
		t.Fatalf("uncapped traversal returned %d nodes, want 21", len(full.Nodes))
	}

	for _, cap := range []int{1, 2, 5, 13} {
		capped := g.Traverse("N00", "forward", nil, nil, 20, cap)
		if len(capped.Nodes) != cap {
			t.Errorf("maxNodes=%d returned %d nodes, want %d", cap, len(capped.Nodes), cap)
			continue
		}
		want := nodeNames(full.Nodes[:cap])
		if got := nodeNames(capped.Nodes); !reflect.DeepEqual(got, want) {
			t.Errorf("maxNodes=%d returned %v, want the uncapped prefix %v", cap, got, want)
		}
	}
}

// TestTraverse_MaxNodesPrefixWithBranching is the branching counterpart of
// TestTraverse_MaxNodesReturnsSamePrefixAsUncapped. A chain cannot catch
// sibling-ordering effects — every node has one successor — so it would pass
// even if the cap changed WHICH of several equal-depth siblings is returned.
// This topology gives depth 1 three siblings and depth 2 five, so any reordering
// introduced by queueing capped-out nodes shows up as a prefix mismatch.
func TestTraverse_MaxNodesPrefixWithBranching(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "R", File: "r.go", Relations: []Relation{
			{Kind: RelCalls, Target: "A"},
			{Kind: RelCalls, Target: "B"},
			{Kind: RelCalls, Target: "C"},
		}},
		Fact{Kind: KindSymbol, Name: "A", File: "a.go", Relations: []Relation{
			{Kind: RelCalls, Target: "A1"},
			{Kind: RelCalls, Target: "A2"},
		}},
		Fact{Kind: KindSymbol, Name: "B", File: "b.go", Relations: []Relation{
			{Kind: RelCalls, Target: "B1"},
		}},
		Fact{Kind: KindSymbol, Name: "C", File: "c.go", Relations: []Relation{
			{Kind: RelCalls, Target: "C1"},
			{Kind: RelCalls, Target: "C2"},
		}},
		Fact{Kind: KindSymbol, Name: "A1", File: "a1.go"},
		Fact{Kind: KindSymbol, Name: "A2", File: "a2.go"},
		Fact{Kind: KindSymbol, Name: "B1", File: "b1.go"},
		Fact{Kind: KindSymbol, Name: "C1", File: "c1.go"},
		Fact{Kind: KindSymbol, Name: "C2", File: "c2.go"},
	)
	s.BuildGraph()
	g := s.Graph()

	full := g.Traverse("R", "forward", nil, nil, 5, 500)
	if len(full.Nodes) != 9 {
		t.Fatalf("uncapped traversal returned %d nodes, want 9", len(full.Nodes))
	}

	for cap := 1; cap <= 9; cap++ {
		capped := g.Traverse("R", "forward", nil, nil, 5, cap)
		if got, want := nodeNames(capped.Nodes), nodeNames(full.Nodes[:cap]); !reflect.DeepEqual(got, want) {
			t.Errorf("maxNodes=%d returned %v, want the uncapped prefix %v", cap, got, want)
		}
		// The walk itself must be cap-independent: every cap sees the whole graph.
		if capped.Stats.NodesVisited != 9 {
			t.Errorf("maxNodes=%d: NodesVisited = %d, want 9 (the frontier must survive the cap)", cap, capped.Stats.NodesVisited)
		}
		if capped.Stats.MaxDepthReached != full.Stats.MaxDepthReached {
			t.Errorf("maxNodes=%d: MaxDepthReached = %d, want %d", cap, capped.Stats.MaxDepthReached, full.Stats.MaxDepthReached)
		}
	}
}

func TestTraverse_RelationKindFilter(t *testing.T) {
	g, _ := buildTestGraph()

	// Only follow "calls" relations from A
	result := g.Traverse("A", "forward", []string{RelCalls}, nil, 10, 100)

	names := nodeNames(result.Nodes)
	// A --calls-> B --calls-> C --calls-> D (imports to E skipped)
	for _, want := range []string{"A", "B", "C", "D"} {
		if !contains(names, want) {
			t.Errorf("calls-only traverse missing %q", want)
		}
	}
	if contains(names, "E") {
		t.Error("E should not be reachable via calls-only")
	}
}

func TestTraverse_NodeKindFilter(t *testing.T) {
	g, _ := buildTestGraph()

	// Traverse from A but only include module-kind nodes in results
	// C and E are modules, A/B/D are symbols
	result := g.Traverse("A", "forward", nil, []string{KindModule}, 10, 100)

	names := nodeNames(result.Nodes)
	// Should traverse through symbols but only include modules in result
	// A(sym) -> B(sym) -> C(mod) -> D(sym), A -> E(mod) -> C
	if !contains(names, "C") || !contains(names, "E") {
		t.Errorf("module filter should include C and E; got %v", names)
	}
	// Start node A is always included regardless of filter
	if !contains(names, "A") {
		t.Errorf("start node A should always be included; got %v", names)
	}
}

func TestTraverse_CycleHandling(t *testing.T) {
	g, _ := buildCyclicGraph()

	result := g.Traverse("A", "forward", nil, nil, 20, 100)

	// Should visit A, B, C without infinite loop
	if result.Stats.NodesVisited != 3 {
		t.Errorf("NodesVisited = %d, want 3 (cycle should be handled)", result.Stats.NodesVisited)
	}
	names := nodeNames(result.Nodes)
	for _, want := range []string{"A", "B", "C"} {
		if !contains(names, want) {
			t.Errorf("cycle traverse missing %q", want)
		}
	}
}

func TestTraverse_DisconnectedNode(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("F", "forward", nil, nil, 10, 100)

	// F is disconnected, should only return F itself
	if len(result.Nodes) != 1 || result.Nodes[0].Name != "F" {
		t.Errorf("disconnected traverse: got %v, want [F]", nodeNames(result.Nodes))
	}
}

func TestTraverse_NonexistentStart(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("NONEXISTENT", "forward", nil, nil, 10, 100)

	// Should still return the start node (with no metadata)
	if len(result.Nodes) != 1 || result.Nodes[0].Name != "NONEXISTENT" {
		t.Errorf("nonexistent start: got %v, want [NONEXISTENT]", nodeNames(result.Nodes))
	}
}

func TestFindPath_DirectConnection(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.FindPath("A", "B", nil, 10)

	if !result.Found {
		t.Fatal("path A->B should be found")
	}
	if len(result.Path) != 2 {
		t.Errorf("path length = %d, want 2 (A, B)", len(result.Path))
	}
	if result.Path[0].Name != "A" || result.Path[1].Name != "B" {
		t.Errorf("path = %v, want [A, B]", pathNames(result.Path))
	}
}

func TestFindPath_MultiHop(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.FindPath("A", "D", nil, 10)

	if !result.Found {
		t.Fatal("path A->D should be found")
	}
	// Shortest: A -> B -> C -> D
	if len(result.Path) != 4 {
		t.Errorf("path length = %d, want 4 (A, B, C, D); path = %v", len(result.Path), pathNames(result.Path))
	}

	// Check edges
	if len(result.Edges) != 3 {
		t.Errorf("edges = %d, want 3", len(result.Edges))
	}
}

func TestFindPath_NoPath(t *testing.T) {
	g, _ := buildTestGraph()

	// F is disconnected
	result := g.FindPath("A", "F", nil, 10)

	if result.Found {
		t.Error("path A->F should not exist")
	}
	if len(result.Path) != 0 {
		t.Errorf("path should be empty, got %v", pathNames(result.Path))
	}
}

func TestFindPath_SameNode(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.FindPath("A", "A", nil, 10)

	if !result.Found {
		t.Fatal("path A->A should be found (trivial)")
	}
	if len(result.Path) != 1 {
		t.Errorf("path length = %d, want 1 (just A)", len(result.Path))
	}
}

func TestFindPath_DepthLimit(t *testing.T) {
	g, _ := buildTestGraph()

	// A -> D needs 3 hops, limit to 2
	result := g.FindPath("A", "D", nil, 2)

	if result.Found {
		t.Error("path A->D should not be found with maxDepth=2")
	}
}

func TestFindPath_RelationKindFilter(t *testing.T) {
	g, _ := buildTestGraph()

	// Only imports: A --imports-> E, but no path from E to D via imports
	result := g.FindPath("A", "D", []string{RelImports}, 10)

	if result.Found {
		t.Error("path A->D via imports only should not exist")
	}
}

func TestFindPath_WithCycle(t *testing.T) {
	g, _ := buildCyclicGraph()

	result := g.FindPath("A", "C", nil, 10)

	if !result.Found {
		t.Fatal("path A->C should be found")
	}
	// Shortest: A -> B -> C
	if len(result.Path) != 3 {
		t.Errorf("path length = %d, want 3; path = %v", len(result.Path), pathNames(result.Path))
	}
}

func TestImpactSet_Basic(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.ImpactSet("C", 10, 100, false)

	if result.Target != "C" {
		t.Errorf("Target = %q, want C", result.Target)
	}

	// Who depends on C? B calls C, E calls C, A calls B and imports E
	// Reverse: C <- B <- A, C <- E <- A (A already counted)
	// Depth 1: B, E
	// Depth 2: A (via B), A (via E, already counted)
	depth1 := result.ByDepth[1]
	depth1Names := make([]string, len(depth1))
	for i, n := range depth1 {
		depth1Names[i] = n.Name
	}
	if len(depth1) != 2 {
		t.Errorf("depth 1 = %d (%v), want 2 (B, E)", len(depth1), depth1Names)
	}
	if !contains(depth1Names, "B") || !contains(depth1Names, "E") {
		t.Errorf("depth 1 should include B and E; got %v", depth1Names)
	}

	depth2 := result.ByDepth[2]
	if len(depth2) != 1 || depth2[0].Name != "A" {
		depth2Names := make([]string, len(depth2))
		for i, n := range depth2 {
			depth2Names[i] = n.Name
		}
		t.Errorf("depth 2 = %v, want [A]", depth2Names)
	}

	if result.Summary == "" {
		t.Error("summary should not be empty")
	}
}

func TestReachableCount(t *testing.T) {
	g, _ := buildTestGraph()

	// Reverse from C: B and E depend on C directly, A transitively. 3 total.
	if got := g.reachableCount([]string{"C"}, "reverse", 10); got != 3 {
		t.Errorf("reachableCount(C, reverse) = %d, want 3", got)
	}
	// Forward from A: B, E (direct), C, D (transitive). 4 total.
	if got := g.reachableCount([]string{"A"}, "forward", 10); got != 4 {
		t.Errorf("reachableCount(A, forward) = %d, want 4", got)
	}
	// Depth limit: reverse from C at depth 1 reaches only B and E.
	if got := g.reachableCount([]string{"C"}, "reverse", 1); got != 2 {
		t.Errorf("reachableCount(C, reverse, depth 1) = %d, want 2", got)
	}
}

func TestReachableCount_Cyclic(t *testing.T) {
	g, _ := buildCyclicGraph()
	// A -> B -> C -> A. Reverse from A reaches C then B (A is the seed). 2 total,
	// and the cycle must not loop forever.
	if got := g.reachableCount([]string{"A"}, "reverse", 10); got != 2 {
		t.Errorf("reachableCount(A, reverse) on cycle = %d, want 2", got)
	}
}

func TestImpactSet_TotalDependents(t *testing.T) {
	g, _ := buildTestGraph()

	// Not truncated: total equals the shown dependents (B, E, A = 3).
	result := g.ImpactSet("C", 10, 100, false)
	if result.TotalDependents != 3 {
		t.Errorf("TotalDependents = %d, want 3", result.TotalDependents)
	}
	if strings.Contains(result.Summary, "showing") {
		t.Errorf("summary should not mention 'showing' when not truncated: %q", result.Summary)
	}
}

func TestImpactSet_TotalDependents_Truncated(t *testing.T) {
	g, _ := buildTestGraph()

	// maxNodes=2 leaves room for the seed (C) plus one dependent, so the display
	// is truncated but the total must still report all 3 dependents.
	result := g.ImpactSet("C", 10, 2, false)
	if result.TotalDependents != 3 {
		t.Errorf("TotalDependents = %d, want 3 (accurate despite cap)", result.TotalDependents)
	}
	if !result.Stats.Truncated {
		t.Error("expected Stats.Truncated with maxNodes=2")
	}
	if !strings.Contains(result.Summary, "3 total dependents (showing 1)") {
		t.Errorf("summary should report accurate total and showing count; got %q", result.Summary)
	}
}

func TestImpactSet_WithForward(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.ImpactSet("C", 10, 100, true)

	if result.Forward == nil {
		t.Fatal("forward dependencies should be included")
	}

	// C -> D (forward)
	names := nodeNames(result.Forward.Nodes)
	if !contains(names, "D") {
		t.Errorf("forward from C should include D; got %v", names)
	}
}

func TestImpactSet_LeafNode(t *testing.T) {
	g, _ := buildTestGraph()

	// D has no dependents
	result := g.ImpactSet("D", 10, 100, false)

	totalDependents := 0
	for _, nodes := range result.ByDepth {
		totalDependents += len(nodes)
	}

	// C calls D, B calls C, E calls C, A calls B and imports E
	if totalDependents < 1 {
		t.Errorf("D should have at least C as direct dependent, got %d total", totalDependents)
	}
}

func TestImpactSet_CycleHandling(t *testing.T) {
	g, _ := buildCyclicGraph()

	result := g.ImpactSet("A", 20, 100, false)

	// In a cycle A->B->C->A, impact of A is: B (depth 1 reverse from A via C->A),
	// Actually reverse: who points TO A? C points to A. Who points to C? B. Who points to B? A (already visited)
	// So: depth 1: C, depth 2: B
	totalDependents := 0
	for _, nodes := range result.ByDepth {
		totalDependents += len(nodes)
	}
	if totalDependents != 2 {
		t.Errorf("cycle impact should have 2 dependents (B, C), got %d", totalDependents)
	}
}

func TestBuildGraph_ViaStore(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "X", File: "x.go", Relations: []Relation{
			{Kind: RelCalls, Target: "Y"},
		}},
		Fact{Kind: KindSymbol, Name: "Y", File: "y.go"},
	)

	// Before BuildGraph
	if s.Graph() != nil {
		t.Error("Graph should be nil before BuildGraph")
	}

	s.BuildGraph()

	g := s.Graph()
	if g == nil {
		t.Fatal("Graph should not be nil after BuildGraph")
	}
	if g.NodeCount() != 2 {
		t.Errorf("NodeCount = %d, want 2", g.NodeCount())
	}
	if g.EdgeCount() != 1 {
		t.Errorf("EdgeCount = %d, want 1", g.EdgeCount())
	}
}

func TestBuildGraph_ClearedByStoreClear(t *testing.T) {
	s := NewStore()
	s.Add(Fact{Kind: KindSymbol, Name: "X", File: "x.go"})
	s.BuildGraph()
	if s.Graph() == nil {
		t.Fatal("Graph should exist after BuildGraph")
	}

	s.Clear()
	if s.Graph() != nil {
		t.Error("Graph should be nil after Clear")
	}
}

func TestTraverse_DefaultParameters(t *testing.T) {
	g, _ := buildTestGraph()

	// Test with zero values (should use defaults)
	result := g.Traverse("A", "forward", nil, nil, 0, 0)

	// Default maxDepth=5, maxNodes=100
	// Should still find all reachable nodes
	names := nodeNames(result.Nodes)
	if len(names) < 5 {
		t.Errorf("default params should find all reachable nodes; got %d", len(names))
	}
}

func TestFindPath_DefaultMaxDepth(t *testing.T) {
	g, _ := buildTestGraph()

	// maxDepth=0 should use default (10)
	result := g.FindPath("A", "D", nil, 0)

	if !result.Found {
		t.Error("should find path A->D with default maxDepth")
	}
}

func TestTraverse_EdgesAreRecorded(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("A", "forward", []string{RelCalls}, nil, 1, 100)

	// A --calls-> B only (depth 1, calls only)
	if len(result.Edges) != 1 {
		t.Errorf("edges = %d, want 1", len(result.Edges))
	}
	if len(result.Edges) > 0 {
		e := result.Edges[0]
		if e.Source != "A" || e.Target != "B" || e.Kind != RelCalls {
			t.Errorf("edge = %+v, want A->B calls", e)
		}
	}
}

func TestFindPath_EdgesHaveCorrectKinds(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.FindPath("A", "C", nil, 10)

	if !result.Found {
		t.Fatal("path should be found")
	}

	// Shortest path A->B->C, edges should have their relation kinds
	for _, e := range result.Edges {
		if e.Kind == "" {
			t.Error("edge kind should not be empty")
		}
	}
}

func TestNewGraph_DeduplicatesEdges(t *testing.T) {
	s := NewStore()
	// Two facts with identical relations (same source->kind->target).
	s.Add(
		Fact{Kind: KindDependency, Name: "dep1", File: "models/a.rb", Relations: []Relation{
			{Kind: RelDependsOn, Target: "User"},
		}},
		Fact{Kind: KindDependency, Name: "dep2", File: "models/b.rb", Relations: []Relation{
			{Kind: RelDependsOn, Target: "User"},
		}},
		Fact{Kind: KindSymbol, Name: "User", File: "models/user.rb"},
		// A fact with a duplicate relation on itself.
		Fact{Kind: KindModule, Name: "A", File: "a.rb", Relations: []Relation{
			{Kind: RelImports, Target: "B"},
		}},
		// Another fact that also creates the same A->imports->B edge.
		Fact{Kind: KindDependency, Name: "A -> B", File: "a.rb", Relations: []Relation{
			{Kind: RelImports, Target: "B"},
		}},
		Fact{Kind: KindModule, Name: "B", File: "b.rb"},
	)
	s.BuildGraph()
	g := s.Graph()

	// dep1->User and dep2->User are distinct source nodes, so both edges exist.
	// But A->imports->B should appear only once despite two facts creating it.
	aEdges := g.ForwardEdges("A")
	count := 0
	for _, e := range aEdges {
		if e.RelKind == RelImports && e.Target == "B" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("A->imports->B should appear exactly once, got %d", count)
	}

	// Reverse: B should have exactly one incoming imports edge from A.
	bEdges := g.ReverseEdges("B")
	count = 0
	for _, e := range bEdges {
		if e.RelKind == RelImports && e.Target == "A" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("B reverse imports from A should appear exactly once, got %d", count)
	}
}

// --- helpers ---

func nodeNames(nodes []TraversalNode) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}

func pathNames(nodes []TraversalNode) []string {
	return nodeNames(nodes)
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// TestNewGraph_CrossRepoCallNormalisation verifies that cross-repo call targets
// whose import paths include a known Go module path are normalised to the
// repo-relative fact name when building the graph.
func TestNewGraph_CrossRepoCallNormalisation(t *testing.T) {
	// Simulate two repos loaded together:
	//   - go-auth repo: root module fact with modulePath prop, plus symbol facts
	//   - golf repo: symbol with a call target using the full external import path
	facts := []Fact{
		// go-auth root module fact — carries the Go module path
		{
			Kind: KindModule,
			Name: ".",
			Repo: "go-auth",
			Props: map[string]any{
				"package":    "goauth",
				"language":   "go",
				"modulePath": "github.com/dejo1307/go-auth",
			},
		},
		// go-auth adapters module
		{Kind: KindModule, Name: "adapters", Repo: "go-auth"},
		// go-auth symbol: adapters.AuthHandler.Login
		{
			Kind: KindSymbol,
			Name: "adapters.AuthHandler.Login",
			Repo: "go-auth",
		},
		// golf symbol with an unresolved external call target
		{
			Kind: KindSymbol,
			Name: "internal/auth.LoginWrapper.Login",
			Repo: "golf",
			Relations: []Relation{
				{Kind: RelCalls, Target: "github.com/dejo1307/go-auth/adapters.AuthHandler.Login"},
			},
		},
	}

	g := NewGraph(facts)

	// The forward edge from golf's LoginWrapper.Login should point to the
	// normalised fact name "adapters.AuthHandler.Login", not the full import path.
	edges := g.ForwardEdges("internal/auth.LoginWrapper.Login")
	if len(edges) == 0 {
		t.Fatal("expected at least one forward edge from LoginWrapper.Login")
	}
	found := false
	for _, e := range edges {
		if e.RelKind == RelCalls && e.Target == "adapters.AuthHandler.Login" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected normalised call edge to adapters.AuthHandler.Login; got edges: %v", edges)
	}
}

// TestNormalizeExternalTarget verifies the helper directly.
func TestNormalizeExternalTarget(t *testing.T) {
	// Longest first, as NewGraph orders them, so the nested module claims its targets.
	mods := []string{"github.com/dejo1307/go-auth/tools", "github.com/dejo1307/go-auth"}

	cases := []struct {
		target string
		want   string
	}{
		{"github.com/dejo1307/go-auth/tools.Run", "..Run"},
		{"github.com/dejo1307/go-auth/adapters.Handler.Login", "adapters.Handler.Login"},
		{"github.com/dejo1307/go-auth.SecurityHeaders", "..SecurityHeaders"},
		{"github.com/other/lib/pkg.Type.Method", ""}, // no matching module
		{"github.com/dejo1307/go-auth", ""},          // no separator after module path
	}

	for _, tc := range cases {
		got := normalizeExternalTarget(tc.target, mods)
		if got != tc.want {
			t.Errorf("normalizeExternalTarget(%q) = %q, want %q", tc.target, got, tc.want)
		}
	}
}

// buildTypeMethodStore models a Go type with a method that makes a call, plus a
// dangling call into an unanalyzed package. The struct and method are separate
// sibling facts with no edge between them, mirroring the goextractor output.
func buildTypeMethodStore() (*Graph, *Store) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "auth.AuthHandler", File: "auth/handler.go", Line: 1,
			Props: map[string]any{"symbol_kind": SymbolStruct}},
		Fact{Kind: KindSymbol, Name: "auth.AuthHandler.Login", File: "auth/handler.go", Line: 10,
			Props: map[string]any{"symbol_kind": SymbolMethod},
			Relations: []Relation{
				{Kind: RelCalls, Target: "jwt.Sign"},
				{Kind: RelCalls, Target: "external.Unknown"}, // no backing fact
			}},
		Fact{Kind: KindSymbol, Name: "jwt.Sign", File: "jwt/jwt.go", Line: 5,
			Props: map[string]any{"symbol_kind": SymbolFunc}},
	)
	s.BuildGraph()
	return s.Graph(), s
}

func TestNewGraph_StructToMethodEdges(t *testing.T) {
	g, _ := buildTypeMethodStore()

	var found bool
	for _, e := range g.ForwardEdges("auth.AuthHandler") {
		if e.RelKind == RelHasMethod && e.Target == "auth.AuthHandler.Login" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected has_method edge auth.AuthHandler -> auth.AuthHandler.Login, got %+v", g.ForwardEdges("auth.AuthHandler"))
	}

	// A package-level function whose owner ("jwt") is not a type must NOT get a
	// has_method edge.
	for _, e := range g.ForwardEdges("jwt") {
		if e.RelKind == RelHasMethod {
			t.Errorf("unexpected has_method edge from non-type owner: %+v", e)
		}
	}
}

func TestTraverse_ForwardFromStructSurfacesMethodCalls(t *testing.T) {
	g, _ := buildTypeMethodStore()

	result := g.Traverse("auth.AuthHandler", "forward", nil, nil, 5, 100)

	names := nodeNames(result.Nodes)
	for _, want := range []string{"auth.AuthHandler.Login", "jwt.Sign"} {
		if !contains(names, want) {
			t.Errorf("forward traverse from struct missing %q; got %v", want, names)
		}
	}
}

// buildRubyMethodStore mirrors the rubyextractor's naming: an instance method is a
// sibling fact named "<class>#<method>", a singleton method "<class>.<method>", and
// a def outside any class "<dir>.<method>". It also carries the two shapes that must
// stay unwired — a "#" name whose owner no fact declares, and one whose owner is a
// symbol that is not a type.
func buildRubyMethodStore() *Graph {
	s := NewStore()
	ruby := func(kind string) map[string]any {
		return map[string]any{"symbol_kind": kind, "language": "ruby"}
	}
	s.Add(
		Fact{Kind: KindSymbol, Name: "Analytics::GoogleTagManager", File: "app/models/analytics/google_tag_manager.rb", Line: 1,
			Props: ruby(SymbolClass)},
		Fact{Kind: KindSymbol, Name: "Analytics::GoogleTagManager#render?", File: "app/models/analytics/google_tag_manager.rb", Line: 12,
			Props:     ruby(SymbolMethod),
			Relations: []Relation{{Kind: RelCalls, Target: "Analytics::Consent.granted?"}}},
		Fact{Kind: KindSymbol, Name: "Analytics::GoogleTagManager.configure", File: "app/models/analytics/google_tag_manager.rb", Line: 4,
			Props: ruby(SymbolFunc)},
		Fact{Kind: KindSymbol, Name: "Analytics::Consent", File: "app/models/analytics/consent.rb", Line: 1,
			Props: ruby(SymbolClass)},
		Fact{Kind: KindSymbol, Name: "Analytics::Consent.granted?", File: "app/models/analytics/consent.rb", Line: 3,
			Props: ruby(SymbolFunc)},
		Fact{Kind: KindSymbol, Name: "Comparable#<=>", File: "app/models/analytics/consent.rb", Line: 20,
			Props: ruby(SymbolMethod)},
		Fact{Kind: KindSymbol, Name: "settings", File: "config/settings.rb", Line: 1,
			Props: ruby(SymbolVariable)},
		Fact{Kind: KindSymbol, Name: "settings#reload", File: "config/settings.rb", Line: 5,
			Props: ruby(SymbolMethod)},
		Fact{Kind: KindSymbol, Name: "lib/tasks.reindex", File: "lib/tasks/reindex.rb", Line: 1,
			Props: ruby(SymbolMethod)},
	)
	s.BuildGraph()
	return s.Graph()
}

func hasMethodEdge(g *Graph, owner, method string) bool {
	for _, e := range g.ForwardEdges(owner) {
		if e.RelKind == RelHasMethod && e.Target == method {
			return true
		}
	}
	return false
}

func ownedBy(g *Graph, method string) []string {
	var owners []string
	for _, e := range g.ReverseEdges(method) {
		if e.RelKind == RelHasMethod {
			owners = append(owners, e.Target)
		}
	}
	return owners
}

func TestNewGraph_ClassToRubyInstanceMethodEdges(t *testing.T) {
	g := buildRubyMethodStore()

	if !hasMethodEdge(g, "Analytics::GoogleTagManager", "Analytics::GoogleTagManager#render?") {
		t.Errorf("expected has_method edge to the instance method, got %+v", g.ForwardEdges("Analytics::GoogleTagManager"))
	}
	if !hasMethodEdge(g, "Analytics::GoogleTagManager", "Analytics::GoogleTagManager.configure") {
		t.Errorf("expected has_method edge to the singleton method, got %+v", g.ForwardEdges("Analytics::GoogleTagManager"))
	}
}

func TestNewGraph_RubyMethodOwnerMustBeAKnownType(t *testing.T) {
	g := buildRubyMethodStore()

	for _, method := range []string{"Comparable#<=>", "settings#reload", "lib/tasks.reindex"} {
		if owners := ownedBy(g, method); len(owners) > 0 {
			t.Errorf("%q was wired to %v; its owner is not a known type", method, owners)
		}
	}
}

func TestTraverse_ForwardFromRubyClassSurfacesInstanceMethodCalls(t *testing.T) {
	g := buildRubyMethodStore()

	result := g.Traverse("Analytics::GoogleTagManager", "forward", nil, nil, 5, 100)

	names := nodeNames(result.Nodes)
	for _, want := range []string{"Analytics::GoogleTagManager#render?", "Analytics::Consent.granted?"} {
		if !contains(names, want) {
			t.Errorf("forward traverse from the class missing %q; got %v", want, names)
		}
	}
}

// The separator that splits a method from its owner is "#" wherever one appears; a
// "." earlier in the name belongs to the owner.
func TestNewGraph_HashSeparatorWinsOverDot(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "Reports", File: "reports.rb", Line: 1,
			Props: map[string]any{"symbol_kind": SymbolClass}},
		Fact{Kind: KindSymbol, Name: "Reports.Weekly", File: "reports.rb", Line: 5,
			Props: map[string]any{"symbol_kind": SymbolClass}},
		Fact{Kind: KindSymbol, Name: "Reports.Weekly#render", File: "reports.rb", Line: 9,
			Props: map[string]any{"symbol_kind": SymbolMethod}},
	)
	s.BuildGraph()
	g := s.Graph()

	if got := ownedBy(g, "Reports.Weekly#render"); len(got) != 1 || got[0] != "Reports.Weekly" {
		t.Errorf("owner of Reports.Weekly#render = %v, want [Reports.Weekly]", got)
	}
}

func TestTraverse_UnresolvedTargetMarked(t *testing.T) {
	g, _ := buildTypeMethodStore()

	result := g.Traverse("auth.AuthHandler.Login", "forward", nil, nil, 5, 100)

	var sawUnresolved, sawResolved bool
	for _, n := range result.Nodes {
		switch n.Name {
		case "external.Unknown":
			sawUnresolved = true
			if !n.Unresolved {
				t.Error("external.Unknown should be marked Unresolved")
			}
		case "jwt.Sign":
			sawResolved = true
			if n.Unresolved {
				t.Error("jwt.Sign is a real fact and must not be marked Unresolved")
			}
		}
	}
	if !sawUnresolved {
		t.Error("expected an unresolved node external.Unknown in the result")
	}
	if !sawResolved {
		t.Error("expected the resolved node jwt.Sign in the result")
	}
}

// buildCrossRepoTypeStore models a go-auth library type (struct + method +
// constructor) consumed by a golf caller, mirroring how the cross-repo call
// normalisation lands golf's calls onto go-auth's local symbol names.
func buildCrossRepoTypeStore() (*Graph, *Store) {
	s := NewStore()
	s.Add(
		// go-auth: the AuthHandler type, one method, and its constructor.
		Fact{Kind: KindSymbol, Name: "adapters.AuthHandler", File: "adapters/h.go", Line: 1, Repo: "go-auth",
			Props: map[string]any{"symbol_kind": SymbolStruct}},
		Fact{Kind: KindSymbol, Name: "adapters.AuthHandler.Login", File: "adapters/h.go", Line: 10, Repo: "go-auth",
			Props: map[string]any{"symbol_kind": SymbolMethod}},
		Fact{Kind: KindSymbol, Name: "adapters.NewAuthHandler", File: "adapters/h.go", Line: 30, Repo: "go-auth",
			Props: map[string]any{"symbol_kind": SymbolFunc}},
		// golf: a setup function that calls the constructor and a method.
		Fact{Kind: KindSymbol, Name: "pkg/auth.Setup", File: "pkg/auth/setup.go", Line: 28, Repo: "golf",
			Props: map[string]any{"symbol_kind": SymbolFunc}, Relations: []Relation{
				{Kind: RelCalls, Target: "adapters.NewAuthHandler"},
				{Kind: RelCalls, Target: "adapters.AuthHandler.Login"},
			}},
	)
	s.BuildGraph()
	return s.Graph(), s
}

func TestImpactSet_TypeRollup(t *testing.T) {
	g, _ := buildCrossRepoTypeStore()

	// Impact on the bare struct must roll up its method + constructor and surface
	// the cross-repo caller (previously this returned "no dependents").
	res := g.ImpactSet("adapters.AuthHandler", 3, 100, false)

	names := nodeNames(impactNodes(res))
	if !contains(names, "pkg/auth.Setup") {
		t.Errorf("type rollup did not surface cross-repo caller pkg/auth.Setup; got %v", names)
	}
	// Seeds (the type entity itself) must not appear as dependents.
	for _, seed := range []string{"adapters.AuthHandler", "adapters.AuthHandler.Login", "adapters.NewAuthHandler"} {
		if contains(names, seed) {
			t.Errorf("seed %q should be excluded from dependents", seed)
		}
	}
	if !reflect.DeepEqual(res.CrossRepoImpact, []string{"golf"}) {
		t.Errorf("CrossRepoImpact = %v, want [golf]", res.CrossRepoImpact)
	}
}

func TestImpactSet_CrossRepoImpactField(t *testing.T) {
	g, _ := buildCrossRepoTypeStore()

	// A plain function target (not a type) still reports cross-repo dependents and
	// per-node repo.
	res := g.ImpactSet("adapters.NewAuthHandler", 2, 100, false)

	if !reflect.DeepEqual(res.CrossRepoImpact, []string{"golf"}) {
		t.Fatalf("CrossRepoImpact = %v, want [golf]", res.CrossRepoImpact)
	}
	var sawRepo bool
	for _, nodes := range res.ByDepth {
		for _, n := range nodes {
			if n.Name == "pkg/auth.Setup" {
				sawRepo = true
				if n.Repo != "golf" {
					t.Errorf("dependent node repo = %q, want golf", n.Repo)
				}
			}
		}
	}
	if !sawRepo {
		t.Error("expected pkg/auth.Setup among dependents")
	}
}

func TestImpactSet_NonTypeUnchanged(t *testing.T) {
	// Single-repo graph (no Repo tags): rollup is inert and CrossRepoImpact stays nil.
	g, _ := buildTestGraph()
	res := g.ImpactSet("D", 5, 100, false)

	if res.CrossRepoImpact != nil {
		t.Errorf("CrossRepoImpact should be nil for same-repo graph, got %v", res.CrossRepoImpact)
	}
	// D is called by C (a module here) — at least one dependent is found, as before.
	if len(impactNodes(res)) == 0 {
		t.Error("expected at least one dependent for D")
	}
}

// TestTraverseFrom_ReverseTypeRollup documents the #6 fix: reverse traversal of a
// bare type finds nothing (its reverse adjacency is empty — callers reference its
// methods/constructor, not the type), but seeding with RollupSeeds surfaces the
// cross-repo caller, matching impact_analysis.
func TestTraverseFrom_ReverseTypeRollup(t *testing.T) {
	g, _ := buildCrossRepoTypeStore()

	// Bare reverse traversal of the type: no callers reference the type directly.
	bare := g.Traverse("adapters.AuthHandler", "reverse", nil, nil, 3, 100)
	if got := nodeNames(bare.Nodes); contains(got, "pkg/auth.Setup") {
		t.Errorf("bare reverse unexpectedly found the caller; got %v", got)
	}

	// Seeded with the type's methods + constructor: the cross-repo caller appears.
	seeded := g.TraverseFrom(g.RollupSeeds("adapters.AuthHandler"), "reverse", nil, nil, 3, 100)
	if got := nodeNames(seeded.Nodes); !contains(got, "pkg/auth.Setup") {
		t.Errorf("seeded reverse did not surface cross-repo caller pkg/auth.Setup; got %v", got)
	}
}

// impactNodes flattens an ImpactResult's depth buckets into a node slice.
func impactNodes(res ImpactResult) []TraversalNode {
	var out []TraversalNode
	for _, nodes := range res.ByDepth {
		out = append(out, nodes...)
	}
	return out
}

// TestArchitecturalReverse_ExcludesReferenceKinds verifies that reference-only
// facts (test_ref/file_ref) are dropped from the architectural reverse index.
// Their RelCalls edges must not count as dependents — otherwise they inflate
// god-class fan-in and hotspots centrality and drift the outlier threshold
// (GAP-XL-15). The unfiltered Reverse() index must still surface them, since
// orphans/impact_analysis rely on seeing test/file references.
func TestArchitecturalReverse_ExcludesReferenceKinds(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "Prod", File: "app/prod.rb"},
		Fact{Kind: KindSymbol, Name: "Caller", File: "app/caller.rb", Relations: []Relation{
			{Kind: RelCalls, Target: "Prod"},
		}},
		Fact{Kind: KindTestRef, Name: "spec/prod_spec.rb", File: "spec/prod_spec.rb", Relations: []Relation{
			{Kind: RelCalls, Target: "Prod"},
		}},
		Fact{Kind: KindFileRef, Name: "config/initializers/boot.rb", File: "config/initializers/boot.rb", Relations: []Relation{
			{Kind: RelCalls, Target: "Prod"},
		}},
	)
	s.BuildGraph()
	g := s.Graph()

	// Unfiltered: all three sources (symbol + test_ref + file_ref) are dependents.
	if got := len(g.ReverseEdges("Prod")); got != 3 {
		t.Fatalf("Reverse()[Prod] = %d edges, want 3 (Caller + test_ref + file_ref)", got)
	}

	// Architectural: only the symbol dependent survives.
	arch := g.ArchitecturalReverseEdges("Prod")
	if len(arch) != 1 {
		t.Fatalf("ArchitecturalReverse()[Prod] = %d edges, want 1 (symbol only): %+v", len(arch), arch)
	}
	// In a reverse edge, Edge.Target holds the SOURCE fact name.
	if arch[0].Target != "Caller" {
		t.Errorf("surviving dependent = %q, want Caller (the symbol source)", arch[0].Target)
	}
}

// TestArchitecturalReverse_ExcludesRouteSources pins collateral introduced by the
// v111 handled_by binder (new/18) and caught by diff_snapshot, not by any test.
//
// bindHTTPHandlers made routes a SOURCE of edges into handler symbols, so on
// fairwayhub/golf all 1254 bound handlers gained fan-in — and 13 of them were
// immediately reported as new "call-graph hotspots". Inspecting one:
//
//	internal/adapters/http/plan.HandlerV2.UpdatePlan — fan-in 4:
//	   [route]    --handled_by--> from /plans/{id:[0-9]+}
//	   [route]    --handled_by--> from /plans/{id}
//	   [test_ref] --calls-->      from handler_v2_test.go
//
// Not one real symbol caller. The handler was ranked a change-risk concentrator purely
// because it is a handler — and every handler gained the same uniform +1, which also
// drifts the mean+2σ threshold and pushed three genuine findings out.
//
// A route is not a symbol; it is an entry-point declaration. That is exactly what
// isReferenceOnlyKind is for (GAP-XL-15). orphans, impact_analysis, traverse and
// find_path keep the unfiltered Reverse() and still see handled_by — which is the whole
// point of emitting it.
func TestArchitecturalReverse_ExcludesRouteSources(t *testing.T) {
	s := NewStore()
	s.Add(Fact{Kind: KindSymbol, Name: "http/plan.HandlerV2.UpdatePlan", File: "http/plan/h.go"})
	s.Add(Fact{Kind: KindSymbol, Name: "http/plan.HandlerV2.caller", File: "http/plan/h.go",
		Relations: []Relation{{Kind: RelCalls, Target: "http/plan.HandlerV2.UpdatePlan"}}})
	s.Add(Fact{Kind: KindRoute, Name: "/plans/{id}", File: "bootstrap/routes.go",
		Relations: []Relation{{Kind: RelHandledBy, Target: "http/plan.HandlerV2.UpdatePlan"}}})
	s.BuildGraph()
	g := s.Graph()

	// The architectural view counts the one real symbol caller, not the route.
	if got := len(g.ArchitecturalReverseEdges("http/plan.HandlerV2.UpdatePlan")); got != 1 {
		t.Errorf("architectural fan-in = %d, want 1 — a route is not an architectural dependent", got)
	}
	// But the handled_by edge must still be there for everyone else: it is what lets
	// impact_analysis walk a route to its handler, and the perf analyzer escalate it.
	var sawRoute bool
	for _, e := range g.ReverseEdges("http/plan.HandlerV2.UpdatePlan") {
		if e.Target == "/plans/{id}" && e.RelKind == RelHandledBy {
			sawRoute = true
		}
	}
	if !sawRoute {
		t.Error("Reverse() lost the handled_by edge — impact_analysis and perf depend on it")
	}
}

// ArchitecturalReverse must drop RelInstantiates edges (a data struct built at
// many sites is not architectural coupling), while Reverse keeps them and
// Forward keeps the constructing side — so traversal/impact/orphans are
// unaffected and the god-class/hotspots fan-in is corrected.
func TestArchitecturalReverse_ExcludesInstantiate(t *testing.T) {
	s := NewStore()
	s.Add(Fact{Kind: KindSymbol, Name: "pkg.Data", File: "pkg/data.go"})
	s.Add(Fact{Kind: KindSymbol, Name: "pkg.build", File: "pkg/build.go",
		Relations: []Relation{{Kind: RelInstantiates, Target: "pkg.Data"}}})
	s.Add(Fact{Kind: KindSymbol, Name: "pkg.call", File: "pkg/call.go",
		Relations: []Relation{{Kind: RelCalls, Target: "pkg.Data"}}})
	s.BuildGraph()
	g := s.Graph()

	hasSrc := func(edges []Edge, src string) bool {
		for _, e := range edges {
			if e.Target == src {
				return true
			}
		}
		return false
	}

	// Raw reverse keeps both the instantiate and the call source.
	rev := g.ReverseEdges("pkg.Data")
	if !hasSrc(rev, "pkg.build") || !hasSrc(rev, "pkg.call") {
		t.Errorf("ReverseEdges should keep both instantiate and call sources; got %v", rev)
	}
	// The architectural view drops the instantiate source, keeps the call source.
	arch := g.ArchitecturalReverseEdges("pkg.Data")
	if hasSrc(arch, "pkg.build") {
		t.Errorf("ArchitecturalReverseEdges must exclude the RelInstantiates source; got %v", arch)
	}
	if !hasSrc(arch, "pkg.call") {
		t.Errorf("ArchitecturalReverseEdges must keep the RelCalls source; got %v", arch)
	}
	// The count must agree with the filtered edge list it summarizes.
	if got, want := g.ArchitecturalFanIn("pkg.Data"), len(arch); got != want {
		t.Errorf("ArchitecturalFanIn = %d but ArchitecturalReverseEdges has %d", got, want)
	}
	// Forward keeps the constructing side (fan-out is unaffected).
	if !hasSrc(g.ForwardEdges("pkg.build"), "pkg.Data") {
		t.Errorf("ForwardEdges must keep the instantiate edge; got %v", g.ForwardEdges("pkg.build"))
	}
}

// TestArchitecturalForwardEdges_ExcludeOwnedMethods — a type's has_method edges point
// at its own body, so they are containment rather than outgoing coupling. Counting
// them turns out-degree into a size measure, and the explainer that reads it calls the
// number "calls out to". The raw forward index still carries them: traversal,
// impact_analysis and find_path are what those edges were synthesized for.
func TestArchitecturalForwardEdges_ExcludeOwnedMethods(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "Batch", File: "app/batch.rb",
			Props:     map[string]any{"symbol_kind": SymbolClass},
			Relations: []Relation{{Kind: RelCalls, Target: "Runner.run"}}},
		Fact{Kind: KindSymbol, Name: "Batch#first", File: "app/batch.rb",
			Props: map[string]any{"symbol_kind": SymbolMethod}},
		Fact{Kind: KindSymbol, Name: "Batch#second", File: "app/batch.rb",
			Props: map[string]any{"symbol_kind": SymbolMethod}},
		Fact{Kind: KindSymbol, Name: "Runner", File: "app/runner.rb",
			Props: map[string]any{"symbol_kind": SymbolClass}},
		Fact{Kind: KindSymbol, Name: "Runner.run", File: "app/runner.rb",
			Props: map[string]any{"symbol_kind": SymbolFunc}},
	)
	s.BuildGraph()
	g := s.Graph()

	if got, want := g.FanOut("Batch"), 3; got != want {
		t.Fatalf("precondition: FanOut(Batch) = %d, want %d (one call plus two owned methods)", got, want)
	}
	arch := g.ArchitecturalForwardEdges("Batch")
	if len(arch) != 1 || arch[0].Target != "Runner.run" {
		t.Errorf("ArchitecturalForwardEdges(Batch) = %v, want only the call to Runner.run", arch)
	}
	if got, want := g.ArchitecturalFanOut("Batch"), len(arch); got != want {
		t.Errorf("ArchitecturalFanOut = %d but ArchitecturalForwardEdges has %d", got, want)
	}
	if got := g.ArchitecturalFanOut("no.such.symbol"); got != 0 {
		t.Errorf("ArchitecturalFanOut(absent) = %d, want 0", got)
	}
	if got := g.ArchitecturalForwardEdges("no.such.symbol"); got != nil {
		t.Errorf("ArchitecturalForwardEdges(absent) = %v, want nil", got)
	}
}

func TestArchitecturalReverse_ExcludesImplementedBy(t *testing.T) {
	s := NewStore()
	s.Add(Fact{Kind: KindSymbol, Name: "pkg.Publish"})
	s.Add(Fact{Kind: KindStorage, Name: "orders.a", Relations: []Relation{{Kind: RelImplementedBy, Target: "pkg.Publish"}}})
	s.Add(Fact{Kind: KindStorage, Name: "orders.b", Relations: []Relation{{Kind: RelImplementedBy, Target: "pkg.Publish"}}})
	s.BuildGraph()
	if got := s.Graph().ArchitecturalFanIn("pkg.Publish"); got != 0 {
		t.Fatalf("implemented_by edges fabricated fan-in: got %d, want 0", got)
	}
}

// --- name-collision disambiguation ---

// buildCollisionGraph mirrors the real multi-repo shape that exposed the bug: a repo
// labelled "svc" (a synthetic service node) and, in a DIFFERENT repo, a top-level
// directory also called "svc". Module fact names are repo-root-relative and not
// repo-prefixed, so the two collide on the name "svc".
//
// The service fact is added LAST on purpose: synthetic cross-repo facts are appended
// after every repo's own facts, so a first-fact-wins index always picks the module and
// never the service.
func buildCollisionGraph() *Graph {
	s := NewStore()
	s.Add(
		// "client" repo: a module that imports the colliding name.
		Fact{Kind: KindModule, Name: "client/app", File: "client/app", Repo: "client",
			Relations: []Relation{{Kind: RelImports, Target: "svc"}}},
		// "other" repo: a top-level directory that happens to be named "svc".
		Fact{Kind: KindModule, Name: "svc", File: "other/svc", Repo: "other"},
		// Synthetic service nodes, appended last.
		Fact{Kind: KindService, Name: "web", Relations: []Relation{
			{Kind: RelDependsOn, Target: "svc"},
		}},
		Fact{Kind: KindService, Name: "svc", Relations: []Relation{
			{Kind: RelDependsOn, Target: "db"},
		}},
		Fact{Kind: KindService, Name: "db"},
	)
	s.BuildGraph()
	return s.Graph()
}

// TestNodeFor_PrefersKindMatchingIncomingRelation pins the core fix: the relation used
// to reach a node decides which of the same-named facts it means. Reached by
// depends_on it is the service; reached by imports it is the module.
func TestNodeFor_PrefersKindMatchingIncomingRelation(t *testing.T) {
	g := buildCollisionGraph()

	svcNode := g.nodeFor("svc", 1, RelDependsOn)
	if svcNode.Kind != KindService {
		t.Errorf("via depends_on: kind = %q, want %q", svcNode.Kind, KindService)
	}
	if svcNode.File != "" || svcNode.Repo != "" {
		t.Errorf("via depends_on: got file=%q repo=%q, want the service fact's empty metadata",
			svcNode.File, svcNode.Repo)
	}

	modNode := g.nodeFor("svc", 1, RelImports)
	if modNode.Kind != KindModule {
		t.Errorf("via imports: kind = %q, want %q", modNode.Kind, KindModule)
	}
	if modNode.File != "other/svc" {
		t.Errorf("via imports: file = %q, want other/svc", modNode.File)
	}
}

// TestNodeFor_FallsBackToKindRank covers a node with no edge context (a traversal or
// path origin): the choice must still be deterministic, and service outranks module.
func TestNodeFor_FallsBackToKindRank(t *testing.T) {
	g := buildCollisionGraph()
	if got := g.nodeFor("svc", 0, "").Kind; got != KindService {
		t.Errorf("no via-relation: kind = %q, want %q", got, KindService)
	}
	// An unknown relation has no natural kind and must fall back the same way.
	if got := g.nodeFor("svc", 0, "no_such_relation").Kind; got != KindService {
		t.Errorf("unknown via-relation: kind = %q, want %q", got, KindService)
	}
}

// TestNodeFor_ConflatedReportsSharedKinds pins the honesty signal: a node whose name is
// shared says so, and an ordinary node stays silent.
func TestNodeFor_ConflatedReportsSharedKinds(t *testing.T) {
	g := buildCollisionGraph()

	got := g.nodeFor("svc", 1, RelDependsOn).Conflated
	if want := []string{KindModule, KindService}; !reflect.DeepEqual(got, want) {
		t.Errorf("Conflated = %v, want %v (sorted)", got, want)
	}
	if got := g.nodeFor("db", 1, RelDependsOn).Conflated; got != nil {
		t.Errorf("single-fact node Conflated = %v, want nil", got)
	}
}

// TestNodeFor_UnresolvedStillMarked guards the pre-existing behaviour for a name with
// no backing fact at all.
func TestNodeFor_UnresolvedStillMarked(t *testing.T) {
	g := buildCollisionGraph()
	n := g.nodeFor("nowhere", 1, RelCalls)
	if !n.Unresolved {
		t.Error("expected Unresolved for a name with no backing fact")
	}
	if n.Kind != "" || n.Conflated != nil {
		t.Errorf("unresolved node should carry no kind/conflation, got kind=%q conflated=%v",
			n.Kind, n.Conflated)
	}
}

// TestTraverse_ServiceFilterKeepsCollidingServiceNode is the regression test for the
// user-visible symptom: filtering a service-to-service walk by node_kinds=["service"]
// used to DROP the colliding hop, because it was labelled a module. The node must now
// survive the filter and the walk must still reach the far side.
func TestTraverse_ServiceFilterKeepsCollidingServiceNode(t *testing.T) {
	g := buildCollisionGraph()
	res := g.Traverse("web", "forward", []string{RelDependsOn}, []string{KindService}, 3, 100)

	var names []string
	for _, n := range res.Nodes {
		if n.Depth > 0 {
			names = append(names, n.Name)
		}
	}
	want := []string{"svc", "db"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("service-filtered traversal = %v, want %v (the colliding hop must not be filtered out)", names, want)
	}
}

// TestFindPath_LabelsHopsByTraversingRelation is the regression test for the reported
// find_path output, where the middle hop of a service-to-service path was reported as
// an unrelated module in another repo.
func TestFindPath_LabelsHopsByTraversingRelation(t *testing.T) {
	g := buildCollisionGraph()
	res := g.FindPath("web", "db", nil, 5)
	if !res.Found {
		t.Fatalf("expected a path web -> svc -> db, got %+v", res)
	}
	if len(res.Path) != 3 {
		t.Fatalf("path length = %d, want 3; path=%+v", len(res.Path), res.Path)
	}
	mid := res.Path[1]
	if mid.Name != "svc" || mid.Kind != KindService {
		t.Errorf("middle hop = %s (%s), want svc (service)", mid.Name, mid.Kind)
	}
	if mid.Repo != "" {
		t.Errorf("middle hop repo = %q, want empty (it is the service, not the other repo's module)", mid.Repo)
	}
	if len(mid.Conflated) != 2 {
		t.Errorf("middle hop should report conflation, got %v", mid.Conflated)
	}
}

// TestImpactSet_GoverningIntent pins the intent-aware traversal: an impact
// analysis on a fact whose file is covered by a declared anchor — exactly or
// as a directory prefix — reports the anchoring pages with their type and
// status, and a fact nothing anchors reports none.
func TestImpactSet_GoverningIntent(t *testing.T) {
	ff := []Fact{
		{Kind: KindSymbol, Repo: "backend", File: "app/services/formatter.rb", Name: "Formatter"},
		{Kind: KindSymbol, Repo: "backend", File: "app/jobs/sync_job.rb", Name: "SyncJob"},
		{Kind: KindSymbol, Repo: "backend", File: "lib/untouched.rb", Name: "Untouched"},
		{Kind: KindIntent, Repo: "wiki", File: "wiki/adrs/fmt.md", Name: "page: wiki/adrs/fmt.md",
			Props: map[string]any{"intent_kind": "page", "page_type": "decision", "status": "accepted"}},
		{Kind: KindIntent, Repo: "wiki", File: "wiki/adrs/fmt.md", Name: "anchor: backend app/services/formatter.rb",
			Props: map[string]any{"intent_kind": "anchor", "intent_owner": "backend", "path": "app/services/formatter.rb", "source": "wiki/adrs/fmt.md"}},
		{Kind: KindIntent, Repo: "wiki", File: "wiki/prds/jobs.md", Name: "anchor: backend app/jobs",
			Props: map[string]any{"intent_kind": "anchor", "intent_owner": "backend", "path": "app/jobs", "source": "wiki/prds/jobs.md"}},
	}
	g := NewGraph(ff)

	got := g.ImpactSet("Formatter", 3, 100, false)
	if len(got.GoverningIntent) != 1 || got.GoverningIntent[0].Page != "wiki/adrs/fmt.md" {
		t.Fatalf("file anchor must govern its symbol, got %+v", got.GoverningIntent)
	}
	if got.GoverningIntent[0].Type != "decision" || got.GoverningIntent[0].Status != "accepted" {
		t.Fatalf("governing page must join type/status from the page declaration, got %+v", got.GoverningIntent[0])
	}
	if !strings.Contains(got.Summary, "governed by 1 intent page(s)") {
		t.Fatalf("summary must mention governing intent, got %q", got.Summary)
	}

	dir := g.ImpactSet("SyncJob", 3, 100, false)
	if len(dir.GoverningIntent) != 1 || dir.GoverningIntent[0].Page != "wiki/prds/jobs.md" {
		t.Fatalf("directory anchor must govern files under it, got %+v", dir.GoverningIntent)
	}
	if dir.GoverningIntent[0].Type != "" {
		t.Fatalf("anchor without a page declaration joins no type, got %+v", dir.GoverningIntent[0])
	}

	none := g.ImpactSet("Untouched", 3, 100, false)
	if len(none.GoverningIntent) != 0 {
		t.Fatalf("unanchored code must report no governing intent, got %+v", none.GoverningIntent)
	}
}

// TestGoverningIntent_RelationTrail pins the trail past the first hop: a
// governing page's outgoing relations ride along, with target type/status
// joined when the target compiles — in either file form — and only its path
// when it does not.
func TestGoverningIntent_RelationTrail(t *testing.T) {
	ff := []Fact{
		{Kind: KindSymbol, Repo: "backend", File: "app/services/formatter.rb", Name: "Formatter"},
		{Kind: KindIntent, Repo: "wiki", File: "wiki/adrs/fmt.md", Name: "page: wiki/adrs/fmt.md",
			Props: map[string]any{"intent_kind": "page", "page_type": "decision", "status": "accepted"}},
		{Kind: KindIntent, Repo: "wiki", File: "wiki/adrs/fmt.md", Name: "anchor: backend app/services/formatter.rb",
			Props: map[string]any{"intent_kind": "anchor", "intent_owner": "backend", "path": "app/services/formatter.rb", "source": "wiki/adrs/fmt.md"}},
		{Kind: KindIntent, Repo: "wiki", File: "wiki/adrs/fmt.md", Name: "relation: part-of wiki/epics/text.md",
			Props: map[string]any{"intent_kind": "relation", "rel": "part-of", "to": "wiki/epics/text.md", "source": "wiki/adrs/fmt.md"}},
		{Kind: KindIntent, Repo: "wiki", File: "wiki/adrs/fmt.md", Name: "relation: depends-on wiki/prds/fmt.md",
			Props: map[string]any{"intent_kind": "relation", "rel": "depends-on", "to": "wiki/prds/fmt.md", "source": "wiki/adrs/fmt.md"}},
		{Kind: KindIntent, Repo: "wiki", File: "wiki/epics/text.md", Name: "page: wiki/epics/text.md",
			Props: map[string]any{"intent_kind": "page", "page_type": "epic", "status": "living"}},
	}
	g := NewGraph(ff)

	got := g.GoverningIntent("Formatter")
	if len(got) != 1 || len(got[0].Relations) != 2 {
		t.Fatalf("governing page must carry its two relations, got %+v", got)
	}
	dep, part := got[0].Relations[0], got[0].Relations[1]
	if dep.Rel != "depends-on" || dep.To != "wiki/prds/fmt.md" || dep.ToType != "" {
		t.Fatalf("relation to an uncompiled target keeps only its path, got %+v", dep)
	}
	if part.Rel != "part-of" || part.ToType != "epic" || part.ToStatus != "living" {
		t.Fatalf("relation target type/status must join from the compiled target, got %+v", part)
	}
}

// TestGoverningIntentForFile answers the reverse query for a file named
// directly, in both the repo-relative and label-prefixed forms.
func TestGoverningIntentForFile(t *testing.T) {
	ff := []Fact{
		{Kind: KindSymbol, Repo: "backend", File: "backend/app/jobs/sync_job.rb", Name: "SyncJob"},
		{Kind: KindIntent, Repo: "wiki", File: "wiki/prds/jobs.md", Name: "anchor: backend app/jobs",
			Props: map[string]any{"intent_kind": "anchor", "intent_owner": "backend", "path": "app/jobs", "source": "wiki/prds/jobs.md"}},
	}
	g := NewGraph(ff)

	for _, file := range []string{"app/jobs/sync_job.rb", "backend/app/jobs/sync_job.rb"} {
		got := g.GoverningIntentForFile("backend", file)
		if len(got) != 1 || got[0].Page != "wiki/prds/jobs.md" {
			t.Fatalf("file form %q must resolve its governing page, got %+v", file, got)
		}
	}
	if got := g.GoverningIntentForFile("backend", "lib/other.rb"); len(got) != 0 {
		t.Fatalf("ungoverned file must report nothing, got %+v", got)
	}
}

// TestGovernedByPage pins the forward half of the reverse query: a page
// resolves in either file form, reports each anchor's measured coverage,
// and the found flag keeps "no such page" apart from "anchored to nothing".
func TestGovernedByPage(t *testing.T) {
	ff := []Fact{
		{Kind: KindSymbol, Repo: "backend", File: "app/jobs/sync_job.rb", Name: "SyncJob"},
		{Kind: KindSymbol, Repo: "backend", File: "app/jobs/retry_job.rb", Name: "RetryJob"},
		{Kind: KindModule, Repo: "backend", File: "app/jobs/sync_job.rb", Name: "jobs"},
		{Kind: KindIntent, Repo: "wiki", File: "wiki/prds/jobs.md", Name: "page: wiki/prds/jobs.md",
			Props: map[string]any{"intent_kind": "page", "page_type": "initiative", "status": "living"}},
		{Kind: KindIntent, Repo: "wiki", File: "wiki/prds/jobs.md", Name: "anchor: backend app/jobs",
			Props: map[string]any{"intent_kind": "anchor", "intent_owner": "backend", "path": "app/jobs", "source": "wiki/prds/jobs.md"}},
		{Kind: KindIntent, Repo: "wiki", File: "wiki/prds/jobs.md", Name: "anchor: backend docs/jobs.md",
			Props: map[string]any{"intent_kind": "anchor", "intent_owner": "backend", "path": "docs/jobs.md", "source": "wiki/prds/jobs.md"}},
	}
	g := NewGraph(ff)

	for _, page := range []string{"wiki/prds/jobs.md", "prds/jobs.md"} {
		cov, found := g.GovernedByPage(page)
		if !found || len(cov) != 2 {
			t.Fatalf("page form %q must resolve with two anchors, got found=%v cov=%+v", page, found, cov)
		}
		if cov[0].Path != "app/jobs" || cov[0].MeasuredFiles != 2 || cov[0].MeasuredFacts != 3 {
			t.Fatalf("directory anchor coverage must count measured files and facts, got %+v", cov[0])
		}
		if cov[1].Path != "docs/jobs.md" || cov[1].MeasuredFacts != 0 {
			t.Fatalf("unmeasured anchor must report zero coverage, got %+v", cov[1])
		}
	}
	if _, found := g.GovernedByPage("wiki/prds/missing.md"); found {
		t.Fatal("a page outside the compiled set must report not found")
	}
	if !g.HasCompiledPages() {
		t.Fatal("a graph with a page node must report compiled pages")
	}
	if NewGraph([]Fact{{Kind: KindSymbol, Repo: "backend", File: "a.rb", Name: "A"}}).HasCompiledPages() {
		t.Fatal("a graph without page nodes must not report compiled pages")
	}
}

// An edge whose target names no fact lands on the node its alias names, for a call
// and for a dependency's package import alike, so a traversal crosses into the
// repository the alias points at.
func TestGraph_FollowsTargetAliases(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindModule, Name: "src/catalog", File: "src/catalog", Repo: "backend"},
		Fact{Kind: KindSymbol, Name: "src/catalog.CatalogService.items", File: "backend/src/catalog/catalog.service.ts", Repo: "backend",
			Relations: []Relation{{Kind: RelCalls, Target: "@example/sdk.ResourceConnector.getCatalogItems"}}},
		Fact{Kind: KindDependency, Name: "src/catalog -> @example/sdk", File: "src/catalog/catalog.service.ts", Repo: "backend",
			Relations: []Relation{{Kind: RelImports, Target: "@example/sdk"}}},
		Fact{Kind: KindModule, Name: "src", File: "src", Repo: "sdk"},
		Fact{Kind: KindSymbol, Name: "src/connectors.ResourceConnector.getCatalogItems", File: "sdk/src/connectors/resource-connector.ts", Repo: "sdk"},
	)
	s.SetTargetAliases(map[string]FactKey{
		"@example/sdk.ResourceConnector.getCatalogItems": {Repo: "sdk", Kind: KindSymbol, Name: "src/connectors.ResourceConnector.getCatalogItems", File: "sdk/src/connectors/resource-connector.ts"},
		"@example/sdk": {Repo: "sdk", Kind: KindModule, Name: "src", File: "src"},
	})
	s.BuildGraph()
	g := s.Graph()

	path := g.FindPath("src/catalog.CatalogService.items", "src/connectors.ResourceConnector.getCatalogItems", nil, 3)
	if !path.Found {
		t.Fatalf("the aliased call is not an edge: %+v", path)
	}
	if imp := g.FindPath("src/catalog", "src", []string{RelImports}, 2); !imp.Found {
		t.Fatalf("the aliased package import is not a module edge: %+v", imp)
	}
}
