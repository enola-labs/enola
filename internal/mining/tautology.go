package mining

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

func tautologicalImplication(antecedent, consequent propPair) bool {
	return antecedent.prop == consequent.prop || antecedent.value == consequent.value
}

// constructionSatisfiedPattern reports whether every name declared under cluster
// matches pattern because of where it is declared: the literal is the cluster's own
// path, or a leading part of it, written as a name qualifier.
//
// The path may sit behind a prefix. A Go name is its import path, so a symbol in
// internal/x of module example.com/m is named example.com/m/internal/x.Name, and
// "example.com/m/internal/x.*" (or ".../internal/*") holds for every symbol under
// internal/x by construction. The path is matched from a segment boundary of the
// literal, so a literal that only ends in the same letters is not mistaken for it.
// A path below the cluster counts too: it restates where names are declared.
// A literal that is one of roots, the prefixes the scope's names carry before their
// directory (a Go module path), or any leading part of one, says only that the name
// is qualified at all.
func constructionSatisfiedPattern(cluster, pattern string, roots map[string]bool) bool {
	literal, isPrefix := strings.CutSuffix(pattern, "*")
	if !isPrefix || strings.HasPrefix(literal, "*") || literal == "" {
		return false
	}
	for root := range roots {
		if strings.HasPrefix(root, literal) {
			return true
		}
	}
	separated := strings.HasSuffix(literal, "/") || strings.HasSuffix(literal, ".")
	qualifies := func(tail string) bool {
		if strings.HasPrefix(cluster+"/", tail) || strings.HasPrefix(cluster+".", tail) {
			return true
		}
		// A directory below the cluster: "most names here are qualified by pkg/a"
		// only says where most of them are declared.
		return separated && strings.HasPrefix(tail, cluster+"/")
	}
	if qualifies(literal) {
		return true
	}
	if !separated {
		return false
	}
	for i := 1; i < len(literal); i++ {
		if literal[i-1] == '/' && qualifies(literal[i:]) {
			return true
		}
	}
	return false
}

func tautologicalNaming(cluster, pattern string, memberCount int, roots map[string]bool) bool {
	return memberCount == 1 || constructionSatisfiedPattern(cluster, pattern, roots)
}

// qualifierRoots collects the prefixes module names carry in front of their
// directory: a Go package in internal/x of module example.com/m is named
// example.com/m/internal/x, so its root is "example.com/m/". Every name declared
// under that module starts with the root by construction.
func qualifierRoots(ff []facts.Fact) map[string]bool {
	roots := map[string]bool{}
	for _, f := range ff {
		if f.Kind != facts.KindModule || f.Name == "" || f.Name == f.File {
			continue
		}
		switch dir := f.File; {
		case dir == "" || dir == ".":
			if strings.Contains(f.Name, "/") {
				roots[f.Name+"/"] = true
			}
		case strings.HasSuffix(f.Name, "/"+dir):
			roots[strings.TrimSuffix(f.Name, dir)] = true
		}
	}
	return roots
}

func reversedPairKey(antecedent, consequent propPair) string {
	first, second := antecedent, consequent
	if second.prop < first.prop || (second.prop == first.prop && second.value < first.value) {
		first, second = second, first
	}
	return identityKey(first.prop, first.value, second.prop, second.value)
}

func (s *scope) dropReversedDuplicates(candidates []Candidate) []Candidate {
	byPair := map[string][]int{}
	for i, c := range candidates {
		if c.Confidence == 1.0 {
			byPair[c.pairKey] = append(byPair[c.pairKey], i)
		}
	}
	drop := map[int]bool{}
	for _, key := range sortedKeys(byPair) {
		indices := byPair[key]
		if len(indices) < 2 {
			continue
		}
		sort.SliceStable(indices, func(a, b int) bool {
			ca, cb := candidates[indices[a]], candidates[indices[b]]
			if ca.Denominator != cb.Denominator {
				return ca.Denominator > cb.Denominator
			}
			return ca.Identity < cb.Identity
		})
		for _, i := range indices[1:] {
			drop[i] = true
			s.suppressed[FamilyPropImplication].Tautological++
		}
	}
	kept := make([]Candidate, 0, len(candidates))
	for i, c := range candidates {
		if !drop[i] {
			kept = append(kept, c)
		}
	}
	return kept
}
