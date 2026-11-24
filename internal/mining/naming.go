package mining

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/intent"
)

const minPatternLiteral = 3

func (s *scope) mineNaming() []Candidate {
	var out []Candidate
	for _, kind := range minedKindOrder {
		ms := s.members[kind]
		if len(ms) == 0 {
			continue
		}
		clusters := map[string][]int{}
		for i, m := range ms {
			for _, cluster := range clusterPrefixes(m.file) {
				clusters[cluster] = append(clusters[cluster], i)
			}
		}
		kept := map[string][]string{}
		for _, cluster := range sortedKeys(clusters) {
			idx := clusters[cluster]
			pattern, matched, passedOverTautology := bestNamePattern(cluster, ms, idx, s.cfg.MinConfidence, s.cfg.IncludeTautologies, s.roots)
			if passedOverTautology {
				s.suppressed[FamilyNaming].Tautological++
			}
			if pattern == "" {
				continue
			}
			if coveredByAncestor(kept, cluster, pattern) {
				continue
			}
			confidence := ratio(matched, len(idx))
			if confidence <= globalMatchRate(ms, pattern) {
				continue
			}
			exceptions := namingExceptions(ms, idx, pattern)
			if !s.admit(FamilyNaming, len(idx), len(exceptions)) {
				continue
			}
			if !s.cfg.IncludeTautologies && tautologicalNaming(cluster, pattern, len(idx), s.roots) {
				s.suppressed[FamilyNaming].Tautological++
				continue
			}
			kept[pattern] = append(kept[pattern], cluster)
			component := s.clusterComponent(cluster, kind)
			statement := s.statement(fmt.Sprintf("%d/%d %s facts under %s/ are named %s", matched, len(idx), kind, cluster, pattern))
			rule := intent.ConstraintRule{
				ID:          slug("mined", cluster, kind, "named", pattern),
				RequireName: component.Name,
				Pattern:     pattern,
				Mode:        "advisory",
				Because:     because(statement, exceptions),
			}
			out = append(out, Candidate{
				Family:      FamilyNaming,
				Kind:        kind,
				Service:     s.service,
				Identity:    identityKey(FamilyNaming, s.service, kind, cluster, pattern),
				Statement:   statement,
				Numerator:   matched,
				Denominator: len(idx),
				Confidence:  confidence,
				Exceptions:  exceptions,
				Witnesses:   namingWitnesses(ms, idx, pattern),
				Components:  []intent.ConstraintComponent{component},
				Rule:        rule,
			})
		}
	}
	return out
}

func coveredByAncestor(kept map[string][]string, cluster, pattern string) bool {
	for _, ancestor := range kept[pattern] {
		if cluster == ancestor || strings.HasPrefix(cluster, ancestor+"/") {
			return true
		}
	}
	return false
}

// bestNamePattern ranks the patterns the cluster's names suggest and keeps the
// best one that clears the confidence floor. Unless tautologies are wanted,
// patterns the cluster satisfies by construction (its own path, or a module
// qualifier such as TypeScript's "src/services." in front of every symbol
// declared there) are passed over rather than ranked, so the strongest real
// regularity surfaces instead of losing to one that says nothing; the third
// result reports that one was passed over, for the suppression count.
func bestNamePattern(cluster string, ms []member, idx []int, minConfidence float64, includeTautologies bool, roots map[string]bool) (string, int, bool) {
	tally := map[string]int{}
	tautological := map[string]int{}
	for _, i := range idx {
		for _, pattern := range namePatterns(ms[i].name) {
			if !includeTautologies && constructionSatisfiedPattern(cluster, pattern, roots) {
				tautological[pattern]++
				continue
			}
			tally[pattern]++
		}
	}
	passedOver := false
	for _, n := range tautological {
		if ratio(n, len(idx)) >= minConfidence {
			passedOver = true
		}
	}
	patterns := sortedKeys(tally)
	sort.SliceStable(patterns, func(a, b int) bool {
		if tally[patterns[a]] != tally[patterns[b]] {
			return tally[patterns[a]] > tally[patterns[b]]
		}
		if len(patterns[a]) != len(patterns[b]) {
			return len(patterns[a]) > len(patterns[b])
		}
		return patterns[a] < patterns[b]
	})
	if len(patterns) > 5 {
		patterns = patterns[:5]
	}
	best, bestMatched := "", 0
	for _, pattern := range patterns {
		matched := 0
		for _, i := range idx {
			if matchMinedName(ms[i].name, pattern) {
				matched++
			}
		}
		if ratio(matched, len(idx)) < minConfidence {
			continue
		}
		if matched > bestMatched || (matched == bestMatched && len(pattern) > len(best)) {
			best, bestMatched = pattern, matched
		}
	}
	return best, bestMatched, passedOver
}

func namePatterns(name string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(pattern string) {
		if !seen[pattern] {
			seen[pattern] = true
			out = append(out, pattern)
		}
	}
	for i := 1; i < len(name); i++ {
		if !nameBoundary(name, i) {
			continue
		}
		if suffix := name[i:]; len(suffix) >= minPatternLiteral && validPatternLiteral(suffix) {
			add("*" + suffix)
		}
		if prefix := name[:i]; len(prefix) >= minPatternLiteral && validPatternLiteral(prefix) {
			add(prefix + "*")
		}
	}
	return out
}

func nameBoundary(name string, i int) bool {
	prev, cur := name[i-1], name[i]
	switch prev {
	case ':', '_', '-', '.', '/', '#':
		return true
	}
	return prev >= 'a' && prev <= 'z' && cur >= 'A' && cur <= 'Z'
}

func validPatternLiteral(literal string) bool {
	return !strings.ContainsAny(literal, "*?[]{}\t\n ")
}

func matchMinedName(name, pattern string) bool {
	switch {
	case strings.HasPrefix(pattern, "*"):
		return strings.HasSuffix(name, pattern[1:])
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(name, pattern[:len(pattern)-1])
	default:
		return name == pattern
	}
}

func globalMatchRate(ms []member, pattern string) float64 {
	matched := 0
	for _, m := range ms {
		if matchMinedName(m.name, pattern) {
			matched++
		}
	}
	return ratio(matched, len(ms))
}

func namingExceptions(ms []member, idx []int, pattern string) []Exception {
	var out []Exception
	for _, i := range idx {
		if matchMinedName(ms[i].name, pattern) {
			continue
		}
		out = append(out, Exception{Name: ms[i].name, File: ms[i].file, Detail: "name outside " + pattern})
	}
	return out
}

func namingWitnesses(ms []member, idx []int, pattern string) []Witness {
	var out []Witness
	for _, i := range idx {
		if len(out) == maxWitnesses {
			break
		}
		if matchMinedName(ms[i].name, pattern) {
			out = append(out, Witness{Name: ms[i].name, File: ms[i].file})
		}
	}
	return out
}
