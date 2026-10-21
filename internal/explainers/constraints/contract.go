package constraints

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
)

// RuleBinding is one rule that names a component containing the queried
// target, rendered for a reader deciding whether an edit is about to breach
// something: the id joins back to the declaring page, the statement is the
// rule in words, and because carries the rationale the rule was declared with.
type RuleBinding struct {
	Rule      string `json:"rule"`
	Statement string `json:"statement"`
	Because   string `json:"because"`
	Mode      string `json:"mode"`
	Source    string `json:"source,omitempty"`
	// NotChecked is why the loaded snapshot cannot evaluate the rule, in the
	// evaluator's own words. A rule listed without it is checked; one listed
	// with it enforces nothing here.
	NotChecked string             `json:"not_checked,omitempty"`
	Exemptions []ExemptionBinding `json:"exemptions,omitempty"`
}

type ExemptionBinding struct {
	Witness string `json:"witness"`
	Owner   string `json:"owner"`
	Because string `json:"because"`
	Since   string `json:"since"`
}

// ExemplarStatus is one guidance exemplar annotated against the current
// snapshot: present when a measured fact carries the exemplar as its exact
// name or its file path, absent otherwise — fail closed, so an exemplar the
// store cannot vouch for is never presented as existing prior art. When the
// store carries no measured facts at all — the declarations-only mode, no
// snapshot to measure against — every exemplar is unmeasured, because
// "absent" and "never looked" must never render the same.
type ExemplarStatus struct {
	Exemplar string `json:"exemplar"`
	Presence string `json:"presence"`
}

const (
	PresencePresent    = "present"
	PresenceAbsent     = "absent"
	PresenceUnmeasured = "unmeasured"
)

func (s ExemplarStatus) Label() string {
	if s.Presence == PresenceUnmeasured {
		return "unmeasured — no snapshot"
	}
	return s.Presence
}

// GuidanceBinding is one guidance rule naming a component that contains the
// queried target — the steering channel of the pre-edit contract. It carries
// the advice itself rather than a statement of law: the message, the mode
// that says how the advice travels (notify: contract only; advisory: plus one
// riding finding), and the exemplars with their presence annotated so a
// reader can go look at prior art that actually exists.
type GuidanceBinding struct {
	Rule      string           `json:"rule"`
	Message   string           `json:"message"`
	Mode      string           `json:"mode"`
	Because   string           `json:"because"`
	Source    string           `json:"source,omitempty"`
	Exemplars []ExemplarStatus `json:"exemplars,omitempty"`
}

// ComponentBinding is one declared component containing the queried target,
// with every rule that names it — law under Rules, steering under Guidance.
type ComponentBinding struct {
	Component string            `json:"component"`
	Source    string            `json:"source,omitempty"`
	Rules     []RuleBinding     `json:"rules"`
	Guidance  []GuidanceBinding `json:"guidance,omitempty"`
}

// ContractFor answers the pre-edit question: which declared components contain
// this target, and what do the rules binding them say. The target may be a
// file path (matched against component patterns with the same bounded dialect
// membership uses — so a file that does not exist yet still answers, which is
// the pre-edit point) or a measured fact name (contained when the fact's own
// file joins the component, under the component's kind and name narrowing).
// The second return reports whether any components are declared at all, so a
// caller can tell "nothing was asked" from "nothing binds this target".
func ContractFor(store *facts.Store, target string) ([]ComponentBinding, bool) {
	components, rules := declarations(store)
	if len(components) == 0 {
		return nil, false
	}

	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	sort.Strings(names)
	var measuredKnown, measuredValue bool
	measured := func() bool {
		if !measuredKnown {
			measuredKnown, measuredValue = true, storeMeasured(store)
		}
		return measuredValue
	}

	var out []ComponentBinding
	for _, name := range names {
		c := components[name]
		if !containsTarget(store, c, target) {
			continue
		}
		binding := ComponentBinding{Component: name, Source: c.source}
		for _, r := range rules {
			if !r.names(name) {
				continue
			}
			if r.guide != "" {
				binding.Guidance = append(binding.Guidance, GuidanceBinding{
					Rule:      r.id,
					Message:   r.message,
					Mode:      r.mode,
					Because:   r.because,
					Source:    r.source,
					Exemplars: exemplarStatuses(store, r.exemplars),
				})
				continue
			}
			binding.Rules = append(binding.Rules, RuleBinding{
				Rule:       r.id,
				Statement:  r.statement(),
				NotChecked: notChecked(r, store, components, measured),
				Because:    r.because,
				Mode:       r.mode,
				Source:     r.source,
				Exemptions: exemptionBindings(r.exempt),
			})
		}
		out = append(out, binding)
	}
	return out, true
}

