package mining

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
)

const maxAllowOnlyTargets = 4

type minedEdge struct {
	via        string
	source     string
	sourceFile string
	target     string
	srcCluster string
	tgtCluster string
}

func (s *scope) mineEdges() []Candidate {
	edges := s.collectEdges()
	grouped := map[string]map[string][]minedEdge{}
	for _, e := range edges {
		if grouped[e.srcCluster] == nil {
			grouped[e.srcCluster] = map[string][]minedEdge{}
		}
		grouped[e.srcCluster][e.via] = append(grouped[e.srcCluster][e.via], e)
	}
	var out []Candidate
	for _, srcCluster := range sortedKeys(grouped) {
		for _, via := range sortedKeys(grouped[srcCluster]) {
			group := grouped[srcCluster][via]
			out = append(out, s.mineForbid(srcCluster, via, group)...)
			out = append(out, s.mineAllowOnly(srcCluster, via, group)...)
		}
	}
	return out
}

func (s *scope) collectEdges() []minedEdge {
	nameFile := map[string]string{}
	for _, kind := range minedKindOrder {
		for _, m := range s.members[kind] {
			if _, ok := nameFile[m.name]; !ok {
				nameFile[m.name] = m.file
			}
		}
	}
	// A name several files declare is its first file above; a fact that
	// carries the file its target is declared in (target_file) is read by that
	// file, and by nothing when the name is not declared there.
	declaredIn := map[string]map[string]bool{}
	for _, kind := range minedKindOrder {
		for _, m := range s.allFacts[kind] {
			if declaredIn[m.name] == nil {
				declaredIn[m.name] = map[string]bool{}
			}
			declaredIn[m.name][m.file] = true
		}
	}
	viaAllowed := map[string]bool{}
	for _, via := range minedVias {
		viaAllowed[via] = true
	}
	seen := map[string]bool{}
	var edges []minedEdge
	kinds := append(append([]string{}, minedKindOrder...), facts.KindDependency)
	for _, kind := range kinds {
		for _, m := range s.allFacts[kind] {
			if m.file == "" {
				continue
			}
			srcCluster := clusterOf(m.file)
			if srcCluster == "" {
				continue
			}
			for _, rel := range m.fact.Relations {
				if !viaAllowed[rel.Kind] {
					continue
				}
				targetFile, ok := nameFile[rel.Target]
				if carried, _ := m.fact.PropAny(facts.PropTargetFile).(string); carried != "" {
					targetFile, ok = carried, declaredIn[rel.Target][carried]
				}
				if !ok {
					continue
				}
				tgtCluster := clusterOf(targetFile)
				if tgtCluster == "" || tgtCluster == srcCluster {
					continue
				}
				key := rel.Kind + "\x00" + m.name + "\x00" + rel.Target
				if seen[key] {
					continue
				}
				seen[key] = true
				edges = append(edges, minedEdge{
					via:        rel.Kind,
					source:     m.name,
					sourceFile: m.file,
					target:     rel.Target,
					srcCluster: srcCluster,
					tgtCluster: tgtCluster,
				})
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].via != edges[j].via {
			return edges[i].via < edges[j].via
		}
		if edges[i].source != edges[j].source {
			return edges[i].source < edges[j].source
		}
		return edges[i].target < edges[j].target
	})
	return edges
}

