package constraints

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// ExplainerName is what this package's findings carry as their Source. It is a
// const rather than a string literal at each site because two other packages
// grade on it — the gate's fail list and the diff's structural-cause test — and
// a typo in either would silently stop enforcing declared law.
const ExplainerName = "constraints"

// violationTitlePrefixes are the three shapes rule.titled produces, longest
// first so "Advisory constraint" is never read as a rule id of "constraint".
var violationTitlePrefixes = []string{
	"Advisory constraint ",
	"Strict constraint ",
	"Constraint ",
}

const violationTitleInfix = " violated: "

// RuleIDFromTitle recovers the rule a violation finding verdicted, or empty for
// any other finding. The title is the only place the rule id survives into a
// snapshot's findings, and it is stable by construction: the diff keys findings
// on it, so it cannot drift without the ratchet noticing first.
func RuleIDFromTitle(title string) string {
	for _, prefix := range violationTitlePrefixes {
		rest, cut := strings.CutPrefix(title, prefix)
		if !cut {
			continue
		}
		id, _, found := strings.Cut(rest, violationTitleInfix)
		if !found || id == "" || strings.Contains(id, " ") {
			return ""
		}
		return id
	}
	return ""
}

// WitnessFromTitle recovers the witness a violation finding names — the same
// string an exemption's witness is matched against, because exemptVerdicts
// builds the title from it. Empty for any other finding. It exists so the diff
// can ask whether THIS breach was exempted rather than whether the rule's
// exemption set moved at all.
func WitnessFromTitle(title string) string {
	for _, prefix := range violationTitlePrefixes {
		rest, cut := strings.CutPrefix(title, prefix)
		if !cut {
			continue
		}
		id, witness, found := strings.Cut(rest, violationTitleInfix)
		if !found || id == "" || strings.Contains(id, " ") {
			return ""
		}
		return witness
	}
	return ""
}

// MembershipIndex answers what each declared component selected in one
// snapshot. It exists so a surface outside this package can compare two
// snapshots' memberships without re-deriving what a selector means — there is
// one definition of membership and it is resolveMembership.
type MembershipIndex struct {
	members        map[string]map[string]bool
	ruleComponents map[string][]string
	ruleDeclared   map[string]string
	ruleExempts    map[string]map[string]bool
	names          map[string]bool
	repoOf         map[string]string
	repos          map[string]bool
}

// declarationBookkeeping are the rule props excluded from a rule's declaration
// identity, because none of them is a term of what the rule judges and a
// snapshot that treats them as one credits a real fix to a change in the law.
//
//   - because is the sentence the finding quotes.
//   - source is the file the rule was declared in. Moving a rule between
//     constraints files judges nothing differently.
//   - recipe and instance are the provenance a recipe expansion stamps.
//     Relabelling an instance changes what a verdict SAYS, never what it
//     decides.
//   - exempt is excluded because it is not one term: it is a set of carve-outs
//     each naming its own witness, and comparing the set as a blob makes an
//     exemption added for witness X answer for witness Y. Y then landed in
//     "the breaching code is unchanged; the law stopped asking" — a false
//     factual claim about work someone did. It is compared per witness instead,
//     by Exempts.
//
// Everything else — the form, the components, the via, the pattern, the mode —
// changes what a verdict means, so a change to any of it means the snapshot
// cannot attribute a disappeared breach to the code.
var declarationBookkeeping = map[string]bool{
	"because":  true,
	"source":   true,
	"recipe":   true,
	"instance": true,
	"exempt":   true,
}