// notChecked is the reason a rule's form cannot be evaluated over the loaded
// snapshot, "" when it can. Without measured facts every form is unchecked for
// the same reason, which the plan's snapshot note already says, so none is
// marked then. The reasons are the ones the evaluator's own finding gives.
func notChecked(r rule, store *facts.Store, components map[string]component, measured func() bool) string {
	if r.capRuntime == "" && r.independent == "" && r.requireConsumer == "" && r.uniqueAcross == "" || !measured() {
		return ""
	}
	switch {
	case r.capRuntime != "":
		frames := runtimeFrames(store)
		if len(frames) == 0 {
			return reasonNoRuntimeCapture
		}
		names, _ := resolveMembership(store, components[r.capRuntime])
		if !framesReach(frames, memberFilesOf(store, names)) {
			return reasonNoFrameInMembers
		}
	case r.independent != "":
		if !newResolvedAncestry(store).any() {
			return reasonNoResolvedAncestry
		}
	case r.requireConsumer != "":
		if len(store.RepoLabels()) < 2 {
			return reasonNoCounterparty
		}
	case r.uniqueAcross != "":
		_, members := resolveMembership(store, components[r.uniqueAcross])
		repos := map[string]bool{}
		for _, f := range members {
			repos[f.Repo] = true
		}
		if len(repos) < 2 {
			return reasonNoCounterparty
		}
	}
	return ""
}

func exemptionBindings(exempt []intent.ConstraintExemption) []ExemptionBinding {
	out := make([]ExemptionBinding, 0, len(exempt))
	for _, ex := range exempt {
		out = append(out, ExemptionBinding{Witness: ex.Witness, Owner: ex.Owner, Because: ex.Because, Since: ex.Since})
	}
	return out
}

// containsTarget reports whether a component contains the target: a raw path
// inside the component's patterns, or a measured fact whose name is exactly
// the target and whose membership (the same resolveMembership the explainer
// verdicts with, so contract and enforcement can never disagree) contains it.
// Both matches are exact — an unresolvable target is contained by nothing.
//
// A predicate component answers for a raw path only when the snapshot already
// carries a member in that file. The path arm exists so a file that does not
// exist yet still gets its contract, and that is precisely what a predicate
// cannot answer for: nothing has been measured about a file nobody has written.
// Fail closed — no contract is honest, a guessed one is not.
//
// The two arms are alternatives rather than a conjunction. Conjoining them made
// the predicate arm unreachable for the component the vocabulary exists to
// write: with no service and no match patterns pathInComponent is false, so
// `plan --paths app/components/x.rb` omitted every rule stated in the new
// vocabulary while `plan --symbols X` included it. A path scope, where one is
// declared, is already ANDed into the membership the predicate arm resolves.
func containsTarget(store *facts.Store, c component, target string) bool {
	names, members := resolveMembership(store, c)
	if names[target] {
		return true
	}
	if c.predicated() {
		return fileHostsMember(members, target)
	}
	return pathInComponent(c, target)
}

// fileHostsMember reports whether any measured member of the component lives in
// the named file — the evidence a predicate component needs before it claims a
// raw path.
func fileHostsMember(members []facts.Fact, target string) bool {
	for _, f := range members {
		if f.File == target {
			return true
		}
		if f.Repo != "" && strings.TrimPrefix(f.File, f.Repo+"/") == target {
			return true
		}
	}
	return false
}