func (s *scope) mineForbid(srcCluster, via string, group []minedEdge) []Candidate {
	total := len(group)
	byTarget := map[string][]minedEdge{}
	for _, e := range group {
		byTarget[e.tgtCluster] = append(byTarget[e.tgtCluster], e)
	}
	var out []Candidate
	for _, tgtCluster := range sortedKeys(byTarget) {
		violations := byTarget[tgtCluster]
		if ratio(len(violations), total) > 1-s.cfg.MinConfidence {
			continue
		}
		exceptions := make([]Exception, 0, len(violations))
		for _, e := range violations {
			exceptions = append(exceptions, Exception{
				Name:   e.source + " -> " + e.target,
				File:   e.sourceFile,
				Detail: via + " edge into " + tgtCluster,
			})
		}
		var witnesses []Witness
		for _, e := range group {
			if len(witnesses) == maxWitnesses {
				break
			}
			if e.tgtCluster != tgtCluster {
				witnesses = append(witnesses, Witness{Name: e.source, File: e.sourceFile, Target: e.target})
			}
		}
		if !s.admit(FamilyForbidEdge, total, len(exceptions)) {
			continue
		}
		source := s.clusterComponent(srcCluster, "")
		target := s.clusterComponent(tgtCluster, "")
		statement := s.statement(fmt.Sprintf("%d/%d %s edges leaving %s/ land outside %s/", total-len(violations), total, via, srcCluster, tgtCluster))
		rule := intent.ConstraintRule{
			ID:      slug("mined-forbid", srcCluster, "to", tgtCluster, via),
			Forbid:  source.Name,
			To:      target.Name,
			Via:     via,
			Mode:    "advisory",
			Because: because(statement, exceptions),
		}
		out = append(out, Candidate{
			Family:      FamilyForbidEdge,
			Service:     s.service,
			Identity:    identityKey(FamilyForbidEdge, s.service, srcCluster, tgtCluster, via),
			Statement:   statement,
			Numerator:   total - len(violations),
			Denominator: total,
			Confidence:  ratio(total-len(violations), total),
			Exceptions:  exceptions,
			Witnesses:   witnesses,
			Components:  []intent.ConstraintComponent{source, target},
			Rule:        rule,
		})
	}
	return out
}

func (s *scope) mineAllowOnly(srcCluster, via string, group []minedEdge) []Candidate {
	total := len(group)
	byTarget := map[string][]minedEdge{}
	for _, e := range group {
		byTarget[e.tgtCluster] = append(byTarget[e.tgtCluster], e)
	}
	targets := sortedKeys(byTarget)
	sort.SliceStable(targets, func(i, j int) bool {
		if len(byTarget[targets[i]]) != len(byTarget[targets[j]]) {
			return len(byTarget[targets[i]]) > len(byTarget[targets[j]])
		}
		return targets[i] < targets[j]
	})
	covered := 0
	var taken []string
	for _, t := range targets {
		if ratio(covered, total) >= s.cfg.MinConfidence {
			break
		}
		taken = append(taken, t)
		covered += len(byTarget[t])
	}
	if len(taken) == 0 || len(taken) > maxAllowOnlyTargets || ratio(covered, total) < s.cfg.MinConfidence {
		return nil
	}
	sort.Strings(taken)
	takenSet := map[string]bool{}
	for _, t := range taken {
		takenSet[t] = true
	}
	var exceptions []Exception
	for _, e := range group {
		if takenSet[e.tgtCluster] {
			continue
		}
		exceptions = append(exceptions, Exception{
			Name:   e.source + " -> " + e.target,
			File:   e.sourceFile,
			Detail: via + " edge into " + e.tgtCluster,
		})
	}
	sort.Slice(exceptions, func(i, j int) bool { return exceptions[i].Name < exceptions[j].Name })
	if !s.admit(FamilyAllowOnly, total, len(exceptions)) {
		return nil
	}
	source := s.clusterComponent(srcCluster, "")
	components := []intent.ConstraintComponent{source}
	var only []string
	for _, t := range taken {
		target := s.clusterComponent(t, "")
		components = append(components, target)
		only = append(only, target.Name)
	}
	statement := s.statement(fmt.Sprintf("%d/%d %s edges leaving %s/ land in %s", covered, total, via, srcCluster, strings.Join(taken, ", ")))
	rule := intent.ConstraintRule{
		ID:      slug("mined-allow", srcCluster, via),
		Allow:   source.Name,
		Only:    only,
		Via:     via,
		Mode:    "advisory",
		Because: because(statement, exceptions),
	}
	return []Candidate{{
		Family:      FamilyAllowOnly,
		Service:     s.service,
		Identity:    identityKey(FamilyAllowOnly, s.service, srcCluster, via),
		Statement:   statement,
		Numerator:   covered,
		Denominator: total,
		Confidence:  ratio(covered, total),
		Exceptions:  exceptions,
		Components:  components,
		Rule:        rule,
	}}
}
