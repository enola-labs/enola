package cycles

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

// maxCycleModules is the largest SCC still reported as a discrete, actionable
// "Cyclic dependency". Larger components are reported as a coupling cluster
// instead, because advice like "introduce an interface" cannot meaningfully be
// applied to a 99-node component. Shared with the depth explainer via
// common.OversizedClusterModules.
//
// The distinction is about ACTIONABILITY and must never touch confidence. A
// cluster is every bit as certain as a cycle — it *is* a cycle, a larger one —
// and confidence in this codebase means "is this real", which is what
// min_confidence filters and the check gate threshold read. Encoding "is this
// easy to fix" in the same number made severity fall as the problem grew:
// adding an edge that pushed an 8-module cycle to 10 resolved a 1.0 finding and
// replaced it with a 0.4 advisory, so a strictly worse graph reported as an
// improvement and passed a gate it had previously failed.
const maxCycleModules = common.OversizedClusterModules

// maxClusterMembers caps how many representative members are listed as evidence
// for an oversized coupling cluster.
const maxClusterMembers = 12

// CycleExplainer detects cyclic dependencies between modules using Tarjan's SCC algorithm.
type CycleExplainer struct{}

// New creates a new CycleExplainer.
func New() *CycleExplainer {
	return &CycleExplainer{}
}

func (e *CycleExplainer) Name() string {
	return "cycles"
}

// Explain builds a dependency graph from import relations and detects cycles.
func (e *CycleExplainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	// Build adjacency list from dependency facts. ActiveRecord associations are
	// excluded: has_many/belongs_to pairs are inherently bidirectional domain
	// relationships, not load-order dependencies, and would otherwise manufacture
	// spurious two-module cycles between associated models.
	//
	// Constant references are excluded for the same reason one step further out.
	// An autoloaded constant reference is a COUPLING claim, not a LOAD-ORDER
	// claim: under Zeitwerk `Billing::Invoice.call` inside app/models resolves
	// when the line runs, so the two directories can reference each other freely
	// and nothing about the boot sequence is at stake. Grading them here made the
	// finding say a thing that is not true of the codebase — and made it useless
	// besides, since every app/* directory calls every other by design, so a flat
	// Rails app reports one blob spanning nearly its whole tree and no cycle a
	// team could act on. What survives the exclusion is the load-order claim
	// proper: inheritance and mixins, which the class body evaluates at
	// definition time, `require`, which is a load instruction, and a packwerk
	// dependency, which the app declared itself. Nothing is deleted by this: the
	// reference edges stay in the fact store, the depth explainer still builds
	// the whole graph from them, and anything asking what couples to what still
	// gets the same answer. What narrows is only the graph the gated finding is
	// derived from.
	// Rolled-up call edges are excluded for the same reason associations are:
	// they are real coupling and a poor cycle oracle. A directory pair joined
	// because one method somewhere called another is not a load-order defect,
	// and admitting them merged this estate's small, nameable cycles into one
	// 508-module blob (46 cycles and 3 clusters became 40 and 4). The edges
	// stay in the store for the readings that want coupling rather than
	// cycles: the metrics suite, impact, and the layer rules.
	graph := common.BuildModuleGraphExcluding(store, facts.CouplingAssociation, facts.CouplingReference, facts.CouplingSymbolRollup)

	// Which build unit each module compiles into, where the language models one.
	// See facts.CompilationUnitProps: a cycle confined to a single unit is not a
	// build-order defect, and saying it "can cause initialization issues" is untrue.
	unitOf := make(map[string]string)
	for _, m := range store.ByKind(facts.KindModule) {
		if u := facts.CompilationUnit(m); u != "" {
			unitOf[m.Name] = u
		}
	}

	// Run Tarjan's SCC
	sccs := tarjanSCC(graph)

	// Filter to cycles (SCCs with size > 1)
	var insights []facts.Insight
	for _, scc := range sccs {
		if len(scc) <= 1 {
			continue
		}

		if len(scc) > maxCycleModules {
			insights = append(insights, coupledClusterInsight(scc))
			continue
		}

		cyclePath := describeCycle(scc, graph)
		evidence := make([]facts.Evidence, 0, len(scc))
		for _, mod := range scc {
			evidence = append(evidence, facts.Evidence{
				Fact:   mod,
				Detail: fmt.Sprintf("module %q is part of the cycle", mod),
			})
		}

		description := fmt.Sprintf("%s. This can cause initialization issues, make refactoring harder, and indicates tight coupling.", cyclePath)
		actions := []string{
			"Introduce an interface to break the cycle",
			"Extract shared types to a separate package",
			"Consider merging tightly coupled modules",
		}
		if unit, ok := sharedUnit(scc, unitOf); ok {
			description = fmt.Sprintf(
				"%s. All of them compile into %q, "+
					"so this is NOT a build-order problem — mutual references between namespaces or "+
					"submodules inside one assembly/crate are legal and ordinary, and the build system "+
					"forbids cycles between units anyway. Read it as a coupling signal: the cycle means "+
					"these directories cannot be understood, moved or split apart independently.",
				cyclePath, unit,
			)
			actions = []string{
				"Treat as a coupling signal rather than a build defect",
				"Extract the shared types the cycle turns on into their own module",
				"Consider merging directories that cannot be separated anyway",
			}
		}

		insights = append(insights, facts.Insight{
			// Title prefix is parsed by pkg/explain (Code health section); keep stable.
			Title: fmt.Sprintf("Cyclic dependency detected (%d modules)", len(scc)),
			// Confidence stays 1.0 either way: the cycle is a structural fact, and
			// what an intra-unit cycle changes is what it MEANS, not whether it is
			// there. Lowering it would also move the finding under any
			// min_confidence filter and the check gate, hiding a real edge.
			Description: description,
			Confidence:  1.0, // Deterministic
			Evidence:    evidence,
			Actions:     actions,
		})
	}

	return insights, nil
}