// pathInComponent joins a raw path — which may not exist yet; that is the
// pre-edit point — against the component's selector. A service-scoped
// component contains only paths under its own repo prefix, matched in both
// the label-prefixed and repo-relative forms; with no match patterns the whole
// service qualifies. An unprefixed path cannot be attributed to a service:
// fail closed.
func pathInComponent(c component, target string) bool {
	if c.service == "" {
		return matchConstraintPath(target, c.match)
	}
	trimmed := strings.TrimPrefix(target, c.service+"/")
	if trimmed == target {
		return false
	}
	if len(c.match) == 0 {
		return true
	}
	return matchConstraintPath(target, c.match) || matchConstraintPath(trimmed, c.match)
}

// exemplarStatuses annotates a guidance rule's exemplars against the store,
// sorted so the contract renders them the same way on every run.
func exemplarStatuses(store *facts.Store, exemplars []string) []ExemplarStatus {
	sorted := append([]string(nil), exemplars...)
	sort.Strings(sorted)
	measured := storeMeasured(store)
	out := make([]ExemplarStatus, 0, len(sorted))
	for _, ex := range sorted {
		out = append(out, ExemplarStatus{Exemplar: ex, Presence: presenceOf(store, measured, ex)})
	}
	return out
}

func storeMeasured(store *facts.Store) bool {
	for _, f := range store.All() {
		if f.Kind != facts.KindIntent {
			return true
		}
	}
	return false
}

func presenceOf(store *facts.Store, measured bool, exemplar string) string {
	switch {
	case !measured:
		return PresenceUnmeasured
	case exemplarPresent(store, exemplar):
		return PresencePresent
	default:
		return PresenceAbsent
	}
}

// exemplarPresent is the deterministic existence check a guidance exemplar
// gets: some measured fact carries the exemplar as its exact name, or as its
// file path (joined in the label-prefixed and repo-relative forms, the same
// double join membership uses). Fail closed — an exemplar the store cannot
// resolve is absent, never assumed to have merely moved.
func exemplarPresent(store *facts.Store, exemplar string) bool {
	pattern := []string{exemplar}
	for _, f := range store.All() {
		if f.Name == exemplar || matchConstraintFile(f, pattern) {
			return true
		}
	}
	return false
}

// names reports whether the rule references the component on either side of
// its form.
func (r rule) names(component string) bool {
	if r.forbid == component || r.forbidReach == component || r.to == component || r.allow == component ||
		r.protect == component || r.private == component || r.forbidFact == component ||
		r.cap == component || r.require == component || r.requireDefines == component ||
		r.requireName == component || r.requireEdge == component || r.protocol == component ||
		r.guide == component || r.forbidName == component || r.forbidCycles == component ||
		r.independent == component || r.storageStaysHome == component || r.capRuntime == component ||
		r.requireConsumer == component || r.uniqueAcross == component || r.requireGoverned == component {
		return true
	}
	for _, name := range append(append(append(append(append([]string{}, r.only...), r.owners...), r.except...), r.steps...), r.among...) {
		if name == component {
			return true
		}
	}
	return false
}

