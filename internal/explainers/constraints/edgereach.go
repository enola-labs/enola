package constraints

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
)

// The declaration screen refuses the pairings this vocabulary can never state a
// basis for. What it cannot see is the snapshot: a component may declare an
// ownership honestly and still reach nothing, because the estate measures no
// methods for its members, or because its members carry no edge of the kind
// the rule walks. A rule then resolves that role against nothing and reads the
// nothing as a verdict — an owners: that owns no edge makes every edge unowned,
// an only: that absorbs no landing makes every landing disallowed.
//
// The question is asked per role and on ONE side. The previous machinery ORed
// three arms belonging to different directions, so an edge pointing the wrong
// way declared a component sighted — deleting an unrelated INBOUND edge flipped
// an owners: rule from a false breach to a correct refusal, which is a verdict
// depending on a fact no rule mentioned. A source-side role asks only whether
// the component's edge sources carry such an edge; a target-side role asks only
// whether such an edge resolves onto it.
//
// One reading is deliberately not asked, and only one: the subject of an
// existential form resolved against the end an edge LANDS on. There an empty
// resolution IS the breach the form exists to report, so a reach test asking
// the same question silences exactly the total violation — which is how a rule
// reporting zero breaches against a total violation shipped once. The
// source-side subject of the same forms gets no such pass: see censusAnswers.

// edgeReachConfidence is the gate-failing 1.0 rather than the 0.4 the skip
// advisories sit at, for the reason unmeasuredPropConfidence is: a skip
// advisory reports a verdict not reached for a cause the snapshot cannot
// resolve, while a role that resolves nothing is not ambiguous. The rule would
// emit either nothing or a full set of false breaches, and both read as decided.
const edgeReachConfidence = 1.0

// UnreachableRole is one declared component sitting in a role that resolves
// against an edge kind it reaches nothing on, from the side that role uses.
type UnreachableRole struct {
	Rule      string `json:"rule"`
	Component string `json:"component"`
	Role      string `json:"role"`
	Side      string `json:"side"`
	Via       string `json:"via"`
	Owns      string `json:"owns"`
	Members   int    `json:"members"`
	Source    string `json:"source,omitempty"`
	// Partial marks the one unreachable role that does NOT silence its rule: a
	// role reading an empty resolution as no verdict, on some but not all of the
	// kinds a multi-kind rule walks. The rule judged the kinds it could reach, so
	// its verdicts stand and its silence is only partial — reported, never a
	// refusal, because refusing would delete the enforcement that did work.
	Partial bool `json:"partial,omitempty"`
}

// Problem renders the defect as the sentence both the lint surface and the
// finding state, so the authoring loop and the gate cannot describe the same
// declaration differently.
func (u UnreachableRole) Problem() string {
	if u.Side == intent.SideSource {
		return fmt.Sprintf("%s sits in the %s role of %s, which resolves it as the source of a %s edge, and no fact it owns or contains carries one — it owns %s, over %d member(s)",
			u.Component, u.Role, u.Rule, u.Via, u.Owns, u.Members)
	}
	return fmt.Sprintf("%s sits in the %s role of %s, which resolves it as the target of a %s edge, and no measured %s edge names a member, names a method it owns, or grounds onto a file it measured a member in — it owns %s, over %d member(s)",
		u.Component, u.Role, u.Rule, u.Via, u.Via, u.Owns, u.Members)
}

// edgeSight indexes, per edge kind, what the snapshot measured on the far end:
// whether the kind was measured at all, the names its edges target, and the
// measured files its path-shaped targets ground onto. Those three plus a
// component's own members are the whole of what a target-side role can resolve
// against.
type edgeSight struct {
	measured map[string]bool
	targets  map[string]map[string]bool
	grounded map[string]map[string]bool
}

// newEdgeSight indexes the named edge kinds in one pass. FactsRef, not All:
// this only reads, and retains nothing but target names and resolved paths.
func newEdgeSight(store *facts.Store, vias map[string]bool, ground *grounding) *edgeSight {
	s := &edgeSight{
		measured: map[string]bool{},
		targets:  make(map[string]map[string]bool, len(vias)),
		grounded: make(map[string]map[string]bool, len(vias)),
	}
	for via := range vias {
		s.targets[via] = map[string]bool{}
		s.grounded[via] = map[string]bool{}
	}
	for _, f := range store.FactsRef() {
		for _, rel := range f.Relations {
			set := s.targets[rel.Kind]
			if set == nil {
				continue
			}
			s.measured[rel.Kind] = true
			set[rel.Target] = true
			// The path test first: a calls or implements target is a symbol name,
			// so resolving one against the file index answers nothing and this
			// loop runs over every relation in the snapshot.
			if !pathTargetEdge(rel, f) {
				continue
			}
			if path, ok := ground.resolve(rel.Target, f.Repo, f.File); ok {
				s.grounded[rel.Kind][path] = true
			}
		}
	}
	return s
}

