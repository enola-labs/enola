package depth

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

// makeStore builds module facts and dependency facts encoding module -> module
// imports. Module names must contain a slash and no dot so they are treated as
// internal by the shared graph builder.
func makeStore(modules []string, deps map[string][]string) *facts.Store {
	s := facts.NewStore()
	for _, m := range modules {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	for src, targets := range deps {
		for _, tgt := range targets {
			s.Add(facts.Fact{
				Kind:      facts.KindDependency,
				File:      src + "/file.go",
				Relations: []facts.Relation{{Kind: facts.RelImports, Target: tgt}},
			})
		}
	}
	return s
}

// chain builds modules a/m0..a/m{n-1} linked m0->m1->...->m{n-1}.
func chain(n int) ([]string, map[string][]string) {
	mods := make([]string, n)
	deps := map[string][]string{}
	for i := 0; i < n; i++ {
		mods[i] = fmt.Sprintf("a/m%d", i)
	}
	for i := 0; i < n-1; i++ {
		deps[mods[i]] = []string{mods[i+1]}
	}
	return mods, deps
}

func TestExplain_ShallowGraph(t *testing.T) {
	mods, deps := chain(4) // longest chain = 4 < minDepth
	insights, err := New().Explain(context.Background(), makeStore(mods, deps))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("expected 0 insights for shallow graph, got %d", len(insights))
	}
}

func TestExplain_DeepChain(t *testing.T) {
	mods, deps := chain(5) // a/m0 has a chain of length 5
	insights, err := New().Explain(context.Background(), makeStore(mods, deps))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 deep-chain insight, got %d: %+v", len(insights), insights)
	}
	in := insights[0]
	if !strings.Contains(in.Title, "a/m0") {
		t.Errorf("deepest module a/m0 should be reported, got title %q", in.Title)
	}
	if len(in.Evidence) != 5 {
		t.Errorf("evidence should list the 5-module chain, got %d", len(in.Evidence))
	}
}

func TestExplain_CycleTerminates(t *testing.T) {
	// a -> b -> c -> a is a cycle; must not infinite-loop, and depth stays small.
	store := makeStore(
		[]string{"a/x", "a/y", "a/z"},
		map[string][]string{
			"a/x": {"a/y"},
			"a/y": {"a/z"},
			"a/z": {"a/x"},
		},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	// Longest cycle-safe chain is 3 (< minDepth), so no insights, and no hang.
	if len(insights) != 0 {
		t.Errorf("expected 0 insights for short cycle, got %d", len(insights))
	}
}

// cyclicGraph: a 3-module cycle x->y->z->x with a tail z->t1->t2->t3. The
// longest simple chain from x visits 6 distinct modules. A correct, cycle-safe
// longest-path must not double-count the cycle entry.
func cyclicGraph() ([]string, map[string][]string) {
	return []string{"a/x", "a/y", "a/z", "a/t1", "a/t2", "a/t3"},
		map[string][]string{
			"a/x":  {"a/y"},
			"a/y":  {"a/z"},
			"a/z":  {"a/x", "a/t1"},
			"a/t1": {"a/t2"},
			"a/t2": {"a/t3"},
		}
}

func TestExplain_CycleDoesNotInflateDepth(t *testing.T) {
	mods, deps := cyclicGraph()
	insights, err := New().Explain(context.Background(), makeStore(mods, deps))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) == 0 {
		t.Fatal("expected at least one deep-chain insight")
	}
	deepest := insights[0]
	// The deepest chain is x->y->z->t1->t2->t3: 6 distinct modules, none repeated.
	if got := len(deepest.Evidence); got != 6 {
		t.Errorf("deepest chain should span 6 distinct modules, got %d: %q", got, deepest.Title)
	}
	seen := map[string]bool{}
	for _, ev := range deepest.Evidence {
		if seen[ev.Fact] {
			t.Errorf("chain double-counts module %q (cycle inflation): %q", ev.Fact, deepest.Title)
		}
		seen[ev.Fact] = true
	}
}

