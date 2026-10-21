package constraints

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// verdictIndependent states that a module stays independent of its
// includers: for each member, the classes whose resolved ancestry includes
// it are its includers, and any edge from the member or from a fact its
// files carry that lands on an includer or an includer's member is one
// finding. The includers come from resolved ancestry only; without it the
// rule says so in one advisory and verdicts nothing, the same refusal the
// ancestor key makes.
func (e *Explainer) verdictIndependent(r rule, store *facts.Store, memberFacts map[string][]facts.Fact, carried map[string][]facts.Fact) []facts.Insight {
	ancestry := newResolvedAncestry(store)
	if !ancestry.any() {
		return []facts.Insight{{
			Title:       fmt.Sprintf("Constraint rule %s cannot be evaluated: %s", r.id, reasonNoResolvedAncestry),
			Description: fmt.Sprintf("independent reads who includes each member off the ancestry a resolving provider emitted, and no provider contributed any to this snapshot. The rule emitted no verdict: a mixin that looks independent because nobody resolved its includers must not read as compliance. Configure a resolving provider such as rubydex, then regenerate. Because: %s", r.because),
			Confidence:  unmeasuredPropConfidence,
			Evidence:    []facts.Evidence{{Fact: "rule: " + r.id, Detail: "declared in " + r.source}},
			Actions:     []string{"Configure a resolving provider and regenerate the snapshot"},
		}}
	}
	memberNames := map[string]bool{}
	for _, f := range memberFacts[r.independent] {
		memberNames[f.Name] = true
	}
	ownerOf := func(name string) string {
		if i := strings.IndexAny(name, "#."); i > 0 {
			return name[:i]
		}
		return name
	}
	var out []facts.Insight
	names := make([]string, 0, len(memberNames))
	for n := range memberNames {
		names = append(names, n)
	}
	sort.Strings(names)
	memberFiles := map[string]bool{}
	for _, f := range memberFacts[r.independent] {
		if f.File != "" {
			memberFiles[f.File] = true
		}
	}
	// What a module encloses is the module's: its methods carry the calls a
	// mixin makes, and they are members of no component by themselves.
	sources := append(append([]facts.Fact{}, memberFacts[r.independent]...), carried[r.independent]...)
	for _, f := range store.ByKind(facts.KindSymbol) {
		if memberNames[ownerOf(f.Name)] && !memberNames[f.Name] {
			sources = append(sources, f)
		}
	}
	for _, module := range names {
		includers := ancestry.descendantsOf(module)
		if len(includers) == 0 {
			continue
		}
		for _, f := range sources {
			ownedByModule := f.Name == module || strings.HasPrefix(f.Name, module+"#") || strings.HasPrefix(f.Name, module+".")
			carriedByModule := f.Kind == facts.KindDependency && memberFiles[f.File] && strings.Contains(f.Name, module)
			if !ownedByModule && !carriedByModule {
				continue
			}
			for _, rel := range f.Relations {
				target := ownerOf(rel.Target)
				if !includers[target] {
					continue
				}
				out = append(out, facts.Insight{
					Title:       r.titled(fmt.Sprintf("%s reaches its includer %s", module, target)),
					Description: fmt.Sprintf("%s is included by %s (resolved ancestry), and %s %s %s. A mixin that reaches the class including it is one half of that class hiding in a module, so neither can be read or reused alone. The rule is declared and membership is exact, so this is a decided-rule breach, not a heuristic. Because: %s", module, target, f.Name, rel.Kind, rel.Target, r.because),
					Confidence:  r.confidence(),
					Evidence:    []facts.Evidence{{File: f.File, Symbol: f.Name, Detail: fmt.Sprintf("%s %s", rel.Kind, rel.Target)}},
					Actions: []string{
						fmt.Sprintf("Move what %s needs from %s into the module, or pass it in", module, target),
						"Amend the rule on its declaring page if the decision behind it changed",
					},
				})
			}
		}
	}
	return out
}
