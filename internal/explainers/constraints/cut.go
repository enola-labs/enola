package constraints

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

const noCut = "No cut the graph can see: the far part declares no public surface and the offender's other edges name no part to move it to"

// cutForEdge proposes the smallest change that satisfies an edge breach from
// a member of the near component to a far target: the far part's public
// member with the same bare name, else its public surface by path, else the
// part the offender's remaining edges mostly reach.
func cutForEdge(resolve *resolver, far string, from facts.Fact, target string) string {
	if far == "" {
		return noCut
	}
	c, ok := resolve.components[far]
	if !ok {
		return noCut
	}
	bare := memberShortName(target)
	if len(c.public) > 0 {
		for _, m := range resolve.memberFacts[far] {
			if matchConstraintFile(m, c.public) && memberShortName(m.Name) == bare && m.Name != target {
				return fmt.Sprintf("Reach %s through %s, the public member of %s with the same name", bare, m.Name, far)
			}
		}
		return fmt.Sprintf("Route the call through the public surface of %s (%s) instead of %s", far, strings.Join(c.public, ", "), target)
	}
	// A fact already in the component its edges reach most has nowhere to move:
	// the breach is the edge, not where the fact sits.
	if home := mostReachedComponent(resolve, from, far); home != "" && !resolve.members[home][from.Name] {
		return fmt.Sprintf("Move %s into %s, where the rest of its edges already go", from.Name, home)
	}
	return noCut
}

// mostReachedComponent names the component the fact's other edges land in
// most often, excluding the far component of the breach.
func mostReachedComponent(resolve *resolver, from facts.Fact, exclude string) string {
	counts := map[string]int{}
	for _, rel := range from.Relations {
		for name, members := range resolve.members {
			if name == exclude || !members[rel.Target] {
				continue
			}
			counts[name]++
		}
	}
	best, bestCount := "", 0
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if counts[name] > bestCount {
			best, bestCount = name, counts[name]
		}
	}
	return best
}

// cutForCycle names the lightest edge of the circle by symbol count.
func cutForCycle(edges []weightedEdge) string {
	if len(edges) == 0 {
		return noCut
	}
	lightest := edges[0]
	for _, e := range edges[1:] {
		if e.weight < lightest.weight || (e.weight == lightest.weight && e.from+e.to < lightest.from+lightest.to) {
			lightest = e
		}
	}
	return fmt.Sprintf("Cut the circle at %s -> %s, its lightest edge (%d symbol edges)", lightest.from, lightest.to, lightest.weight)
}

type weightedEdge struct {
	from, to string
	weight   int
}

// cutForStorage names the owning part's public member that reaches the
// same table, or the surface to go through.
func cutForStorage(r rule, resolve *resolver, model, table string) string {
	for name, members := range resolve.members {
		if name == r.storageStaysHome || !members[model] {
			continue
		}
		c := resolve.components[name]
		if len(c.public) == 0 {
			return fmt.Sprintf("Ask %s, which owns table %s, for an operation on its public surface instead of reaching %s", name, table, model)
		}
		for _, m := range resolve.memberFacts[name] {
			if !matchConstraintFile(m, c.public) {
				continue
			}
			for _, rel := range m.Relations {
				if ownerName(rel.Target) == model {
					return fmt.Sprintf("Reach table %s through %s, the public member of %s that already uses %s", table, m.Name, name, model)
				}
			}
		}
		return fmt.Sprintf("Route the access to table %s through the public surface of %s (%s)", table, name, strings.Join(c.public, ", "))
	}
	return noCut
}