// TestExplain_CycleUnderCountRegression guards BUG-3: the old memoized
// longestChain cached a node's depth even when it was truncated by the cycle
// back-edge cut, then reused that truncated value globally. In this graph
// (m/a<->m/b cycle, tail m/c->m/d->m/e, feeder m/r->m/b) computing m/a first
// memoized m/b at depth 1, so m/r's true depth-6 chain (r->b->a->c->d->e) was
// never reported. The SCC-condensation rewrite reports it correctly.
func TestExplain_CycleUnderCountRegression(t *testing.T) {
	store := makeStore(
		[]string{"m/a", "m/b", "m/c", "m/d", "m/e", "m/r"},
		map[string][]string{
			"m/a": {"m/b", "m/c"},
			"m/b": {"m/a"},
			"m/c": {"m/d"},
			"m/d": {"m/e"},
			"m/r": {"m/b"},
		},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	byModule := map[string]facts.Insight{}
	for _, in := range insights {
		for _, m := range []string{"m/r", "m/a"} {
			if strings.Contains(in.Title, m+" (") {
				byModule[m] = in
			}
		}
	}

	rIn, ok := byModule["m/r"]
	if !ok {
		t.Fatalf("m/r (true depth 6) was not reported — under-count regression; got %+v", titles(insights))
	}
	if !strings.Contains(rIn.Title, "depth 6") {
		t.Errorf("m/r should be reported at depth 6, got title %q", rIn.Title)
	}
	if len(rIn.Evidence) != 6 {
		t.Errorf("m/r chain should span 6 distinct modules, got %d", len(rIn.Evidence))
	}
	seen := map[string]bool{}
	for _, ev := range rIn.Evidence {
		if seen[ev.Fact] {
			t.Errorf("chain double-counts %q", ev.Fact)
		}
		seen[ev.Fact] = true
	}
	// The cycle members m/a and m/b share a depth of 5 (>= minDepth) and must be
	// reported too — as a single component insight keyed on the smallest member.
	if _, ok := byModule["m/a"]; !ok {
		t.Errorf("cycle component (m/a) at depth 5 should be reported; got %v", titles(insights))
	}
}

func titles(ins []facts.Insight) []string {
	out := make([]string, len(ins))
	for i, in := range ins {
		out[i] = in.Title
	}
	return out
}

func TestExplain_CapsAtMaxInsights(t *testing.T) {
	// A single long chain: a/m0..a/m15. Modules a/m0..a/m11 have depth >= minDepth
	// (5), i.e. 12 qualifying modules — more than the cap of 10.
	mods, deps := chain(16)
	insights, err := New().Explain(context.Background(), makeStore(mods, deps))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != maxInsights {
		t.Fatalf("expected output capped at %d, got %d", maxInsights, len(insights))
	}
	if !strings.Contains(insights[0].Title, "a/m0 (") || !strings.Contains(insights[0].Title, "depth 16") {
		t.Errorf("deepest module a/m0 (depth 16) should rank first, got %q", insights[0].Title)
	}
}

func TestExplain_SelfImportDoesNotAddDepth(t *testing.T) {
	// a/m0 imports itself and a/m1; the self-import must not inflate its depth.
	mods, deps := chain(5)
	deps["a/m0"] = append(deps["a/m0"], "a/m0")
	insights, err := New().Explain(context.Background(), makeStore(mods, deps))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d", len(insights))
	}
	if !strings.Contains(insights[0].Title, "depth 5") {
		t.Errorf("self-import should not add depth; want depth 5, got %q", insights[0].Title)
	}
}

// oversizedRing builds one big SCC of OversizedClusterModules+3 modules named
// prefix+NN, returning the module list, the dep map, and the ring members.
func oversizedRing(prefix string) ([]string, map[string][]string, []string) {
	n := common.OversizedClusterModules + 3
	ring := make([]string, n)
	deps := map[string][]string{}
	for i := 0; i < n; i++ {
		ring[i] = fmt.Sprintf("%s%02d", prefix, i)
	}
	for i := 0; i < n; i++ {
		deps[ring[i]] = []string{ring[(i+1)%n]}
	}
	return append([]string(nil), ring...), deps, ring
}

