package mining

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
)

type propPair struct {
	prop  string
	value string
}

func (s *scope) mineProps() []Candidate {
	var out []Candidate
	for _, kind := range minedKindOrder {
		ms := s.members[kind]
		if len(ms) == 0 {
			continue
		}
		values := make([][]propPair, len(ms))
		index := map[propPair][]int{}
		for i, m := range ms {
			seen := map[propPair]bool{}
			for _, prop := range sortedStringProps(m.fact.Props) {
				for _, v := range strings.Fields(m.fact.PropString(prop)) {
					p := propPair{prop: prop, value: v}
					if seen[p] {
						continue
					}
					seen[p] = true
					values[i] = append(values[i], p)
					index[p] = append(index[p], i)
				}
			}
		}
		pairs := sortedPropPairs(index)
		component := s.kindComponent(kind, ms)
		out = append(out, s.mineUnconditionalProps(kind, ms, component, index, pairs)...)
		out = append(out, s.mineConditionalProps(kind, ms, component, values, index, pairs)...)
	}
	return out
}

func (s *scope) mineUnconditionalProps(kind string, ms []member, component intent.ConstraintComponent, index map[propPair][]int, pairs []propPair) []Candidate {
	var out []Candidate
	total := len(ms)
	for _, p := range pairs {
		holding := index[p]
		if ratio(len(holding), total) < s.cfg.MinConfidence {
			continue
		}
		exceptions := exceptionsLacking(ms, holding, p)
		if !s.admit(FamilyPropImplication, total, len(exceptions)) {
			continue
		}
		statement := s.statement(fmt.Sprintf("%d/%d %s facts have %s containing %s", len(holding), total, kind, p.prop, p.value))
		rule := intent.ConstraintRule{
			ID:              slug("mined", kind, "all", p.prop, p.value),
			Require:         component.Name,
			MustPropContain: &intent.PropMatch{Prop: p.prop, Value: p.value},
			Mode:            "advisory",
			Because:         because(statement, exceptions),
		}
		out = append(out, Candidate{
			Family:      FamilyPropImplication,
			Kind:        kind,
			Service:     s.service,
			Identity:    identityKey(FamilyPropImplication, s.service, kind, "all", p.prop, p.value),
			Statement:   statement,
			Numerator:   len(holding),
			Denominator: total,
			Confidence:  ratio(len(holding), total),
			Exceptions:  exceptions,
			Components:  []intent.ConstraintComponent{component},
			Rule:        rule,
		})
	}
	return out
}

func (s *scope) mineConditionalProps(kind string, ms []member, component intent.ConstraintComponent, values [][]propPair, index map[propPair][]int, pairs []propPair) []Candidate {
	var out []Candidate
	total := len(ms)
	for _, antecedent := range pairs {
		holding := index[antecedent]
		tally := map[propPair]int{}
		for _, i := range holding {
			for _, consequent := range values[i] {
				if consequent != antecedent {
					tally[consequent]++
				}
			}
		}
		for _, consequent := range sortedPropPairs(tally) {
			matched := tally[consequent]
			confidence := ratio(matched, len(holding))
			if confidence < s.cfg.MinConfidence {
				continue
			}
			baseRate := ratio(len(index[consequent]), total)
			if baseRate >= s.cfg.MinConfidence || confidence <= baseRate {
				continue
			}
			exceptions := conditionalExceptions(ms, holding, values, consequent)
			if !s.admit(FamilyPropImplication, len(holding), len(exceptions)) {
				continue
			}
			if !s.cfg.IncludeTautologies && tautologicalImplication(antecedent, consequent) {
				s.suppressed[FamilyPropImplication].Tautological++
				continue
			}
			statement := s.statement(fmt.Sprintf("%d/%d %s facts whose %s contains %s also have %s containing %s",
				matched, len(holding), kind, antecedent.prop, antecedent.value, consequent.prop, consequent.value))
			rule := intent.ConstraintRule{
				ID:               slug("mined", kind, antecedent.prop, antecedent.value, "implies", consequent.prop, consequent.value),
				Require:          component.Name,
				WhenPropContains: &intent.PropMatch{Prop: antecedent.prop, Value: antecedent.value},
				MustPropContain:  &intent.PropMatch{Prop: consequent.prop, Value: consequent.value},
				Mode:             "advisory",
				Because:          because(statement, exceptions),
			}
			out = append(out, Candidate{
				Family:      FamilyPropImplication,
				Kind:        kind,
				Service:     s.service,
				Identity:    identityKey(FamilyPropImplication, s.service, kind, "when", antecedent.prop, antecedent.value, "then", consequent.prop, consequent.value),
				Statement:   statement,
				Numerator:   matched,
				Denominator: len(holding),
				Confidence:  confidence,
				Exceptions:  exceptions,
				Components:  []intent.ConstraintComponent{component},
				Rule:        rule,
				pairKey:     reversedPairKey(antecedent, consequent),
			})
		}
	}
	if s.cfg.IncludeTautologies {
		return out
	}
	return s.dropReversedDuplicates(out)
}

func exceptionsLacking(ms []member, holding []int, p propPair) []Exception {
	holds := map[int]bool{}
	for _, i := range holding {
		holds[i] = true
	}
	var out []Exception
	for i, m := range ms {
		if holds[i] {
			continue
		}
		out = append(out, Exception{Name: m.name, File: m.file, Detail: "missing " + p.prop + " " + p.value})
	}
	return out
}

func conditionalExceptions(ms []member, holding []int, values [][]propPair, consequent propPair) []Exception {
	var out []Exception
	for _, i := range holding {
		has := false
		for _, p := range values[i] {
			if p == consequent {
				has = true
				break
			}
		}
		if has {
			continue
		}
		out = append(out, Exception{Name: ms[i].name, File: ms[i].file, Detail: "missing " + consequent.prop + " " + consequent.value})
	}
	return out
}

func because(statement string, exceptions []Exception) string {
	text := "Mined regularity: " + statement + ". " + exceptionClause(exceptions) + " Rewrite this rationale before adopting."
	return text
}

func exceptionClause(exceptions []Exception) string {
	if len(exceptions) == 0 {
		return "No exceptions in the mined snapshot."
	}
	names := make([]string, len(exceptions))
	for i, e := range exceptions {
		if e.File != "" {
			names[i] = e.Name + " (" + e.File + ")"
		} else {
			names[i] = e.Name
		}
	}
	return "Exceptions: " + strings.Join(names, ", ") + "."
}

// sortedStringProps are the props a rule can be mined over. The doc prop is not
// one: it is a comment's words, and a word two symbols' comments share is not a
// regularity of the code. Nor is body_hash: it is one symbol's fingerprint, and
// mining over it yields a rule per symbol that says nothing. decl_hash is the same.
func sortedStringProps(props map[string]any) []string {
	var keys []string
	for k, v := range props {
		if k == facts.PropDoc || k == facts.PropBodyHash || k == facts.PropDeclHash {
			continue
		}
		if sv, ok := v.(string); ok && sv != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func sortedPropPairs[V any](m map[propPair]V) []propPair {
	pairs := make([]propPair, 0, len(m))
	for p := range m {
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].prop != pairs[j].prop {
			return pairs[i].prop < pairs[j].prop
		}
		return pairs[i].value < pairs[j].value
	})
	return pairs
}
