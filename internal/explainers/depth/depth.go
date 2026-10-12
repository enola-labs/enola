// Package depth flags modules that sit too deep in the import chain — i.e.
// whose longest transitive dependency path is unusually long. Deep modules are
// slow to understand and load-bearing: a change at the bottom of a long chain
// can force rebuilds/retests all the way up.
package depth

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

const (
	// minDepth is the longest-chain length at or above which a module is
	// reported. A chain of N means N modules are imported in sequence below it.
	minDepth = 5
	// maxInsights caps how many deep modules are reported, deepest first.
	maxInsights = 10
)

// DepthExplainer detects modules deep in the dependency chain.
type DepthExplainer struct{}

// New creates a new DepthExplainer.
func New() *DepthExplainer {
	return &DepthExplainer{}
}

func (e *DepthExplainer) Name() string {
	return "dependency-depth"
}

// Explain builds the module import graph and computes each module's longest
// downstream dependency chain (cycle-safe), reporting the deepest ones.
//
// Cycles are handled by collapsing each strongly-connected component to a single
// super-node (the condensation), which is a DAG, then taking the longest path by
// module count over that DAG. A small component contributes its full size to any
// chain passing through it (a genuine tangle deepens the chain). But an *oversized*
// component (> common.OversizedClusterModules) is an autoload coupling cluster, not
// deep layering — in Ruby/Rails mutual constant references collapse most of the app
// into one giant SCC. Such a cluster is therefore (a) weighted 0 (componentWeight)
// and (b) made a sink in the condensation (its outgoing edges are dropped), so a
// chain earns depth only from genuine, distinct-module layering above/outside the
// cluster and never by threading through it. Otherwise every chain reaching the
// cluster would report an inflated depth that merely restates the coupling the
// cycles explainer already reports as a "Highly coupled module cluster". A module's
// reported depth is its component's depth; one insight is emitted per component
// (keyed by its smallest member), so a small cycle yields a single finding rather
// than one per entangled module.
func (e *DepthExplainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	graph := common.BuildModuleGraph(store)
	if len(graph) == 0 {
		return nil, nil
	}

	// Condense the module graph into its SCC DAG. Components come back sorted
	// (sorted members, ordered by smallest member) so the whole computation is
	// deterministic regardless of Go's randomized map iteration.
	sccs := common.StronglyConnectedComponents(graph)
	sccOf := make(map[string]int, len(graph))
	for i, scc := range sccs {
		for _, m := range scc {
			sccOf[m] = i
		}
	}

	// Build the condensed adjacency (successor component indices), deduped and
	// sorted. Self-edges and intra-component edges are dropped — the condensation
	// is acyclic by construction. An oversized coupling cluster is made a sink (its
	// outgoing edges are dropped) so a chain cannot earn depth by threading through
	// it — see the Explain doc comment.
	succ := make([][]int, len(sccs))
	seen := make([]map[int]bool, len(sccs))
	for i := range seen {
		seen[i] = map[int]bool{}
	}
	for mod, neighbors := range graph {
		si := sccOf[mod]
		if len(sccs[si]) > common.OversizedClusterModules {
			continue // oversized cluster is a depth sink
		}
		for _, n := range neighbors {
			sj, ok := sccOf[n]
			if !ok || sj == si {
				continue
			}
			if !seen[si][sj] {
				seen[si][sj] = true
				succ[si] = append(succ[si], sj)
			}
		}
	}
	for i := range succ {
		sort.Ints(succ[i])
	}

	// Longest path (by module count) over the DAG, memoized. Each component's
	// depth is its own size plus the deepest successor chain; bestSucc lets us
	// reconstruct a concrete chain of distinct modules for the evidence.
	depth := make([]int, len(sccs))
	bestSucc := make([]int, len(sccs))
	computed := make([]bool, len(sccs))
	for i := range bestSucc {
		bestSucc[i] = -1
	}
	var compute func(i int) int
	compute = func(i int) int {
		if computed[i] {
			return depth[i]
		}
		computed[i] = true // condensation is a DAG, so no cycle guard is needed
		best, bi := 0, -1
		for _, j := range succ[i] {
			if d := compute(j); d > best {
				best, bi = d, j
			}
		}
		depth[i] = componentWeight(sccs[i]) + best
		bestSucc[i] = bi
		return depth[i]
	}
	for i := range sccs {
		compute(i)
	}

	type result struct {
		module string
		depth  int
		chain  []string
		path   string
	}
	var results []result
	for i, scc := range sccs {
		if depth[i] < minDepth {
			continue
		}
		results = append(results, result{
			module: scc[0], depth: depth[i],
			chain: chainFor(i, sccs, bestSucc), path: chainPath(i, sccs, bestSucc),
		})
	}

	// Deepest first; break ties by module name for determinism.
	sort.Slice(results, func(i, j int) bool {
		if results[i].depth != results[j].depth {
			return results[i].depth > results[j].depth
		}
		return results[i].module < results[j].module
	})

	var insights []facts.Insight
	for i, r := range results {
		if i >= maxInsights {
			break
		}
		evidence := make([]facts.Evidence, 0, len(r.chain))
		for _, m := range r.chain {
			evidence = append(evidence, facts.Evidence{Fact: m})
		}

		insights = append(insights, facts.Insight{
			// Title format is parsed by pkg/explain (Code health section); keep stable.
			Title: fmt.Sprintf("Deep dependency chain: %s (depth %d)", r.module, r.depth),
			Description: fmt.Sprintf(
				"Module %q has a longest dependency chain of %d modules: %s. "+
					"%sDeep chains slow comprehension and widen rebuild/retest impact when a "+
					"module near the bottom changes.",
				r.module, r.depth, r.path, groupNote(r.path),
			),
			Confidence: 0.7,
			Evidence:   evidence,
			Actions: []string{
				"Flatten the chain by depending on shared abstractions instead of deep transitive modules",
				"Check whether intermediate modules are pass-through layers that can be removed",
				"Introduce interfaces to decouple the deepest modules from their consumers",
			},
		})
	}

	return insights, nil
}