// TestExplain_OversizedClusterNotDeep: a large autoload cluster with a short tail
// hanging off it produces no deep-chain finding — the cluster is a weight-0 sink, so
// it neither counts toward depth nor lets a chain thread through it.
func TestExplain_OversizedClusterNotDeep(t *testing.T) {
	mods, deps, ring := oversizedRing("c/m")
	// A short 2-module tail hanging off the cluster: c/m00 -> t/t0 -> t/t1.
	mods = append(mods, "t/t0", "t/t1")
	deps[ring[0]] = append(deps[ring[0]], "t/t0")
	deps["t/t0"] = []string{"t/t1"}

	insights, err := New().Explain(context.Background(), makeStore(mods, deps))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Fatalf("oversized cluster should not produce a deep-chain finding, got %d: %v", len(insights), titles(insights))
	}
}

// TestExplain_ClusterIsDepthSink: genuine layering ABOVE a cluster still fires (the
// cluster contributes nothing but the real layers count), while shallow layering
// into a cluster does not — proving depth is earned only outside the cluster.
func TestExplain_ClusterIsDepthSink(t *testing.T) {
	mods, deps, ring := oversizedRing("k/m")
	// 5 genuine layers a0->a1->a2->a3->a4, then a4 imports into the cluster.
	for i := 0; i < 5; i++ {
		mods = append(mods, fmt.Sprintf("a/l%d", i))
	}
	for i := 0; i < 4; i++ {
		deps[fmt.Sprintf("a/l%d", i)] = []string{fmt.Sprintf("a/l%d", i+1)}
	}
	deps["a/l4"] = []string{ring[0]}
	// 3 shallow layers b0->b1->b2, then b2 imports into the cluster.
	for i := 0; i < 3; i++ {
		mods = append(mods, fmt.Sprintf("b/l%d", i))
	}
	deps["b/l0"] = []string{"b/l1"}
	deps["b/l1"] = []string{"b/l2"}
	deps["b/l2"] = []string{ring[0]}

	insights, err := New().Explain(context.Background(), makeStore(mods, deps))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	byMod := map[string]facts.Insight{}
	for _, in := range insights {
		for _, ev := range in.Evidence {
			// first evidence fact is the chain head
			byMod[ev.Fact] = in
			break
		}
	}
	// a/l0 has 5 real layers into the cluster -> reported at depth 5, and the chain
	// must NOT include any cluster member.
	aIn, ok := byMod["a/l0"]
	if !ok {
		t.Fatalf("5-layer chain above the cluster should be reported; got %v", titles(insights))
	}
	if !strings.Contains(aIn.Title, "depth 5") {
		t.Errorf("a/l0 should be depth 5 (cluster contributes 0), got %q", aIn.Title)
	}
	for _, ev := range aIn.Evidence {
		if strings.HasPrefix(ev.Fact, "k/m") {
			t.Errorf("cluster member %q must not appear in the chain", ev.Fact)
		}
	}
	// b/l0 has only 3 layers into the cluster -> below minDepth -> not reported.
	if _, ok := byMod["b/l0"]; ok {
		t.Errorf("3-layer chain into the cluster should be suppressed; got %v", titles(insights))
	}
}

func TestExplain_EmptyGraph(t *testing.T) {
	insights, err := New().Explain(context.Background(), facts.NewStore())
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("empty store should yield no insights, got %d", len(insights))
	}
}

// TestExplain_Deterministic guards against the regression where cycle handling
// depended on Go's randomized map iteration order. Each Explain call re-ranges
// the graph map, so repeated calls exercise different iteration orders; the
// rendered titles must be identical every time.
func TestExplain_Deterministic(t *testing.T) {
	mods, deps := cyclicGraph()
	store := makeStore(mods, deps)

	titles := func() []string {
		insights, err := New().Explain(context.Background(), store)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		out := make([]string, len(insights))
		for i, in := range insights {
			out[i] = in.Title
		}
		return out
	}

	want := strings.Join(titles(), "\n")
	for i := 0; i < 50; i++ {
		if got := strings.Join(titles(), "\n"); got != want {
			t.Fatalf("non-deterministic output on iteration %d:\nwant:\n%s\ngot:\n%s", i, want, got)
		}
	}
}

