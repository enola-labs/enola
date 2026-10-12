package constraints

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

// verdictForbidCycles reads the module graph with the reference and rollup
// edges admitted, because a declared set of parts is small and named and
// those edges are what Ruby has, maps each module to the part its members
// live in, contracts
// the graph to one node per part, and reports every strongly connected
// component of two or more parts. A cycle inside one part is not what the
// rule states, so self-edges are dropped with the contraction.
func (e *Explainer) verdictForbidCycles(r rule, store *facts.Store, memberFacts map[string][]facts.Fact) []facts.Insight {
	parts := append([]string{r.forbidCycles}, r.among...)
	partOfModule := map[string]string{}
	for _, part := range parts {
		for _, f := range memberFacts[part] {
			for _, dir := range moduleDirsOf(f) {
				if _, taken := partOfModule[dir]; !taken {
					partOfModule[dir] = part
				}
			}
		}
	}
	if len(partOfModule) == 0 {
		return nil
	}
	// Reference and rollup edges are admitted: between declared parts a
	// constant reference is a dependency, and on Ruby it is the only kind
	// there is. Associations stay out, as everywhere: they are data, not reach.
	moduleGraph := common.BuildModuleGraphExcluding(store, facts.CouplingAssociation)
	contracted := map[string]map[string]bool{}
	witnesses := map[string][]string{}
	edgeCount := map[string]int{}
	for _, part := range parts {
		contracted[part] = map[string]bool{}
	}
	for source, targets := range moduleGraph {
		from, ok := partOfModule[nearestPart(source, partOfModule)]
		if !ok {
			continue
		}
		for _, target := range targets {
			to, ok := partOfModule[nearestPart(target, partOfModule)]
			if !ok || to == from {
				continue
			}
			contracted[from][to] = true
			key := from + "\x00" + to
			edgeCount[key]++
			if len(witnesses[key]) < 3 {
				witnesses[key] = append(witnesses[key], source+" -> "+target)
			}
		}
	}
	adjacency := map[string][]string{}
	for from, tos := range contracted {
		for to := range tos {
			adjacency[from] = append(adjacency[from], to)
		}
		sort.Strings(adjacency[from])
	}
	var out []facts.Insight
	for _, scc := range stronglyConnected(adjacency, parts) {
		if len(scc) < 2 {
			continue
		}
		sort.Strings(scc)
		// Every ordered pair, not the sorted members read as a ring: the parts are in
		// name order, and the edges that close the circle are wherever they are.
		var evidence []facts.Evidence
		for _, a := range scc {
			for _, b := range scc {
				for _, w := range witnesses[a+"\x00"+b] {
					evidence = append(evidence, facts.Evidence{Fact: w, Detail: fmt.Sprintf("%s reaches %s", a, b)})
				}
			}
		}
		out = append(out, facts.Insight{
			Title:       r.titled(fmt.Sprintf("%s depend on each other in a circle", strings.Join(scc, ", "))),
			Description: fmt.Sprintf("The parts %s form a dependency cycle: each reaches the others through module edges the graph holds, so none can be taken out or reasoned about without the others. The rule is declared and membership is exact, so this is a decided-rule breach, not a heuristic. Because: %s", strings.Join(scc, ", "), r.because),
			Confidence:  r.confidence(),
			Evidence:    evidence,
			Actions: []string{
				cutForCycle(componentEdges(scc, edgeCount)),
				"Break one edge of the circle so the parts order again",
				"Amend the rule on its declaring page if the decision behind it changed",
			},
		})
	}
	return out
}

// moduleDirsOf names the module a member fact lives in, in both the raw and
// repo-stripped forms the module graph may use.
func moduleDirsOf(f facts.Fact) []string {
	if f.Kind == facts.KindModule {
		return []string{f.Name}
	}
	return common.ModuleDirCandidates(f)
}

// nearestPart walks up a directory path until it meets a module a part owns,
// so a file nested under a part's directory still belongs to the part.
func nearestPart(dir string, partOfModule map[string]string) string {
	for d := dir; d != "" && d != "."; d = parentDir(d) {
		if _, ok := partOfModule[d]; ok {
			return d
		}
	}
	return dir
}

func parentDir(dir string) string {
	i := strings.LastIndex(dir, "/")
	if i < 0 {
		return ""
	}
	return dir[:i]
}

// stronglyConnected is Tarjan's algorithm over the contracted part graph,
// visiting nodes in the declared order so the output is stable.
func stronglyConnected(adjacency map[string][]string, order []string) [][]string {
	index := 0
	indices := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var out [][]string
	var visit func(v string)
	visit = func(v string) {
		indices[v] = index
		low[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adjacency[v] {
			if _, seen := indices[w]; !seen {
				visit(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] && indices[w] < low[v] {
				low[v] = indices[w]
			}
		}
		if low[v] == indices[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			out = append(out, scc)
		}
	}
	for _, v := range order {
		if _, seen := indices[v]; !seen {
			visit(v)
		}
	}
	return out
}

// componentEdges lists the edges between the parts of one circle, wherever in the
// name order their ends fall.
func componentEdges(scc []string, edgeCount map[string]int) []weightedEdge {
	var out []weightedEdge
	for _, a := range scc {
		for _, b := range scc {
			if n := edgeCount[a+"\x00"+b]; a != b && n > 0 {
				out = append(out, weightedEdge{from: a, to: b, weight: n})
			}
		}
	}
	return out
}