// chainPath renders the chain chainFor reconstructs so that every arrow is a
// dependency the graph holds. The chain runs over components, and an edge between two
// components is real; an order among the members of one component is not, because
// they reach each other in a cycle and no single path through them is the chain. So
// a component of several modules is printed as one braced group, and an arrow into
// or out of it means some member of the group has that dependency.
func chainPath(i int, sccs [][]string, bestSucc []int) string {
	var hops []string
	for i != -1 {
		if len(sccs[i]) > common.OversizedClusterModules {
			break // weight-0 sink; not part of the reported chain
		}
		if len(sccs[i]) == 1 {
			hops = append(hops, sccs[i][0])
		} else {
			hops = append(hops, "{"+strings.Join(sccs[i], ", ")+"}")
		}
		i = bestSucc[i]
	}
	return strings.Join(hops, " -> ")
}

// groupNote explains the braces, on the chains that have them.
func groupNote(path string) string {
	if !strings.Contains(path, "{") {
		return ""
	}
	return "Modules in braces depend on each other in a cycle and count together. "
}

// componentWeight is how much a strongly-connected component contributes to a
// dependency-chain's depth: its full member count for a small tangle, but 0 for an
// oversized autoload cluster. Combined with making the cluster a sink (its outgoing
// edges are dropped), this means a chain earns depth only from genuine layering
// above/outside the cluster, never from threading through it — see the Explain doc
// comment.
func componentWeight(scc []string) int {
	if len(scc) > common.OversizedClusterModules {
		return 0
	}
	return len(scc)
}

// chainFor reconstructs the deepest chain of distinct modules starting at
// component i: all of each small component's (sorted) members, then its best
// successor, and so on down the DAG. An oversized cluster (weight 0, and a sink so
// it is always terminal) is omitted, keeping the reported chain length equal to the
// reported depth instead of dumping ~100 cluster members.
func chainFor(i int, sccs [][]string, bestSucc []int) []string {
	var out []string
	for i != -1 {
		if len(sccs[i]) > common.OversizedClusterModules {
			break // weight-0 sink; not part of the reported chain
		}
		out = append(out, sccs[i]...)
		i = bestSucc[i]
	}
	return out
}