// statement renders the rule in words — the same shape for every form, so a
// reader (or an agent about to edit) needs no schema knowledge to obey it.
func (r rule) statement() string {
	switch {
	case r.forbid != "" && len(r.toName) > 0 && r.receiver == "none":
		return fmt.Sprintf("%s must not reach %s via %s, called with no receiver", r.forbid, forbidFarEnd(r), r.via)
	case r.forbid != "":
		return fmt.Sprintf("%s must not reach %s via %s", r.forbid, forbidFarEnd(r), r.via)
	case r.forbidReach != "":
		vias := r.via
		if vias == "" {
			vias = strings.Join(reachVias, ", ")
		}
		return fmt.Sprintf("%s must not reach %s through any path over %s", r.forbidReach, r.to, vias)
	case r.allow != "":
		return fmt.Sprintf("%s may reach only %s via %s", r.allow, strings.Join(r.only, ", "), r.via)
	case r.protect != "":
		return fmt.Sprintf("only %s may reach %s via %s", strings.Join(r.owners, ", "), r.protect, r.via)
	case r.private != "":
		if len(r.except) > 0 {
			return fmt.Sprintf("non-exported members of %s are reachable only from inside it or from %s", r.private, strings.Join(r.except, ", "))
		}
		return fmt.Sprintf("non-exported members of %s are reachable only from inside it", r.private)
	case r.forbidFact != "":
		return fmt.Sprintf("%s must have no members", r.forbidFact)
	case r.cap != "":
		return fmt.Sprintf("%s must not exceed %d members", r.cap, r.maxMembers)
	case r.require != "":
		return fmt.Sprintf("members of %s%s must have %s containing %s",
			r.require, requireScope(r), r.mustProp, r.mustValue)
	case r.requireDefines != "":
		return fmt.Sprintf("class members of %s must define %s", r.requireDefines, r.method)
	case r.requireName != "":
		return fmt.Sprintf("members of %s must be named like %s", r.requireName, r.pattern)
	case r.forbidName != "":
		scope := "members"
		if r.surface == "exported" {
			scope = "exported members"
		}
		return fmt.Sprintf("%s of %s must not be named like %s", scope, r.forbidName, r.pattern)
	case r.forbidCycles != "":
		return fmt.Sprintf("%s must not depend on each other in a circle", strings.Join(append([]string{r.forbidCycles}, r.among...), ", "))
	case r.independent != "":
		return fmt.Sprintf("%s must not reach the classes that include it", r.independent)
	case r.storageStaysHome != "":
		return fmt.Sprintf("%s must reach only the tables it owns", r.storageStaysHome)
	case r.capRuntime != "":
		return fmt.Sprintf("each frame of %s must stay within %d %s", r.capRuntime, r.max, r.metric)
	case r.requireConsumer != "":
		return fmt.Sprintf("every route of %s must have a client", r.requireConsumer)
	case r.uniqueAcross != "":
		return fmt.Sprintf("no two members of %s in different repositories may share a %s", r.uniqueAcross, r.by)
	case r.requireGoverned != "":
		return fmt.Sprintf("every file of %s must be governed by a compiled page", r.requireGoverned)
	case r.requireEdge != "":
		switch {
		case r.direction == "inbound" && r.to != "":
			return fmt.Sprintf("members of %s must have an inbound %s edge from %s", r.requireEdge, r.via, r.to)
		case r.direction == "inbound":
			return fmt.Sprintf("members of %s must have an inbound %s edge", r.requireEdge, r.via)
		case r.to != "":
			return fmt.Sprintf("members of %s must have an outbound %s edge into %s", r.requireEdge, r.via, r.to)
		default:
			return fmt.Sprintf("members of %s must have an outbound %s edge", r.requireEdge, r.via)
		}
	case r.protocol != "" && len(r.steps) > 1:
		prerequisites := make([]string, 0, len(r.steps)-1)
		for i := len(r.steps) - 2; i >= 0; i-- {
			prerequisites = append(prerequisites, r.steps[i])
		}
		return fmt.Sprintf("members of %s that reach %s via %s must also reach %s, in the declared order of obligation — structural conformance, not runtime ordering", r.protocol, r.steps[len(r.steps)-1], r.via, strings.Join(prerequisites, ", "))
	}
	return ""
}

// ComponentCount is one declared component's resolution against a measured
// store: how many facts its selector actually names, and the selector it named
// them with — a predicate count with no predicate beside it leaves the author
// guessing which of several narrowings produced the number.
type ComponentCount struct {
	Component string `json:"component"`
	Members   int    `json:"members"`
	Selector  string `json:"selector,omitempty"`
}

// MemberCounts resolves every declared component in the store against the
// measured facts and reports its membership size, in component-name order —
// the authoring loop's answer to "does my selector select anything", using
// exactly the membership the explainer verdicts with so lint and enforcement
// can never disagree about what a component contains.
func MemberCounts(store *facts.Store) []ComponentCount {
	components, _ := declarations(store)
	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []ComponentCount
	for _, name := range names {
		memberNames, _ := resolveMembership(store, components[name])
		out = append(out, ComponentCount{
			Component: name,
			Members:   len(memberNames),
			Selector:  selectorSummary(components[name]),
		})
	}
	return out
}