// NewMembershipIndex resolves every declared component against the facts of one
// snapshot. Facts with no declaration in them yield an empty index rather than
// an error: a snapshot that declares nothing selects nothing.
func NewMembershipIndex(ff []facts.Fact) *MembershipIndex {
	store := facts.NewStore()
	store.Add(ff...)
	components, rules := declarations(store)
	index := &MembershipIndex{
		members:        make(map[string]map[string]bool, len(components)),
		ruleComponents: make(map[string][]string, len(rules)),
		ruleDeclared:   make(map[string]string, len(rules)),
		ruleExempts:    make(map[string]map[string]bool, len(rules)),
		names:          make(map[string]bool, len(ff)),
		repoOf:         make(map[string]string, len(ff)),
		repos:          map[string]bool{},
	}
	for name, c := range components {
		index.members[name], _ = resolveMembership(store, c)
	}
	for _, r := range rules {
		index.ruleComponents[r.id] = r.componentNames()
		witnesses := make(map[string]bool, len(r.exempt))
		for _, ex := range r.exempt {
			witnesses[ex.Witness] = true
		}
		index.ruleExempts[r.id] = witnesses
	}
	for _, f := range store.ByKind(facts.KindIntent) {
		if f.PropString("intent_kind") != "rule" {
			continue
		}
		if id := f.PropString("rule"); id != "" {
			index.ruleDeclared[id] = declarationIdentity(f)
		}
	}
	for _, f := range ff {
		if f.Name != "" {
			index.names[f.Name] = true
			if _, seen := index.repoOf[f.Name]; !seen {
				index.repoOf[f.Name] = f.Repo
			}
		}
		if f.Repo != "" {
			index.repos[f.Repo] = true
		}
	}
	return index
}

// Exempts reports whether the snapshot's declaration of one rule carves out one
// witness by name. Per witness, never per rule: an exemption added for one
// breach says nothing about another, and the blob comparison that conflated
// them printed a genuine fix as a law that stopped asking.
func (m *MembershipIndex) Exempts(id, witness string) bool {
	return m.ruleExempts[id][witness]
}

// RepoOf names the repository label a fact of that name carried, and whether
// the snapshot measured such a fact at all.
func (m *MembershipIndex) RepoOf(name string) (string, bool) {
	repo, measured := m.repoOf[name]
	return repo, measured
}

// Repos is the set of repository labels this snapshot measured — what tells a
// repo dropped from a union snapshot from code that was deleted. Both leave the
// witness unmeasured; only one of them is a fix.
func (m *MembershipIndex) Repos() map[string]bool { return m.repos }

// Declaration returns the canonical identity of one rule's compiled
// declaration, and whether the snapshot declared that rule at all. Two
// snapshots whose identities differ for the same rule id declared two different
// laws, whatever the id says.
func (m *MembershipIndex) Declaration(id string) (string, bool) {
	declaration, declared := m.ruleDeclared[id]
	return declaration, declared
}

// declarationIdentity renders a rule intent fact's props as one canonical
// string: sorted, so the identity is a function of the declaration and never of
// map order.
func declarationIdentity(f facts.Fact) string {
	pairs := make([]string, 0, f.PropCount())
	for key, value := range f.Props {
		if declarationBookkeeping[key] {
			continue
		}
		pairs = append(pairs, fmt.Sprintf("%s=%v", key, value))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\n")
}

// Selects reports whether the component contained the named fact.
func (m *MembershipIndex) Selects(component, name string) bool {
	return m.members[component][name]
}

// Measured reports whether the snapshot carried a fact of that name at all —
// the test that separates "the code was deleted" from "the code is still here
// and no longer selected".
func (m *MembershipIndex) Measured(name string) bool { return m.names[name] }

// ComponentsOfRule lists every component a rule names, in declaration order.
func (m *MembershipIndex) ComponentsOfRule(id string) []string { return m.ruleComponents[id] }

// componentNames lists every component a rule names — the source side, the
// counterpart side, and every step or exception list.
func (r rule) componentNames() []string {
	var out []string
	add := func(names ...string) {
		for _, n := range names {
			if n != "" {
				out = append(out, n)
			}
		}
	}
	add(r.forbid, r.forbidReach, r.to, r.allow, r.protect, r.private, r.forbidFact,
		r.cap, r.require, r.requireDefines, r.requireName, r.requireEdge, r.protocol, r.guide)
	add(r.only...)
	add(r.owners...)
	add(r.except...)
	add(r.steps...)
	return out
}