// censusAnswers reports whether the extraction census already answered this
// role's reach, which is the ONE reading its carve-out justifies: a subject
// resolved against the end an edge LANDS on, whose empty resolution IS the
// breach the existential form exists to report. Screening that role for reach
// would silence exactly the total violation.
//
// It never answers for a SOURCE-side subject. The census is keyed on (repo,
// file extension): it certifies that a kind of edge is measured in a repo's
// files, never which FACTS in them carry it. A concept owning nothing sources
// its edges from class facts while the calls ride `Owner#method` facts, so the
// census earns its "not extraction blindness" certificate on facts the rule
// does not walk — and the rule reads its own empty source set as every member
// breaching, or as nothing to say at all. Both are decided verdicts drawn from
// a resolution that reached no fact, which is what the reach screen exists to
// refuse.
func censusAnswers(role intent.EdgeRole) bool {
	return role.Subject && role.CensusMeasured && role.Side == intent.SideTarget
}

// unreachableRoles reports every role that resolves a concept component against
// an edge kind it reaches nothing on, in rule then component then via order.
// Components silenced for another reason are skipped: an unasked component
// belongs to a repo this snapshot never loaded, an unevaluable one has no
// membership to measure, and an empty one is the dead-selector advisory's
// subject — calling any of them unreachable replaces a known cause with a guess.
func unreachableRoles(store *facts.Store, rules []rule, resolve *resolver, ground *grounding, silenced ...map[string]bool) []UnreachableRole {
	vias := map[string]bool{}
	concept := false
	for _, r := range rules {
		for _, role := range r.edgeRoles() {
			if resolve.components[role.Component].predicated() {
				concept = true
			}
		}
		for _, via := range r.walkedVias() {
			vias[via] = true
		}
	}
	if !concept || len(vias) == 0 {
		return nil
	}
	sight := newEdgeSight(store, vias, ground)

	quiet := func(name string) bool {
		for _, set := range silenced {
			if set[name] {
				return true
			}
		}
		return false
	}

	var out []UnreachableRole
	for _, r := range rules {
		for _, role := range r.edgeRoles() {
			c, declared := resolve.components[role.Component]
			if !declared || !c.predicated() || quiet(role.Component) {
				continue
			}
			if censusAnswers(role) {
				continue
			}
			if len(resolve.members[role.Component]) == 0 {
				continue
			}
			walked := r.walkedVias()
			var unreached []string
			for _, via := range walked {
				// An edge kind the snapshot never measured is not unreachability
				// and must not read as it: absent and unreachable are different
				// claims, the same distinction blindSourceClasses draws.
				if !sight.measured[via] || sight.reaches(r, role, resolve, via) {
					continue
				}
				unreached = append(unreached, via)
			}
			if len(unreached) == 0 {
				continue
			}
			owns, _ := intent.OwnershipPrecedence(c.owns, r.owns[role.Component])
			partial := !role.Breaches && len(unreached) < len(walked)
			for _, via := range unreached {
				out = append(out, UnreachableRole{
					Rule:      r.id,
					Component: role.Component,
					Role:      role.Role,
					Side:      role.Side,
					Via:       via,
					Owns:      owns,
					Members:   len(resolve.members[role.Component]),
					Source:    r.source,
					Partial:   partial,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		if out[i].Component != out[j].Component {
			return out[i].Component < out[j].Component
		}
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].Via < out[j].Via
	})
	return out
}

// reaches asks the one question the role's side uses, and never the other.
func (s *edgeSight) reaches(r rule, role intent.EdgeRole, resolve *resolver, via string) bool {
	if role.Side == intent.SideSource {
		for _, f := range resolve.sources(r, role.Component) {
			for _, rel := range f.Relations {
				if rel.Kind == via {
					return true
				}
			}
		}
		return false
	}
	named := s.targets[via]
	for name := range resolve.members[role.Component] {
		if named[name] {
			return true
		}
	}
	if resolve.ownsMethods(r, role.Component) {
		for name := range resolve.ownedMethodsOf(role.Component).ownerOf {
			if named[name] {
				return true
			}
		}
	}
	grounded := s.grounded[via]
	for _, f := range resolve.memberFacts[role.Component] {
		if f.File != "" && grounded[f.File] {
			return true
		}
	}
	return false
}

// edgeRoles reads the schema's role table for one compiled rule, so the
// explainer and the validator enumerate the same roles from the same place.
func (r rule) edgeRoles() []intent.EdgeRole {
	return intent.EdgeRoles(r.declaration())
}

// walkedVias reads the schema's via table for one compiled rule.
func (r rule) walkedVias() []string {
	return intent.RuleVias(r.declaration())
}

// declaration rebuilds the declared shape of a compiled rule, so the two schema
// tables are read from one definition rather than mirrored here. Only the
// fields the tables read are carried: a mirror that drifted would enumerate a
// role the validator does not, which is the gap the enumeration exists to close.
func (r rule) declaration() intent.ConstraintRule {
	return intent.ConstraintRule{
		Forbid:         r.forbid,
		ForbidReach:    r.forbidReach,
		To:             r.to,
		Allow:          r.allow,
		Only:           r.only,
		Protect:        r.protect,
		Owners:         r.owners,
		Private:        r.private,
		Except:         r.except,
		ForbidFact:     r.forbidFact,
		Cap:            r.cap,
		Require:        r.require,
		RequireDefines: r.requireDefines,
		RequireName:    r.requireName,
		RequireEdge:    r.requireEdge,
		Direction:      r.direction,
		Protocol:       r.protocol,
		Steps:          r.steps,
		Guide:          r.guide,
		Via:            r.via,
	}
}

// unreachableRoleTitle names the finding one unreachable role produces. The
// title is the diff's identity for a finding, so it carries the rule, the
// component, the role and the edge kind and nothing that moves between runs.
func unreachableRoleTitle(u UnreachableRole) string {
	if u.Partial {
		return fmt.Sprintf("Constraint rule %s judged no %s edge over %s in the %s role", u.Rule, u.Via, u.Component, u.Role)
	}
	return fmt.Sprintf("Constraint rule %s cannot verdict: %s resolves no %s edge in the %s role", u.Rule, u.Component, u.Via, u.Role)
}

// unreachableRoleInsight states one unreachable role as a finding. It names the
// reach rather than the breach count, because the count a reader would
// otherwise see — zero verdicts, or a full set of them — is exactly what the
// finding exists to disqualify.
func unreachableRoleInsight(u UnreachableRole, c component, r rule) facts.Insight {
	if u.Partial {
		return facts.Insight{
			Title:       unreachableRoleTitle(u),
			Description: fmt.Sprintf("%s. The rule walks %s, and it reached a verdict over the kinds this role can be resolved on — so its verdicts stand and this is not a refusal. What it cannot do is cover %s, and every surface renders a rule that judged three of four edge kinds exactly as it renders one that judged all four. Because: %s", u.Problem(), strings.Join(r.walkedVias(), ", "), u.Via, r.because),
			Confidence:  reachSkipConfidence,
			Evidence: []facts.Evidence{
				{Fact: "rule: " + u.Rule, Detail: "declared in " + u.Source},
				{Fact: "component: " + u.Component, Detail: "declared in " + c.source},
			},
			Actions: []string{
				fmt.Sprintf("Read this rule's verdict as covering every kind except %s", u.Via),
				fmt.Sprintf("Narrow the rule to the edge kinds %s can be resolved on, with via: on the declaring page", u.Component),
			},
		}
	}
	return facts.Insight{
		Title:       unreachableRoleTitle(u),
		Description: fmt.Sprintf("%s. The rule reads that nothing as %s, so it emitted no verdict at all: a role that resolves nothing must not have its silence, or its breaches, read as decided. The component selects %s. Because: %s", u.Problem(), unreachableMisreading(u.Role), selectorSummary(c), r.because),
		Confidence:  edgeReachConfidence,
		Evidence: []facts.Evidence{
			{Fact: "rule: " + u.Rule, Detail: "declared in " + u.Source},
			{Fact: "component: " + u.Component, Detail: "declared in " + c.source},
			{File: c.source, Fact: "component: " + u.Component, Detail: "the declaring file"},
		},
		Actions: []string{
			fmt.Sprintf("Declare owns: %s on %s in %s if its members' methods carry the %s edges", intent.OwnsMethods, u.Component, c.source, u.Via),
			fmt.Sprintf("Point the rule at an edge kind this snapshot measures on %s, or bind the role to a path component", u.Component),
		},
	}
}

// unreachableMisreading words what the empty resolution becomes once the rule
// reads it, which differs by role and is the whole reason an unreachable role
// is not merely a rule that judges less.
func unreachableMisreading(role string) string {
	switch role {
	case "owners":
		return "an edge arriving from outside every owner, including the owners' own"
	case "only":
		return "an edge landing outside every allowed component, including the ones landing inside it"
	case "except":
		return "a reach from outside the exception, including the excepted component's own"
	case "to":
		return "an edge that never lands there, so a forbidden edge reads as absent and a demanded one as missing"
	case "steps":
		return "a step nothing reaches, so a skipped prerequisite reads as satisfied"
	default:
		return "compliance"
	}
}

// UnreachableRoles is the lint entry point to the same measurement the
// explainer refuses on, so the authoring loop and the gate can never disagree
// about which role resolves nothing.
func UnreachableRoles(store *facts.Store) []UnreachableRole {
	components, rules := declarations(store)
	if len(components) == 0 {
		return nil
	}
	unasked := unaskedComponents(store, components)
	unevaluable := map[string]bool{}
	for _, u := range unevaluableSelectors(store, components, unasked) {
		unevaluable[u.Component] = true
	}
	members := map[string]map[string]bool{}
	memberFacts := map[string][]facts.Fact{}
	carried := map[string][]facts.Fact{}
	for name, c := range components {
		members[name], memberFacts[name] = resolveMembership(store, c)
		sources := append([]facts.Fact{}, memberFacts[name]...)
		for _, f := range store.ByKind(facts.KindDependency) {
			if carrierFor(f, c) {
				sources = append(sources, f)
			}
		}
		sortFactsByNameThenFile(sources)
		carried[name] = sources
	}
	ground := newGrounding(store, memberFacts)
	resolve := newResolver(store, components, members, memberFacts, carried, ground)
	return unreachableRoles(store, rules, resolve, ground, unasked, unevaluable)
}