// TestExplain_ChainArrowsAreDependencies: routes imports only the controllers, and
// the five modules below it are one cycle. Their names in order are not a path, so
// they are printed as a group and the depth still counts each of them.
func TestExplain_ChainArrowsAreDependencies(t *testing.T) {
	deps := map[string][]string{
		"app/routes":           {"app/Http/Controllers"},
		"app/Http/Controllers": {"app/Services"},
		"app/Services":         {"app/Repositories", "app/Gateways"},
		"app/Repositories":     {"app/Http/Controllers"},
		"app/Gateways":         {"app/Models"},
		"app/Models":           {"app/Services"},
	}
	modules := []string{"app/routes", "app/Gateways", "app/Http/Controllers", "app/Models", "app/Repositories", "app/Services"}
	insights, err := New().Explain(context.Background(), makeStore(modules, deps))
	if err != nil {
		t.Fatal(err)
	}
	var got *facts.Insight
	for i := range insights {
		if insights[i].Title == "Deep dependency chain: app/routes (depth 6)" {
			got = &insights[i]
		}
	}
	if got == nil {
		t.Fatalf("no finding for app/routes at depth 6: %v", titles(insights))
	}
	const want = "app/routes -> {app/Gateways, app/Http/Controllers, app/Models, app/Repositories, app/Services}. " +
		"Modules in braces depend on each other in a cycle and count together."
	if !strings.Contains(got.Description, want) {
		t.Errorf("description = %q, want it to contain %q", got.Description, want)
	}
	if strings.Contains(got.Description, "app/routes -> app/Gateways") {
		t.Errorf("description prints app/routes -> app/Gateways, and no such import exists: %s", got.Description)
	}
	if len(got.Evidence) != 6 {
		t.Errorf("evidence rows = %d, want every module of the chain", len(got.Evidence))
	}
}

// TestExplain_AcyclicChainHasNoGroups: a plain chain reads as it did.
func TestExplain_AcyclicChainHasNoGroups(t *testing.T) {
	mods, deps := chain(6)
	insights, err := New().Explain(context.Background(), makeStore(mods, deps))
	if err != nil || len(insights) == 0 {
		t.Fatalf("insights = %v, err = %v", insights, err)
	}
	const want = "a/m0 -> a/m1 -> a/m2 -> a/m3 -> a/m4 -> a/m5. Deep chains"
	if !strings.Contains(insights[0].Description, want) {
		t.Errorf("description = %q, want it to contain %q", insights[0].Description, want)
	}
}

// TestExplain_EveryPrintedArrowIsADependency runs random graphs through the explainer
// and checks each arrow of each chain: between two modules it is an import, and into
// or out of a braced group some member of the group has it.
func TestExplain_EveryPrintedArrowIsADependency(t *testing.T) {
	seed := uint32(11)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>16) % n
	}
	members := func(hop string) []string {
		return strings.Split(strings.Trim(hop, "{}"), ", ")
	}
	checked := 0
	for round := 0; round < 200; round++ {
		size := 6 + next(8)
		modules := make([]string, size)
		for i := range modules {
			modules[i] = fmt.Sprintf("p/m%02d", i)
		}
		edge := map[string]bool{}
		deps := map[string][]string{}
		for e := 0; e < size+next(size); e++ {
			from, to := modules[next(size)], modules[next(size)]
			if from != to && !edge[from+">"+to] {
				edge[from+">"+to] = true
				deps[from] = append(deps[from], to)
			}
		}
		insights, err := New().Explain(context.Background(), makeStore(modules, deps))
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range insights {
			const marker = " modules: "
			path := in.Description[strings.Index(in.Description, marker)+len(marker):]
			path = path[:strings.Index(path, ". ")]
			hops := strings.Split(path, " -> ")
			for i := 0; i+1 < len(hops); i++ {
				found := false
				for _, from := range members(hops[i]) {
					for _, to := range members(hops[i+1]) {
						found = found || edge[from+">"+to]
					}
				}
				if !found {
					t.Fatalf("%q prints %s -> %s, and no module of the first imports one of the second", path, hops[i], hops[i+1])
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no chain was long enough to report, so nothing was checked")
	}
}