// ExemplarNote is one guidance exemplar the current snapshot cannot resolve —
// a lint note, never an error: prior art moving out from under its exemplar
// makes the advice weaker, not the declaration invalid.
type ExemplarNote struct {
	Rule     string `json:"rule"`
	Exemplar string `json:"exemplar"`
}

// AbsentExemplars resolves every guidance rule's exemplars against the
// measured store and reports the absent ones, in rule-id then exemplar order —
// `constraints lint`'s authoring-loop answer to "does the prior art I am
// pointing at still exist", using exactly the existence check the contract
// annotates with so lint and delivery can never disagree.
func AbsentExemplars(store *facts.Store) []ExemplarNote {
	_, rules := declarations(store)
	var out []ExemplarNote
	for _, r := range rules {
		if r.guide == "" {
			continue
		}
		sorted := append([]string(nil), r.exemplars...)
		sort.Strings(sorted)
		for _, ex := range sorted {
			if !exemplarPresent(store, ex) {
				out = append(out, ExemplarNote{Rule: r.id, Exemplar: ex})
			}
		}
	}
	return out
}

type ChangedFile struct {
	Path string
	Repo string
}

type GuidanceMatch struct {
	Rule         string           `json:"rule"`
	Component    string           `json:"component"`
	Message      string           `json:"message"`
	Mode         string           `json:"mode"`
	Because      string           `json:"because"`
	Source       string           `json:"source,omitempty"`
	Exemplars    []ExemplarStatus `json:"exemplars,omitempty"`
	MatchedFiles []string         `json:"matched_files"`
}

func GuidanceForFiles(store *facts.Store, changed []ChangedFile) []GuidanceMatch {
	if len(changed) == 0 {
		return nil
	}
	components, rules := declarations(store)
	if len(components) == 0 {
		return nil
	}
	var out []GuidanceMatch
	for _, r := range rules {
		if r.guide == "" {
			continue
		}
		c, declared := components[r.guide]
		if !declared {
			continue
		}
		matched := map[string]bool{}
		for _, f := range changed {
			if fileInComponent(c, f.Path, f.Repo) {
				matched[f.Path] = true
			}
		}
		if len(matched) == 0 {
			continue
		}
		out = append(out, GuidanceMatch{
			Rule:         r.id,
			Component:    r.guide,
			Message:      r.message,
			Mode:         r.mode,
			Because:      r.because,
			Source:       r.source,
			Exemplars:    exemplarStatuses(store, r.exemplars),
			MatchedFiles: sortedMemberNames(matched),
		})
	}
	return out
}

func fileInComponent(c component, path, repo string) bool {
	if c.service != "" {
		switch {
		case repo != "" && repo != c.service:
			return false
		case repo == "" && !strings.HasPrefix(path, c.service+"/"):
			return false
		}
		if len(c.match) == 0 {
			return true
		}
	}
	if matchConstraintPath(path, c.match) {
		return true
	}
	prefix := repo
	if prefix == "" {
		prefix = c.service
	}
	if prefix != "" {
		if trimmed := strings.TrimPrefix(path, prefix+"/"); trimmed != path {
			return matchConstraintPath(trimmed, c.match)
		}
	}
	return false
}

// ViolationsReferencing filters constraint insights to those whose evidence
// names the target exactly — as the file, the source symbol, or the fact an
// edge lands on. Exact, like everything else here: a violation that merely
// mentions a related path is not evidence about this target.
func ViolationsReferencing(insights []facts.Insight, target string) []facts.Insight {
	source := New().Name()
	var out []facts.Insight
	for _, in := range insights {
		if in.Source != source {
			continue
		}
		for _, ev := range in.Evidence {
			if ev.File == target || ev.Symbol == target || ev.Fact == target {
				out = append(out, in)
				break
			}
		}
	}
	return out
}