// describeCycle states a cyclic component as a sentence whose arrows are all edges
// of the graph.
//
// The component's members come back sorted, and joining them with arrows printed a
// path through modules that do not import each other: whoever followed it to break
// the cycle started from an edge that is not in the code. A component is a set of
// modules that all reach each other, and it usually holds several cycles, so what
// is printed is one of them, walked along real edges, and the members beside it
// when that cycle does not pass through all of them.
func describeCycle(scc []string, graph map[string][]string) string {
	cycle := cycleThrough(scc, graph)
	path := strings.Join(cycle, " -> ")
	if len(cycle) == len(scc)+1 {
		return "The following modules form a dependency cycle: " + path
	}
	return fmt.Sprintf("The %d modules %s depend on each other through more than one cycle. One of them: %s",
		len(scc), strings.Join(scc, ", "), path)
}

// cycleThrough returns a shortest cycle through the component's first member, as the
// modules in import order with the first repeated at the end. Neighbours are taken
// in sorted order, so the same graph always prints the same cycle.
func cycleThrough(scc []string, graph map[string][]string) []string {
	start := scc[0]
	member := make(map[string]bool, len(scc))
	for _, m := range scc {
		member[m] = true
	}
	next := func(mod string) []string {
		var out []string
		for _, n := range graph[mod] {
			if member[n] && n != mod {
				out = append(out, n)
			}
		}
		sort.Strings(out)
		return out
	}
	// Breadth-first from start: the first module found that imports start closes a
	// shortest cycle.
	parent := map[string]string{start: ""}
	queue := []string{start}
	for len(queue) > 0 {
		mod := queue[0]
		queue = queue[1:]
		for _, n := range next(mod) {
			if n == start {
				// Back from mod to start, then reversed: start ... mod start.
				cycle := []string{start}
				for at := mod; at != ""; at = parent[at] {
					cycle = append(cycle, at)
				}
				for i, j := 0, len(cycle)-1; i < j; i, j = i+1, j-1 {
					cycle[i], cycle[j] = cycle[j], cycle[i]
				}
				return cycle
			}
			if _, seen := parent[n]; !seen {
				parent[n] = mod
				queue = append(queue, n)
			}
		}
	}
	// Unreachable for a strongly connected component of two or more modules.
	return append(append([]string{}, scc...), start)
}

// sharedUnit reports the build unit every module in the cycle compiles into, when
// they all share one.
//
// Every module must be accounted for. A cycle that reaches even one module with no
// known unit — a language that models none, or a directory the extractor could not
// attribute — may well span separately built things, and that is exactly the case
// the softened wording would be wrong about.
func sharedUnit(scc []string, unitOf map[string]string) (string, bool) {
	unit := ""
	for _, mod := range scc {
		u, ok := unitOf[mod]
		if !ok || u == "" {
			return "", false
		}
		if unit == "" {
			unit = u
		} else if u != unit {
			return "", false
		}
	}
	return unit, unit != ""
}

// coupledClusterInsight reports an oversized SCC as a soft coupling signal rather
// than a discrete cycle. The title deliberately does NOT start with "Cyclic
// dependency" so pkg/explain does not fold it into the cycle count.
func coupledClusterInsight(scc []string) facts.Insight {
	members := scc
	if len(members) > maxClusterMembers {
		members = members[:maxClusterMembers]
	}
	evidence := make([]facts.Evidence, 0, len(members))
	for _, mod := range members {
		evidence = append(evidence, facts.Evidence{
			Fact:   mod,
			Detail: fmt.Sprintf("module %q is part of the cluster", mod),
		})
	}
	return facts.Insight{
		Title: fmt.Sprintf("Highly coupled module cluster (%d modules)", len(scc)),
		Description: fmt.Sprintf(
			"%d modules reference each other mutually. In an autoloaded codebase "+
				"(e.g. Rails) this is expected — constant references between directories "+
				"resolve lazily, so this is not a load-order cycle. Treat it as an overall "+
				"coupling-density signal, not a defect to break.",
			len(scc),
		),
		// Deterministic and certain, exactly like the cycle it grew out of.
		// Lowering it here is what let a growing cycle drop under the gate.
		Confidence: 1.0,
		Evidence:   evidence,
		Actions: []string{
			"Look for a few high-traffic modules whose extraction would thin the cluster",
			"Prefer narrowing individual module responsibilities over a single big refactor",
		},
	}
}

// tarjanSCC computes strongly connected components of the module graph. It
// delegates to common.StronglyConnectedComponents, whose output is deterministic
// (sorted components with sorted members) — so the emitted cycle path, evidence
// order, and multi-cycle insight order no longer depend on Go's randomized map
// iteration.
func tarjanSCC(graph map[string][]string) [][]string {
	return common.StronglyConnectedComponents(graph)
}
