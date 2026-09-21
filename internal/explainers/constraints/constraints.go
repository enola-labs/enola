// Package constraints verdicts declared constraint rules against the measured
// graph — the enforcement half of the components/rules vocabulary pages
// declare. A component resolves to the measured facts its match patterns
// select (the same bounded path dialect declared layers use); a rule states
// one of the enforceable forms in intent.RuleForms — twenty-one of them, and
// that table is the count's only source: forbid (one component must
// not reach another via a named edge kind), forbid_reach (one component must
// not reach another through ANY measured path of rule-via edges — the
// transitive form, a bounded breadth-first walk whose shortest witness path
// each violation renders, degrading to one 0.4 advisory when a membership is
// too large to walk), allow-only (a component's edges
// may land only in the named components), protect (only the named owner
// components may reach this one — walked from the whole graph, since the
// unknown is the source), private (the component's non-exported members may
// be reached only from inside it or from the except components — the
// visibility form, walked from the whole graph over every rule-via edge kind
// against the extractor-measured exported prop), forbid-fact (a component's
// membership must be empty), cap (a component's membership must not exceed a
// declared count), require (members matching an optional when-clause must
// carry a named prop value — verdicting what a fact carries rather than what
// edges it makes), and require-defines (every class-kind member symbol must
// have a measured method symbol of the declared name — the protocol form; a
// class that inherits, includes or extends anything is out of scope, fail
// closed, because the definition could ride composition the check cannot see
// through), and require-name (every member fact's name must match a bounded
// convention pattern — prefix*, *suffix, or exact, never a general glob or
// regex), and require-edge (every member must have at least one measured edge
// of the declared via kind, inbound or outbound, optionally scoped to a
// counterpart component — the existential form: where every other edge form
// forbids an edge, this one demands one exists, so an orphaned event or an
// unconsumed route is a breach instead of invisible; a member whose edge
// visibility the snapshot cannot demonstrate for that file kind is skipped
// with a named count, fail closed), and protocol (members of one component
// conform to an ordered list of step components: a member making a via edge
// into step K's members must also make via edges into every step 1..K-1's
// members — the ordered form. It verdicts STRUCTURAL protocol conformance,
// which a static graph can decide — a caller that structurally skips a
// mandatory step — and never runtime ordering, which it cannot: that step 1
// executes before step 2 is a claim only a runtime-observed sequence could
// back, so the compiled rule carries verification: structural and the
// observed level stays future runtime-provider work. A member touching no
// step is a bystander the rule does not bind; a member whose file class
// cannot demonstrate the via kind is skipped with a named count, fail
// closed). A further form, guide, is steering rather than law: advice for
// whoever is about to edit inside the component, delivered through the
// pre-edit contract (ContractFor) with each exemplar checked present or
// absent against the snapshot. In notify mode (its default) a guidance rule
// emits no finding ever; in advisory mode it emits ONE 0.9 finding per
// guided component — never one per member, because guidance is not a
// violation census — so it rides check output visibly and can fail nothing.
//
// Eight further forms state laws only a graph can settle: forbid_cycles/among
// (the named parts may not depend on each other in a circle), independent (no
// module in the component reaches a class whose resolved ancestry includes
// it), forbid_name (the negative of require_name), storage_stays_home (every
// storage fact a member reaches is itself a member), cap_runtime (a
// runtime-observed metric per member frame stays under a budget),
// require_consumer (every member route has a client in the snapshot),
// unique_across (no two members in different repositories share the named
// property), and require_governed (every member file carries an anchor from a
// compiled page).
//
// Everything here is exact and fail-closed: membership is path equality or a
// declared subtree, target resolution is exact fact-name match — an edge whose
// target string does not name a member is no violation, never a guess — and a
// violation is a decided-rule breach at confidence 1.0, because the rule was
// stated and the edge is measured. A rule declared advisory reports its
// breaches at a confidence below the check gate's floor, so the finding
// surfaces without failing anything. The one advisory this package emits on
// its own is the zero-member component note, so a selector that matches
// nothing is visible instead of silently satisfying every rule that names it.
package constraints

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
)

// emptyComponentConfidence caps the dead-selector advisory: a component that
// matches nothing may be a moved tree or a selector wrong from the start, and
// this finding cannot tell which — it exists so that vacuous compliance never
// reads as compliance.
const emptyComponentConfidence = 0.4

// absentServiceConfidence caps the unasked-component advisory, the
// counterparty rule's visible trace: a component naming a service absent from
// the snapshot may point at a repo left out of the append or at a mistyped
// label, and this finding cannot tell which — it exists, like the dead-selector
// advisory, so that unasked silence never reads as compliance.
const absentServiceConfidence = 0.4

// reachDepthCap bounds the forbid_reach walk: a path longer than this many
// edges is not searched for. The cap is part of the rule's meaning, stated in
// the skip advisory and here, so a distant reachability nobody can act on
// never costs an unbounded traversal.
const reachDepthCap = 12

// reachComponentCap bounds the forbid_reach memberships: a component larger
// than this on either side of the rule degrades the whole rule to one skip
// advisory instead of walking — the honest degrade, visible rather than slow.
const reachComponentCap = 500

// reachSkipConfidence caps the too-large-to-walk advisory, for the same reason
// the dead-selector advisory sits at 0.4: the finding reports that no verdict
// was reached, and silence there must never read as compliance.
const reachSkipConfidence = 0.4

// advisoryConfidence is what a mode: advisory rule's breaches report at —
// deliberately below check's default 1.0 floor, so an advisory rule surfaces
// in every insight listing and fails no gate. The breach itself is as decided
// as a ratchet rule's; the declaring page chose reporting over enforcement.
const advisoryConfidence = 0.9

const exemptedConfidence = 0.9

const deadExemptionConfidence = 0.4

// edgeSkipConfidence caps the require_edge unmeasurable-member advisory, for
// the same reason the reach-skip advisory sits at 0.4: it reports that no
// verdict was reached for the skipped members, and silence there must never
// read as compliance.
const edgeSkipConfidence = 0.4

const protocolSkipConfidence = 0.4

// requireSkipConfidence caps the require form's zero-edge advisory, for the
// same reason every other skip advisory sits at 0.4: it reports that the edge
// antecedent could select nobody, and that silence must never read as
// compliance.
const requireSkipConfidence = 0.4

// memberKinds are the measured fact kinds a component selector ranges over when
// it does not name one.
var memberKinds = []string{facts.KindModule, facts.KindSymbol, facts.KindRoute, facts.KindStorage}

// referenceMemberKinds are kinds a component may select only by naming them —
// each for its own reason, all with the same consequence.
//
// test_ref and file_ref carry reference edges rather than architectural
// coupling: a test file referencing a production symbol must not make that
// symbol look used by production, so the explainers that count dependents
// exclude them, and a component that did not ask for them must not acquire
// them either. lint carries a linter's report. dependency carries either an
// import edge, whose home is the importing file rather than any component, or
// an external package — a thing this repository DECLARES rather than contains,
// which no path-shaped component was ever written to hold.
//
// A rule about tests is the case that first needed this: "a component test must
// not reach a fixture factory" is an ordinary edge rule whose near end is a
// test file, and while no component could select one the rule could not be
// written at all. Supply-chain rules are the newest: `kind: dependency` with a
// `where: {type: package, pinned: false}` selects exactly the declared packages
// that name no single version. Naming the kind is the whole opt-in: a
// declaration that omits `kind:` ranges over memberKinds exactly as before.
var referenceMemberKinds = []string{facts.KindTestRef, facts.KindFileRef, facts.KindLint, facts.KindDependency}

// reachVias are the edge kinds the private form walks — the same closed
// vocabulary a rule's via may name, kept as a sorted slice here because the
// form verdicts every kind at once and the walk order must be deterministic.
// Widening the rule-via vocabulary widens this list with it, deliberately in
// the same change.
var reachVias = []string{facts.RelCalls, facts.RelDependsOn, facts.RelImplements, facts.RelImports}

// Explainer verdicts constraint-rule intent facts against measured edges.
type Explainer struct{}

// New creates the explainer.
func New() *Explainer { return &Explainer{} }

// Name returns the explainer identifier; `check --fail-on=constraints`
// selects it by this name.
func (e *Explainer) Name() string { return "constraints" }

type component struct {
	name        string
	service     string
	match       []string
	kind        string
	namePattern string
	where       []intent.WherePair
	owns        string
	ancestor    string
	public      []string
	handles     []string
	governedBy  string
	source      string
	recipe      string
	instance    string
	role        string
}

// predicated reports whether the component selects by what facts carry rather
// than only by where they sit. It is the switch on every reading that differs
// between the two: a path component's file patterns are a claim about a whole
// file, and a predicate is a claim about one measured fact.
func (c component) predicated() bool {
	return len(c.where) > 0 || c.ancestor != "" || len(c.handles) > 0 || c.governedBy != ""
}

type rule struct {
	id, because, source, mode string
	recipe, instance          string
	forbid, to                string
	forbidReach               string
	allow                     string
	only                      []string
	protect                   string
	owners                    []string
	private                   string
	except                    []string
	forbidFact                string
	cap                       string
	maxMembers                int
	require                   string
	whenProp, whenValue       string
	whenEdgeTo                []string
	mustProp, mustValue       string
	requireDefines, method    string
	anyOf                     []string
	forbidCycles              string
	among                     []string
	independent               string
	requireName, pattern      string
	requires, receiver        string
	forbidName, surface       string
	requireEdge, direction    string
	whenVia                   string
	toName                    []string
	protocol                  string
	steps                     []string
	guide, message            string
	exemplars                 []string
	via                       string
	storageStaysHome          string
	capRuntime, metric        string
	max                       int
	requireConsumer           string
	uniqueAcross, by          string
	requireGoverned           string
	since                     string
	growth                    int
	owns                      map[string]string
	exempt                    []intent.ConstraintExemption
}

func (r rule) advisory() bool { return r.mode == "advisory" }

func (r rule) strict() bool { return r.mode == "strict" }

func (r rule) confidence() float64 {
	if r.advisory() {
		return advisoryConfidence
	}
	return 1.0
}

// titled prefixes a violation title with the rule's enforcement weight, so an
// advisory finding can never be mistaken for a gating one in a listing — and a
// strict one is recognizable to the check gate, which exempts findings carrying
// check.StrictConstraintTitlePrefix from delta scoping: a strict breach fails
// even when the baseline already carried it.
func (r rule) titled(rest string) string {
	switch {
	case r.advisory():
		return fmt.Sprintf("Advisory constraint %s violated: %s", r.id, rest)
	case r.strict():
		return fmt.Sprintf("Strict constraint %s violated: %s", r.id, rest)
	default:
		return fmt.Sprintf("Constraint %s violated: %s", r.id, rest)
	}
}

// declarations reads the compiled component and rule intent facts back into
// their evaluable shapes — shared between the explainer's verdict pass and the
// contract query, so the two can never read a declaration differently.
func declarations(store *facts.Store) (map[string]component, []rule) {
	components := map[string]component{}
	var rules []rule
	for _, f := range store.ByKind(facts.KindIntent) {
		switch f.PropString("intent_kind") {
		case "component":
			c := decodeComponent(f)
			// First declaration wins, which is what the declaration screen's own
			// index does. A duplicate component name is refused there, so a
			// second fact for one name reaches this loop only from a store the
			// screen never passed — and the answer it gets must still be the
			// screen's, or a declaration compiles under one reading and verdicts
			// under the other.
			if _, seen := components[c.name]; !seen {
				components[c.name] = c
			}
		case "rule":
			rules = append(rules, decodeRule(f))
		}
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].id < rules[j].id })
	return components, rules
}

// decodeComponent reads one compiled component fact back into the selector
// the evaluator resolves.
func decodeComponent(f facts.Fact) component {
	c := component{
		name:        f.PropString("component"),
		service:     f.PropString("service"),
		match:       strings.Fields(f.PropString("match")),
		kind:        f.PropString("kind"),
		namePattern: f.PropString("name_pattern"),
		where:       intent.DecodeWhere(f.PropString("where")),
		owns:        f.PropString("owns"),
		ancestor:    f.PropString("ancestor"),
		public:      strings.Fields(f.PropString("public")),
		handles:     strings.Fields(f.PropString("handles")),
		governedBy:  f.PropString("governed_by"),
		source:      f.PropString("source"),
		recipe:      f.PropString("recipe"),
		instance:    f.PropString("instance"),
		role:        f.PropString("role"),
	}
	return c
}

// decodeRule reads one compiled rule fact back into the rule the evaluator
// verdicts.
func decodeRule(f facts.Fact) rule {
	r := rule{
		id:               f.PropString("rule"),
		mode:             f.PropString("mode"),
		recipe:           f.PropString("recipe"),
		instance:         f.PropString("instance"),
		because:          f.PropString("because"),
		source:           f.PropString("source"),
		forbid:           f.PropString("forbid"),
		forbidReach:      f.PropString("forbid_reach"),
		to:               f.PropString("to"),
		allow:            f.PropString("allow"),
		only:             strings.Fields(f.PropString("only")),
		protect:          f.PropString("protect"),
		owners:           strings.Fields(f.PropString("owners")),
		private:          f.PropString("private"),
		except:           strings.Fields(f.PropString("except")),
		forbidFact:       f.PropString("forbid_fact"),
		cap:              f.PropString("cap"),
		require:          f.PropString("require"),
		whenProp:         f.PropString("when_prop"),
		whenValue:        f.PropString("when_value"),
		whenEdgeTo:       strings.Fields(f.PropString("when_edge_to")),
		mustProp:         f.PropString("must_prop"),
		mustValue:        f.PropString("must_value"),
		requireDefines:   f.PropString("require_defines"),
		anyOf:            strings.Fields(f.PropString("any_of")),
		forbidCycles:     f.PropString("forbid_cycles"),
		among:            strings.Fields(f.PropString("among")),
		independent:      f.PropString("independent"),
		method:           f.PropString("method"),
		requireName:      f.PropString("require_name"),
		forbidName:       f.PropString("forbid_name"),
		surface:          f.PropString("surface"),
		pattern:          f.PropString("pattern"),
		requireEdge:      f.PropString("require_edge"),
		direction:        f.PropString("direction"),
		whenVia:          f.PropString("when_via"),
		toName:           strings.Fields(f.PropString("to_name")),
		receiver:         f.PropString("receiver"),
		requires:         f.PropString("requires"),
		protocol:         f.PropString("protocol"),
		steps:            strings.Fields(f.PropString("steps")),
		guide:            f.PropString("guide"),
		message:          f.PropString("message"),
		exemplars:        strings.Fields(f.PropString("exemplars")),
		via:              f.PropString("via"),
		owns:             intent.DecodeOwnership(f.PropString("owns")),
		exempt:           intent.DecodeExemptions(f.PropString("exempt")),
		storageStaysHome: f.PropString("storage_stays_home"),
		capRuntime:       f.PropString("cap_runtime"),
		metric:           f.PropString("metric"),
		requireConsumer:  f.PropString("require_consumer"),
		uniqueAcross:     f.PropString("unique_across"),
		by:               f.PropString("by"),
		requireGoverned:  f.PropString("require_governed"),
		since:            f.PropString("since"),
	}
	if n, ok := intPropOf(f, "max"); ok {
		r.max = n
	}
	if n, ok := intPropOf(f, "growth"); ok {
		r.growth = n
	}
	if n, ok := intPropOf(f, "max_members"); ok {
		r.maxMembers = n
	}
	return r
}

// Explain resolves each declared component to its member facts, then emits one
// proof-class violation per rule breach, plus one advisory per component whose
// selector matched nothing.
func (e *Explainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	ev := e.evaluate(store, evaluation{})
	return ev.insights, nil
}

// evaluation is one pass of the constraints explainer over a store, with two
// knobs the verdict pass never uses: a membership exclusion, so a radius can
// ask what the rules say once a file's facts belong to no part, and a guard
// that turns a rule's panic into a named "not computed" entry instead of a
// crash, so a comparison can never read a rule that did not run as unaffected.
type evaluation struct {
	exclude func(facts.Fact) bool
	guard   bool
}

// evaluated is what one pass produced: every insight, the rule verdicts keyed
// by rule id, and the rules that could not be computed.
type evaluated struct {
	insights    []facts.Insight
	byRule      map[string][]facts.Insight
	rules       []rule
	notComputed []NotComputed
}

func (e *Explainer) evaluate(store *facts.Store, opts evaluation) evaluated {
	components, rules := declarations(store)
	out := evaluated{byRule: map[string][]facts.Insight{}, rules: rules}
	if len(components) == 0 {
		return out
	}

	names := make([]string, 0, len(components))
	for n := range components {
		names = append(names, n)
	}
	sort.Strings(names)

	// The counterparty rule, inherited from intentcheck: a component naming a
	// service no loaded fact carries the label of is UNASKED — the snapshot
	// cannot answer for a repo it does not contain, so every rule naming the
	// component emits nothing, and the absent-service advisory below is the
	// only trace. "Matches 0 because the repo is not loaded" must never verdict
	// as anything.
	unasked := unaskedComponents(store, components)

	// The predicate equivalent of the counterparty rule: a where this snapshot
	// cannot answer — a property nothing measures, a threshold against a
	// property that is never a number, a compiled field that is no test at all
	// — leaves the component UNEVALUABLE and every rule naming it emits
	// nothing. Silence here is the fail-closed reading: an empty membership
	// would make every rule over it hold, and a rule that holds because its
	// selector is broken is the one failure this vocabulary must never produce.
	// The loud finding below is the trace. Unasked wins over unevaluable — a
	// repo that was never loaded is a different silence, reported as its own.
	unevaluables := unevaluableSelectors(store, components, unasked)
	unevaluable := map[string]bool{}
	for _, u := range unevaluables {
		unevaluable[u.Component] = true
	}

	// Membership is a set of canonical fact NAMES, because a name is the only
	// thing an edge's target string can be matched against exactly; the facts
	// themselves are kept alongside, because they carry the edges to walk and
	// the file each violation is evidenced with.
	members := map[string]map[string]bool{}
	memberFacts := map[string][]facts.Fact{}
	for _, name := range names {
		members[name], memberFacts[name] = resolveMembership(store, components[name])
		if opts.exclude != nil {
			members[name], memberFacts[name] = withoutExcluded(members[name], memberFacts[name], opts.exclude)
		}
	}

	// Imports edges do not ride the member facts: extractors carry them on
	// KindDependency facts whose File is the importing file. A carrier is not a
	// member — membership is unchanged — but its file joining a component's
	// patterns is the same exact match membership itself uses, so the edges it
	// carries are walked as sourced from that component. A component narrowed
	// to one exact fact name gets no carriers: a file-level join would attribute
	// every edge in the file to the single fact the author named.
	carriers := map[string][]facts.Fact{}
	for _, name := range names {
		c := components[name]
		for _, f := range store.ByKind(facts.KindDependency) {
			if carrierFor(f, c) && (opts.exclude == nil || !opts.exclude(f)) {
				carriers[name] = append(carriers[name], f)
			}
		}
		sortFactsByNameThenFile(carriers[name])
	}

	// Everything a rule may walk edges FROM before ownership is read: the
	// members themselves and the dependency facts carrying their files' edges.
	// What a member ENCLOSES is added per rule by the resolver, because a rule
	// may override what a component owns for its own reach.
	carried := map[string][]facts.Fact{}
	for _, name := range names {
		sources := append(append([]facts.Fact{}, memberFacts[name]...), carriers[name]...)
		sortFactsByNameThenFile(sources)
		carried[name] = sources
	}

	// What each repository measured, indexed once: the subordinate fallback every
	// target site below reaches for after exact-name membership has failed. It
	// carries the memberships too, because a predicate component's file join is
	// only as wide as the files it measured a member in.
	ground := newGrounding(store, memberFacts)

	// One resolver, asked one question per role: how the fact that MADE an edge
	// resolves onto a component, or how the fact the edge LANDED ON does, never
	// both of a single role. It reads the declared ownership through the single
	// statement of precedence, so a rule's reach is the rule's own.
	resolve := newResolver(store, components, members, memberFacts, carried, ground)

	// Which files the snapshot measured exported content in — the private form's
	// file-granular test, asked of the whole store rather than of one component's
	// members, so a narrowed membership cannot make a file look wholly internal.
	exportedFiles := map[string]bool{}
	for _, r := range rules {
		if r.private == "" {
			continue
		}
		for _, f := range store.FactsRef() {
			if visible, ok := f.PropAny("exported").(bool); ok && visible && f.File != "" {
				exportedFiles[f.File] = true
			}
		}
		break
	}

	// Allow-only needs to tell "lands in a component the rule does not allow"
	// from "does not resolve to anything measured" — only the former is a
	// breach. The resolvable set is every canonical member-kind fact name,
	// plus the service nodes: a service-to-service edge targets a repo label,
	// and skipping it as unresolvable would blind allow-only to every
	// cross-repo landing.
	resolvable := map[string]bool{}
	for _, r := range rules {
		if r.allow == "" {
			continue
		}
		for _, kind := range append(append([]string{}, memberKinds...), facts.KindService) {
			for _, f := range store.ByKind(kind) {
				resolvable[f.Name] = true
			}
		}
		break
	}

	// Protect and private walk in the reverse direction — the unknown is the
	// source, so the walk covers every edge-carrying fact in the store, not
	// one component's members: the member kinds, the dependency carriers, and
	// the service nodes whose relations are the cross-repo edges. Forbid_reach
	// needs the same whole-graph set for its forward walk: an intermediate hop
	// may live in no declared component at all. Require_edge needs it twice
	// over: an unscoped inbound rule searches every edge-carrying fact for the
	// demanded edge, and the extraction census that decides measurability is a
	// whole-store question in every direction. Sorted for the same reason
	// memberships are.
	var graphWalk []facts.Fact
	for _, r := range rules {
		if r.protect == "" && r.private == "" && r.forbidReach == "" && r.requireEdge == "" && r.protocol == "" {
			continue
		}
		for _, kind := range append(append([]string{}, memberKinds...), facts.KindDependency, facts.KindService) {
			graphWalk = append(graphWalk, store.ByKind(kind)...)
		}
		sort.Slice(graphWalk, func(i, j int) bool {
			if graphWalk[i].Name != graphWalk[j].Name {
				return graphWalk[i].Name < graphWalk[j].Name
			}
			return graphWalk[i].File < graphWalk[j].File
		})
		break
	}

	// Require-defines resolves definitions against the whole symbol store —
	// the method may live in another file of the class — and needs every name
	// that composes behavior in (mixin dependency carriers name their source
	// scope; superclass edges ride the class symbol itself), because a class
	// composing anything is out of the form's scope, fail closed.
	definedNames := map[string]bool{}
	composed := map[string]bool{}
	for _, r := range rules {
		if r.requireDefines == "" {
			continue
		}
		for _, f := range store.ByKind(facts.KindSymbol) {
			definedNames[f.Name] = true
			for _, rel := range f.Relations {
				if rel.Kind == facts.RelImplements {
					composed[f.Name] = true
				}
			}
		}
		for _, f := range store.ByKind(facts.KindDependency) {
			for _, rel := range f.Relations {
				if rel.Kind != facts.RelImplements {
					continue
				}
				if source, ok := strings.CutSuffix(f.Name, " -> "+rel.Target); ok {
					composed[source] = true
				}
			}
		}
		break
	}

	var census map[string]map[string]bool
	for _, r := range rules {
		if r.requireEdge != "" || r.protocol != "" {
			census = edgeClassCensus(graphWalk)
			break
		}
	}

	// Which roles resolve nothing on the side they use. A rule holding one of
	// them emits no verdict: the declaration screen refuses what this vocabulary
	// can never state a basis for, and this refuses what this SNAPSHOT cannot.
	unreachable := map[string][]UnreachableRole{}
	refused := map[string]bool{}
	for _, u := range unreachableRoles(store, rules, resolve, ground, unasked, unevaluable) {
		unreachable[u.Rule] = append(unreachable[u.Rule], u)
		if !u.Partial {
			refused[u.Rule] = true
		}
	}

	var insights []facts.Insight
	for _, r := range rules {
		if namesUnasked(r, unasked) || namesUnasked(r, unevaluable) {
			continue
		}
		for _, u := range unreachable[r.id] {
			insights = append(insights, unreachableRoleInsight(u, components[u.Component], r))
		}
		if refused[r.id] {
			continue
		}
		verdicts, cause := e.verdictsFor(r, opts.guard, func() []facts.Insight {
			return e.verdictsOf(r, store, graphWalk, resolve, ground, resolvable, members, memberFacts, carriers, carried, exportedFiles, definedNames, composed, census)
		})
		if cause != "" {
			out.notComputed = append(out.notComputed, NotComputed{Rule: r.id, Cause: cause})
			continue
		}
		if r.since != "" {
			verdicts = stampSince(r, verdicts)
		}
		decided := exemptVerdicts(r, verdicts)
		if r.recipe != "" {
			for i := range decided {
				decided[i].Description += fmt.Sprintf(" This verdict traces to rule %s (recipe %s, instantiated in %s).", r.id, r.recipe, r.source)
			}
		}
		out.byRule[r.id] = append(out.byRule[r.id], decided...)
		insights = append(insights, decided...)
	}

	for _, u := range unevaluables {
		insights = append(insights, unevaluableSelectorInsight(u, components[u.Component]))
	}

	insights = append(insights, oneLevelSuperclassAdvisories(store, names, components, members, unasked, unevaluable)...)

	for _, name := range names {
		c := components[name]
		if unasked[name] {
			insights = append(insights, facts.Insight{
				Title:       fmt.Sprintf("Constraint component %s names service %s not present in this snapshot", name, c.service),
				Description: fmt.Sprintf("No loaded fact carries the repo label %s, so the snapshot cannot answer for the service and every rule naming %s emitted no verdict — unasked, never failed. Either the repo was left out of this multi-repo snapshot or the label is wrong; this advisory exists so that silence cannot be read as compliance.%s", c.service, name, componentRecipeProvenance(c)),
				Confidence:  absentServiceConfidence,
				Evidence:    []facts.Evidence{{Fact: "component: " + name, Detail: "declared in " + c.source}},
				Actions: []string{
					"Append the missing repo to the snapshot if the rule should verdict here",
					"Fix the service label on the declaring file if it names the wrong repo",
				},
			})
			continue
		}
		if unevaluable[name] {
			continue
		}
		if len(members[name]) > 0 {
			continue
		}
		insights = append(insights, facts.Insight{
			Title:       fmt.Sprintf("Constraint component %s matches nothing", name),
			Description: fmt.Sprintf("The component's selector (%s) selects no measured fact, so every rule naming it holds vacuously — a dead selector enforcing nothing. Either the code moved out from under the selector or it never matched; this advisory exists so that silence cannot be read as compliance.%s", selectorSummary(c), componentRecipeProvenance(c)),
			Confidence:  emptyComponentConfidence,
			Evidence:    []facts.Evidence{{Fact: "component: " + name, Detail: "declared in " + c.source}},
			Actions: []string{
				"Fix the match patterns if the code moved",
				"Remove the component and the rules naming it if the decision is retired",
			},
		})
	}

	sort.Slice(insights, func(i, j int) bool { return insights[i].Title < insights[j].Title })
	out.insights = insights
	return out
}

// verdictsFor runs one rule's verdicts. Under a guard a panic becomes the
// rule's cause and no verdict, so the caller lists the rule as not computed
// rather than silently unaffected; without a guard the panic propagates as it
// always has, because a verdict pass that crashes must not read as clean.
func (e *Explainer) verdictsFor(r rule, guard bool, run func() []facts.Insight) (verdicts []facts.Insight, cause string) {
	if !guard {
		return run(), ""
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			verdicts, cause = nil, fmt.Sprint(recovered)
		}
	}()
	return run(), ""
}

// verdictSeam runs before a rule's verdicts when set; it exists so a test can
// make one rule fail and prove the radius names it instead of staying silent.
var verdictSeam func(r rule)

func (e *Explainer) verdictsOf(r rule, store *facts.Store, graphWalk []facts.Fact, resolve *resolver, ground *grounding, resolvable map[string]bool, members map[string]map[string]bool, memberFacts, carriers, carried map[string][]facts.Fact, exportedFiles, definedNames, composed map[string]bool, census map[string]map[string]bool) []facts.Insight {
	if verdictSeam != nil {
		verdictSeam(r)
	}
	var verdicts []facts.Insight
	switch {
	case r.forbid != "":
		verdicts = e.verdictForbid(r, resolve, ground)
	case r.forbidReach != "":
		verdicts = e.verdictForbidReach(r, graphWalk, resolve, members)
	case r.allow != "":
		verdicts = e.verdictAllowOnly(r, resolve, resolvable, ground)
	case r.protect != "":
		verdicts = e.verdictProtect(r, graphWalk, resolve)
	case r.private != "":
		verdicts = e.verdictPrivate(r, graphWalk, resolve, memberFacts, exportedFiles)
	case r.forbidFact != "":
		verdicts = e.verdictForbidFact(r, memberFacts, members)
	case r.cap != "":
		verdicts = e.verdictCap(r, memberFacts, members)
	case r.require != "":
		verdicts = e.verdictRequire(r, memberFacts, members)
	case r.requireDefines != "":
		verdicts = e.verdictRequireDefines(r, memberFacts, members, definedNames, composed)
	case r.forbidCycles != "":
		verdicts = e.verdictForbidCycles(r, store, memberFacts)
	case r.independent != "":
		verdicts = e.verdictIndependent(r, store, memberFacts, carried)
	case r.requireName != "":
		verdicts = e.verdictRequireName(r, memberFacts, members)
	case r.forbidName != "":
		verdicts = e.verdictForbidName(r, memberFacts, members)
	case r.requireEdge != "":
		verdicts = e.verdictRequireEdge(r, graphWalk, memberFacts, carriers, members, census, resolve, ground)
	case r.protocol != "":
		verdicts = e.verdictProtocol(r, memberFacts, carriers, members, census, resolve)
	case r.guide != "":
		verdicts = e.verdictGuide(r)
	case r.storageStaysHome != "":
		verdicts = e.verdictStorageStaysHome(r, store, memberFacts, members, resolve)
	case r.capRuntime != "":
		verdicts = e.verdictCapRuntime(r, store, members)
	case r.requireConsumer != "":
		verdicts = e.verdictRequireConsumer(r, store, memberFacts)
	case r.uniqueAcross != "":
		verdicts = e.verdictUniqueAcross(r, memberFacts)
	case r.requireGoverned != "":
		verdicts = e.verdictRequireGoverned(r, store, memberFacts)
	}

	return verdicts
}

// withoutExcluded drops the members the exclusion names, by file, from one
// component's resolved membership: the selector still admitted them, the
// radius asks what the rules say once they are gone.
func withoutExcluded(names map[string]bool, members []facts.Fact, exclude func(facts.Fact) bool) (map[string]bool, []facts.Fact) {
	kept := make([]facts.Fact, 0, len(members))
	keptNames := map[string]bool{}
	for _, m := range members {
		if exclude(m) {
			continue
		}
		kept = append(kept, m)
		keptNames[m.Name] = true
	}
	for name := range names {
		if !keptNames[name] {
			delete(names, name)
		}
	}
	return names, kept
}

// dedupVerdicts folds insights with identical titles into one, merging their
// evidence. Two carrier facts can name the same dependency — gin's gin.go and
// context.go each emit a dependency fact named ". -> …/render", and each
// carries the same normalized imports edge — so a per-edge walk emits the same
// verdict twice, which reads as two breaches of one rule on one dependency.
// One verdict, every witnessing file in its evidence. Evidence is sorted and
// exact duplicates (one fact carrying the same relation twice) dropped, so the
// merged verdict is a function of the graph and never of walk order.
func dedupVerdicts(insights []facts.Insight) []facts.Insight {
	indexByTitle := map[string]int{}
	var out []facts.Insight
	for _, insight := range insights {
		if at, seen := indexByTitle[insight.Title]; seen {
			out[at].Evidence = append(out[at].Evidence, insight.Evidence...)
			continue
		}
		indexByTitle[insight.Title] = len(out)
		out = append(out, insight)
	}
	for i := range out {
		evidence := out[i].Evidence
		sort.Slice(evidence, func(a, b int) bool {
			if evidence[a].File != evidence[b].File {
				return evidence[a].File < evidence[b].File
			}
			if evidence[a].Symbol != evidence[b].Symbol {
				return evidence[a].Symbol < evidence[b].Symbol
			}
			return evidence[a].Fact < evidence[b].Fact
		})
		deduped := evidence[:0]
		for j, ev := range evidence {
			if j == 0 || ev != evidence[j-1] {
				deduped = append(deduped, ev)
			}
		}
		out[i].Evidence = deduped
	}
	return out
}

func exemptVerdicts(r rule, verdicts []facts.Insight) []facts.Insight {
	if len(r.exempt) == 0 {
		return verdicts
	}
	byTitle := map[string]intent.ConstraintExemption{}
	for _, ex := range r.exempt {
		byTitle[r.titled(ex.Witness)] = ex
	}
	matched := map[string]bool{}
	skipped := false
	out := make([]facts.Insight, 0, len(verdicts))
	for _, v := range verdicts {
		if strings.HasPrefix(v.Title, fmt.Sprintf("forbid_reach rule %s skipped:", r.id)) ||
			strings.HasPrefix(v.Title, fmt.Sprintf("require_edge rule %s skipped:", r.id)) ||
			strings.HasPrefix(v.Title, fmt.Sprintf("require rule %s skipped:", r.id)) ||
			strings.HasPrefix(v.Title, fmt.Sprintf("protocol rule %s skipped:", r.id)) {
			skipped = true
		}
		ex, found := byTitle[v.Title]
		if !found {
			out = append(out, v)
			continue
		}
		matched[ex.Witness] = true
		out = append(out, exemptedInsight(r, ex, v))
	}
	if skipped {
		return out
	}
	for _, ex := range r.exempt {
		if matched[ex.Witness] {
			continue
		}
		out = append(out, deadExemptionInsight(r, ex))
	}
	return out
}

func exemptedInsight(r rule, ex intent.ConstraintExemption, v facts.Insight) facts.Insight {
	evidence := append([]facts.Evidence{{
		Fact:   "rule: " + r.id,
		Detail: fmt.Sprintf("exempted by %s since %s — %s", ex.Owner, ex.Since, ex.Because),
	}}, v.Evidence...)
	return facts.Insight{
		Title:       fmt.Sprintf("Exempted from constraint %s: %s", r.id, ex.Witness),
		Description: fmt.Sprintf("The rule would report this witness as a violation, and the declaration carves it out: exempted by %s since %s, because: %s. An exemption is a recorded decision riding the law itself — counted here, never a violation in any enforcement mode — and the rule stands unchanged for every other witness. Rule because: %s", ex.Owner, ex.Since, ex.Because, r.because),
		Confidence:  exemptedConfidence,
		Evidence:    evidence,
		Actions: []string{
			"Fix the underlying breach and delete the exemption from " + r.source + " if the decision is retired",
			"Leave the exemption in place if the carve-out stands — it is a declared decision, not debt",
		},
	}
}

func deadExemptionInsight(r rule, ex intent.ConstraintExemption) facts.Insight {
	return facts.Insight{
		Title:       fmt.Sprintf("Constraint exemption on %s matches nothing: %s", r.id, ex.Witness),
		Description: fmt.Sprintf("The exemption's witness matches no violation the rule reports: either the breach it excused is gone — the exemption outlived its violation and should be deleted — or the witness never matched anything (witnesses are matched exactly against the violation identity the rule titles with). This warning exists so a dead exemption cannot silently outlive its reason. Declared by %s since %s, because: %s", ex.Owner, ex.Since, ex.Because),
		Confidence:  deadExemptionConfidence,
		Evidence:    []facts.Evidence{{Fact: "rule: " + r.id, Detail: "declared in " + r.source}},
		Actions: []string{
			"Delete the exemption from " + r.source + " if the violation it excused is gone",
			"Fix the witness to the exact violation identity if it never matched",
		},
	}
}

// verdictForbid emits one violation per measured via-edge from the forbidden
// component into the to component.
func (e *Explainer) verdictForbid(r rule, resolve *resolver, ground *grounding) []facts.Insight {
	var out []facts.Insight
	skipped := map[string]bool{}
	for _, f := range resolve.sources(r, r.forbid) {
		from, sourced := resolve.source(r, r.forbid, f)
		if !sourced {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind != r.via {
				continue
			}
			// A literal far end is compared against the target the near end
			// recorded, which is all the graph holds when that end is an
			// external package or a function imported from one. It resolves
			// against no component, so it reaches no basis either: the two
			// ways a rule may name its far end are read here in the order the
			// declaration wrote them.
			var onto basis
			if len(r.toName) > 0 {
				if !matchesAnyBoundedName(rel.Target, r.toName) {
					continue
				}
				if r.receiver == "none" && strings.ContainsAny(rel.Target, ".#") {
					continue
				}
			} else {
				var landed bool
				onto, landed = resolve.target(r, r.to, rel, f)
				if !landed {
					if ground.ungroundable(rel, f) {
						skipped[rel.Target] = true
					}
					continue
				}
			}
			out = append(out, facts.Insight{
				Title:       r.titled(fmt.Sprintf("%s -> %s via %s", f.Name, rel.Target, r.via)),
				Description: fmt.Sprintf("%s must not reach %s via %s, and the graph measures exactly this edge. The rule is declared, %s, so this is a decided-rule breach, not a heuristic. Because: %s", r.forbid, forbidFarEnd(r), r.via, forbidFarBasis(r, from, onto), r.because),
				Confidence:  r.confidence(),
				Evidence: []facts.Evidence{{
					File:   f.File,
					Symbol: f.Name,
					Fact:   rel.Target,
					Detail: "forbidden " + r.via + " edge",
				}},
				Actions: []string{
					cutForEdge(resolve, r.to, f, rel.Target),
					"Remove or reroute the edge if the rule stands",
					"Amend the rule on its declaring page if the decision behind it changed",
				},
			})
		}
	}
	out = dedupVerdicts(out)
	if len(skipped) > 0 {
		out = append(out, groundSkipInsight(r, skipped))
	}
	return out
}

// verdictForbidReach emits one violation per (source, target) pair where a
// member (or dependency carrier) of the forbidden component reaches a member
// of the to component through ANY measured path of rule-via edges — the
// transitive form. The walk is a breadth-first search over exact fact names,
// bounded by reachDepthCap and a visited set, with adjacency built from the
// sorted whole-graph fact set and every neighbor list sorted — so the witness
// path, the shortest one found, is a function of the graph and never of store
// order. A membership larger than reachComponentCap on either side degrades
// the rule to one skip advisory instead of walking: the honest degrade,
// visible rather than slow. Direct edges are one-hop paths here, so every
// pair a forbid rule would catch is caught; a rule declaring both forms
// reports through both, because separate rules are separate.
// forbidFarEnd renders whichever way the rule named its far end, so a verdict
// reads the same whether the end resolved to a component or to a literal.
func forbidFarEnd(r rule) string {
	if len(r.toName) > 0 {
		return strings.Join(r.toName, " or ")
	}
	return r.to
}

// matchesAnyBoundedName reports whether the target matches any of the literals
// in the bounded dialect the validator admits.
func matchesAnyBoundedName(target string, patterns []string) bool {
	for _, p := range patterns {
		if intent.MatchBoundedName(target, p) {
			return true
		}
		// A literal naming a bare method matches the method of a chained or
		// receiver-qualified call target as well: `update_all` is the call
		// whether the extractor recorded it as update_all, where.update_all or
		// Order.update_all. A literal carrying a receiver stays exact.
		if !strings.ContainsAny(p, ".#") {
			if i := strings.LastIndexAny(target, ".#"); i >= 0 && intent.MatchBoundedName(target[i+1:], p) {
				return true
			}
		}
	}
	return false
}

// forbidFarBasis states what makes the far end the far end. A literal is
// matched against the recorded edge target and nothing else, which is a weaker
// claim than a resolved membership and is stated as one rather than dressed up.
func forbidFarBasis(r rule, from, onto basis) string {
	if len(r.toName) > 0 {
		return "the edge target the near end recorded matches the named literal"
	}
	return edgeBasis(from, onto)
}

func (e *Explainer) verdictForbidReach(r rule, graphWalk []facts.Fact, resolve *resolver, members map[string]map[string]bool) []facts.Insight {
	if len(members[r.forbidReach]) > reachComponentCap || len(members[r.to]) > reachComponentCap {
		return []facts.Insight{{
			Title:       fmt.Sprintf("forbid_reach rule %s skipped: component too large for bounded traversal", r.id),
			Description: fmt.Sprintf("The rule names %s (%d members) and %s (%d members), and a side exceeding %d members makes the bounded walk a cost nobody declared. No verdict was reached — this advisory exists so that the skip cannot be read as compliance. Because: %s", r.forbidReach, len(members[r.forbidReach]), r.to, len(members[r.to]), reachComponentCap, r.because),
			Confidence:  reachSkipConfidence,
			Evidence:    []facts.Evidence{{Fact: "rule: " + r.id, Detail: "declared in " + r.source}},
			Actions: []string{
				"Narrow the component selectors if the rule should verdict here",
				"Split the rule across smaller components on the declaring page",
			},
		}}
	}

	vias := reachVias
	if r.via != "" {
		vias = []string{r.via}
	}
	viaSet := map[string]bool{}
	for _, kind := range vias {
		viaSet[kind] = true
	}

	// Adjacency over exact fact names. graphWalk is sorted by name then file,
	// so each neighbor list is appended in a deterministic order; sorting and
	// deduplicating it afterwards makes the BFS discovery order — and with it
	// the shortest-path tiebreak — a function of the graph alone.
	adjacency := map[string][]string{}
	toSet := map[string]bool{}
	targetBasisOf := map[string]basis{}
	for name := range members[r.to] {
		toSet[name] = true
		targetBasisOf[name] = exactBasis
	}
	for _, f := range graphWalk {
		for _, rel := range f.Relations {
			if !viaSet[rel.Kind] {
				continue
			}
			adjacency[f.Name] = append(adjacency[f.Name], rel.Target)
			// A path target names no fact, so it is a leaf of this walk — but it
			// can still BE the landing the rule forbids, and the direct form now
			// catches exactly those. Collecting them here keeps the invariant
			// that every pair forbid catches is within reach's. A target that is
			// a member's method lands the same way, on the declaration's terms.
			if toSet[rel.Target] {
				continue
			}
			if onto, landed := resolve.target(r, r.to, rel, f); landed {
				toSet[rel.Target] = true
				targetBasisOf[rel.Target] = onto
			}
		}
	}
	for name, targets := range adjacency {
		sort.Strings(targets)
		deduped := targets[:0]
		for i, t := range targets {
			if i == 0 || t != targets[i-1] {
				deduped = append(deduped, t)
			}
		}
		adjacency[name] = deduped
	}

	// Sources: the component's member names, plus its dependency carriers —
	// the same two walks verdictForbid sources from, so a pair the direct form
	// catches is never out of this form's reach.
	walked := resolve.sources(r, r.forbidReach)
	sources := sortedMemberNames(members[r.forbidReach])
	sourceBasisOf := map[string]basis{}
	for _, name := range sources {
		sourceBasisOf[name] = exactBasis
	}
	for _, f := range walked {
		if _, seen := sourceBasisOf[f.Name]; seen {
			continue
		}
		from, sourced := resolve.source(r, r.forbidReach, f)
		if !sourced {
			continue
		}
		sourceBasisOf[f.Name] = from
		sources = append(sources, f.Name)
	}

	sourceFacts := firstFactByName(walked)
	viaWords := strings.Join(vias, ", ")

	var out []facts.Insight
	for _, source := range sources {
		for _, path := range reachWitnesses(adjacency, source, toSet) {
			target := path[len(path)-1]
			f := sourceFacts[source]
			out = append(out, facts.Insight{
				Title:       r.titled(fmt.Sprintf("%s reaches %s", source, target)),
				Description: fmt.Sprintf("%s must not reach %s through any measured path over %s, and the graph measures one: %s. The rule is declared, %s, and every hop is a measured edge, so this is a decided-rule breach, not a heuristic. Because: %s", r.forbidReach, r.to, viaWords, strings.Join(path, " -> "), edgeBasis(sourceBasisOf[source], targetBasisOf[target]), r.because),
				Confidence:  r.confidence(),
				Evidence: []facts.Evidence{{
					File:   f.File,
					Symbol: source,
					Fact:   target,
					Detail: fmt.Sprintf("reachable in %d hop(s)", len(path)-1),
				}},
				Actions: []string{
					"Break the path at any hop if the rule stands",
					"Amend the rule on its declaring page if the decision behind it changed",
				},
			})
		}
	}
	return out
}

// reachWitnesses walks breadth-first from one source over the adjacency and
// returns the shortest path to every distinct target-set member reached
// within reachDepthCap edges, in target-name order — one witness per (source,
// target) pair, deduplicated by the visited set that also bounds the walk.
// The source reaching itself is no path: a member of both components is a
// membership overlap, not a measured reach.
func reachWitnesses(adjacency map[string][]string, source string, toSet map[string]bool) [][]string {
	type queued struct {
		name  string
		depth int
	}
	visited := map[string]bool{source: true}
	parent := map[string]string{}
	queue := []queued{{name: source, depth: 0}}
	var reached []string
	for qi := 0; qi < len(queue); qi++ {
		item := queue[qi]
		if item.depth >= reachDepthCap {
			continue
		}
		for _, next := range adjacency[item.name] {
			if visited[next] {
				continue
			}
			visited[next] = true
			parent[next] = item.name
			if toSet[next] {
				reached = append(reached, next)
			}
			queue = append(queue, queued{name: next, depth: item.depth + 1})
		}
	}
	sort.Strings(reached)

	witnesses := make([][]string, 0, len(reached))
	for _, target := range reached {
		var path []string
		for cur := target; cur != source; cur = parent[cur] {
			path = append(path, cur)
		}
		path = append(path, source)
		for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
			path[i], path[j] = path[j], path[i]
		}
		witnesses = append(witnesses, path)
	}
	return witnesses
}

// verdictAllowOnly emits one violation per measured via-edge from the allow
// component that resolves to a measured fact outside every allowed component.
// An edge landing back inside the allow component itself is not a breach: a
// component reaching its own members is internal structure, not an
// architectural reach, and forcing every page to list a component in its own
// only: would make each rule assert something nobody decided. An edge whose
// target resolves to nothing measured is skipped — fail closed, never guessed
// into a breach.
func (e *Explainer) verdictAllowOnly(r rule, resolve *resolver, resolvable map[string]bool, ground *grounding) []facts.Insight {
	// One target question, asked of the allow component and of every allowed
	// landing in turn — exact name, then a member's method, then the measured
	// file a path target grounds onto.
	allowed := func(rel facts.Relation, from facts.Fact) bool {
		if _, landed := resolve.target(r, r.allow, rel, from); landed {
			return true
		}
		for _, name := range r.only {
			if _, landed := resolve.target(r, name, rel, from); landed {
				return true
			}
		}
		return false
	}
	var out []facts.Insight
	skipped := map[string]bool{}
	for _, f := range resolve.sources(r, r.allow) {
		from, sourced := resolve.source(r, r.allow, f)
		if !sourced {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind != r.via {
				continue
			}
			if !resolvable[rel.Target] && !ground.resolves(rel, f) {
				if ground.ungroundable(rel, f) {
					skipped[rel.Target] = true
				}
				continue
			}
			if allowed(rel, f) {
				continue
			}
			onto := groundedBasis
			if resolvable[rel.Target] {
				onto = exactBasis
			}
			out = append(out, facts.Insight{
				Title:       r.titled(fmt.Sprintf("%s -> %s via %s", f.Name, rel.Target, r.via)),
				Description: fmt.Sprintf("%s may reach only %s via %s, and the graph measures this edge landing in none of them. The rule is declared, %s, so this is a decided-rule breach, not a heuristic. Because: %s", r.allow, strings.Join(r.only, ", "), r.via, disallowedBasis(from, onto), r.because),
				Confidence:  r.confidence(),
				Evidence: []facts.Evidence{{
					File:   f.File,
					Symbol: f.Name,
					Fact:   rel.Target,
					Detail: "disallowed " + r.via + " edge",
				}},
				Actions: []string{
					"Reroute the edge into an allowed component if the rule stands",
					"Widen only: on the declaring page if the decision behind it changed",
				},
			})
		}
	}
	out = dedupVerdicts(out)
	if len(skipped) > 0 {
		out = append(out, groundSkipInsight(r, skipped))
	}
	return out
}

// verdictProtect emits one violation per measured via-edge landing on a member
// of the protected component from a source owned by none of the owner
// components. Source ownership resolves exactly: a member-kind fact by its
// canonical name in an owner's membership, a dependency carrier by its file
// joining an owner's patterns (never a name-narrowed owner's — a file-level
// join cannot prove the named fact made the edge). An edge from inside the
// protected component itself is not a breach, by the same reasoning as
// allow-only's self edges: internal structure is not a reach.
func (e *Explainer) verdictProtect(r rule, graphWalk []facts.Fact, resolve *resolver) []facts.Insight {
	inside := append([]string{r.protect}, r.owners...)
	var out []facts.Insight
	for _, f := range graphWalk {
		for _, rel := range f.Relations {
			if rel.Kind != r.via {
				continue
			}
			onto, landed := resolve.target(r, r.protect, rel, f)
			if !landed {
				continue
			}
			if resolve.sourceIn(r, inside, f) {
				continue
			}
			out = append(out, facts.Insight{
				Title:       r.titled(fmt.Sprintf("%s -> %s via %s", f.Name, rel.Target, r.via)),
				Description: fmt.Sprintf("Only %s may reach members of %s via %s, and the graph measures this edge arriving from outside every owner. The rule is declared, %s, so this is a decided-rule breach, not a heuristic. Because: %s", strings.Join(r.owners, ", "), r.protect, r.via, reverseBasis(onto, "owners:"), r.because),
				Confidence:  r.confidence(),
				Evidence: []facts.Evidence{{
					File:   f.File,
					Symbol: f.Name,
					Fact:   rel.Target,
					Detail: "unowned " + r.via + " edge",
				}},
				Actions: []string{
					cutForEdge(resolve, r.protect, f, rel.Target),
					"Route the access through an owning component if the rule stands",
					"Add the source's component to owners: on the declaring page if the decision behind it changed",
				},
			})
		}
	}
	return dedupVerdicts(out)
}

// verdictPrivate emits one violation per measured reach-edge landing on a
// non-exported member of the private component from a source that is neither
// inside the component nor inside an except component. Non-exported is the
// extractor's own measurement: a member counts only when every fact bearing
// its name carries exported: false — a member with no boolean exported prop
// (the extractor recorded no visibility) is out of the rule's scope, and a
// name whose facts disagree about visibility is too, both fail closed. The
// walk covers every rule-via edge kind at once: privacy is about any measured
// reach, so the form carries no via of its own.
func (e *Explainer) verdictPrivate(r rule, graphWalk []facts.Fact, resolve *resolver, memberFacts map[string][]facts.Fact, exported map[string]bool) []facts.Insight {
	internal := map[string]bool{}
	// The same measurement, keyed by file: a file-granular import target names no
	// member, and reaching a file whose every measured fact is non-exported is
	// reaching non-exported code. The file test reads the SNAPSHOT's facts, not
	// the component's members, and that is the whole of it: a membership can be a
	// strict subset of what a file holds — every narrowing on a component makes
	// it one — so a file marked internal from its member alone gates an import
	// that reached the file's other, exported content. One exported fact anywhere
	// in the file disqualifies it; a member with no visibility prop, or a name
	// whose facts disagree, disqualifies that name, both fail closed.
	internalFiles := map[string]bool{}
	// A component that names its public files decides visibility by path:
	// inside those files a member is the surface, outside them it is
	// internal, whatever the language's own keyword says. Ruby marks every
	// method exported, so without this a Ruby component could not state a
	// surface at all.
	public := resolve.components[r.private].public
	for _, f := range memberFacts[r.private] {
		visible, ok := f.PropAny("exported").(bool)
		if len(public) > 0 && f.File != "" {
			visible, ok = matchConstraintPath(f.File, public), true
		}
		if !ok || visible {
			internal[f.Name] = false
			if f.File != "" {
				internalFiles[f.File] = false
			}
			continue
		}
		if _, seen := internal[f.Name]; !seen {
			internal[f.Name] = true
		}
		if _, seen := internalFiles[f.File]; f.File != "" && !seen && !exported[f.File] {
			internalFiles[f.File] = true
		}
	}
	inside := append([]string{r.private}, r.except...)
	reachKind := map[string]bool{}
	for _, kind := range reachVias {
		reachKind[kind] = true
	}
	scope := ""
	if len(r.except) > 0 {
		scope = fmt.Sprintf(" (or except: %s)", strings.Join(r.except, ", "))
	}
	var out []facts.Insight
	for _, f := range graphWalk {
		for _, rel := range f.Relations {
			if !reachKind[rel.Kind] {
				continue
			}
			// The member behind the target, which is the target itself when it is
			// one and the declaring member when the declaration owns it: the
			// visibility the rule reads was measured on the member, so a method is
			// reached as private exactly when its owner is.
			onto := groundedBasis
			member, named := resolve.memberBehind(r, r.private, rel.Target)
			switch {
			case named && internal[member]:
				onto = exactBasis
				if member != rel.Target {
					onto = ownedBasis
				}
			case named:
				continue
			case !resolve.ground.resolvedPathIn(rel, f, func(path string) bool { return internalFiles[path] }):
				continue
			}
			if resolve.sourceIn(r, inside, f) {
				continue
			}
			out = append(out, facts.Insight{
				Title:       r.titled(fmt.Sprintf("%s -> %s via %s", f.Name, rel.Target, rel.Kind)),
				Description: fmt.Sprintf("%s %s %s, reachable only from inside the component%s, and the graph measures this %s edge arriving from outside. The rule is declared, %s, and the visibility is the extractor's own measurement, so this is a decided-rule breach, not a heuristic. Because: %s", rel.Target, privateSubject(onto), r.private, scope, rel.Kind, privateBasisPhrase(onto), r.because),
				Confidence:  r.confidence(),
				Evidence: []facts.Evidence{{
					File:   f.File,
					Symbol: f.Name,
					Fact:   rel.Target,
					Detail: "reach into a non-exported member via " + rel.Kind,
				}},
				Actions: []string{
					cutForEdge(resolve, r.private, f, rel.Target),
					"Route the access through the component's exported surface if the rule stands",
					"Add the source's component to except: on the declaring page if the decision behind it changed",
				},
			})
		}
	}
	return dedupVerdicts(out)
}

// verdictForbidFact emits one violation per member of a component declared to
// have none.
func (e *Explainer) verdictForbidFact(r rule, memberFacts map[string][]facts.Fact, members map[string]map[string]bool) []facts.Insight {
	var out []facts.Insight
	first := firstFactByName(memberFacts[r.forbidFact])
	for _, name := range sortedMemberNames(members[r.forbidFact]) {
		f := first[name]
		out = append(out, facts.Insight{
			Title:       r.titled(fmt.Sprintf("%s is measured in %s", name, r.forbidFact)),
			Description: fmt.Sprintf("%s must have no members, and the graph measures this fact inside it. The rule is declared and the membership is exact, so this is a decided-rule breach, not a heuristic. Because: %s", r.forbidFact, r.because),
			Confidence:  r.confidence(),
			Evidence: []facts.Evidence{{
				File:   f.File,
				Symbol: f.Name,
				Detail: "member of forbidden component " + r.forbidFact,
			}},
			Actions: []string{
				"Remove or relocate the fact if the rule stands",
				"Retire the rule on its declaring page if the decision behind it changed",
			},
		})
	}
	return out
}

// verdictCap emits ONE violation when a component's membership exceeds its
// declared cap, naming the count and the overflow — the members beyond the cap
// in name order. One finding, not one per member: the breach is the count, and
// a per-member fanout would churn every violation's identity each time any
// member was added or removed.
func (e *Explainer) verdictCap(r rule, memberFacts map[string][]facts.Fact, members map[string]map[string]bool) []facts.Insight {
	names := sortedMemberNames(members[r.cap])
	if len(names) <= r.maxMembers {
		return nil
	}
	overflow := names[r.maxMembers:]
	first := firstFactByName(memberFacts[r.cap])
	evidence := make([]facts.Evidence, 0, len(overflow))
	for _, name := range overflow {
		evidence = append(evidence, facts.Evidence{
			File:   first[name].File,
			Symbol: name,
			Detail: "beyond the declared cap",
		})
	}
	return []facts.Insight{{
		Title:       r.titled(fmt.Sprintf("%s has %d members over a cap of %d", r.cap, len(names), r.maxMembers)),
		Description: fmt.Sprintf("%s membership counts %d against a declared cap of %d. The overflow, in name order: %s. The rule is declared and the membership is exact, so this is a decided-rule breach, not a heuristic. Because: %s", r.cap, len(names), r.maxMembers, strings.Join(overflow, ", "), r.because),
		Confidence:  r.confidence(),
		Evidence:    capEvidence(r, evidence, len(names)),
		Actions: []string{
			"Shrink the surface back under the cap if the rule stands",
			"Raise max_members on the declaring page if the decision behind it changed",
		},
	}}
}

// verdictRequire emits one violation per member of the require component that
// matches the when clauses (or every member, when none is declared) and fails
// the must clause. The prop clauses are whole-member containment over the
// fact's space-separated set prop — never substring, so "columns contains
// company_id" cannot be satisfied by parent_company_id. A member whose
// when-prop is absent simply does not match the when clause: what was never
// measured is out of the rule's scope, not in breach of it.
//
// The edge clause reads the member fact's OWN outgoing relations of the rule's
// via kind and matches their targets against the declared literals. Everything
// it touches lives on the one fact that made the member a member: no second
// component is resolved, no edge is followed to whatever sits at its far end,
// so the form asks no ownership question and stays a property rule that
// happens to read a relation. Carrier facts are deliberately not folded in —
// a dependency carrier's edges belong to a FILE, and attributing them to each
// member of that file would be exactly the attribution this form must not
// make. That restraint is what the advisory below has to keep audible: where a
// component's facts are not the facts that carry the edges — a Ruby class,
// whose calls ride its Owner#method facts — the antecedent selects nobody, and
// a rule that holds because it looked at nothing is the failure this explainer
// exists to prevent.
//
// The advisory is therefore read off the antecedent's own answers rather than
// from a second scan beside it: whether any member was selected is counted as
// the members are selected, on the same representative facts factEdgeTo is
// called on, so the advisory cannot certify an edge the antecedent never
// consults. It is counted before the prop clause narrows, so it speaks for the
// edge antecedent alone.
func (e *Explainer) verdictRequire(r rule, memberFacts map[string][]facts.Fact, members map[string]map[string]bool) []facts.Insight {
	memberNames := sortedMemberNames(members[r.require])
	var out []facts.Insight
	edgeSelectedAny := false
	first := firstFactByName(memberFacts[r.require])
	for _, name := range memberNames {
		f := first[name]
		witness, selected := factEdgeTo(f, r.via, r.whenEdgeTo)
		if !selected {
			continue
		}
		edgeSelectedAny = true
		if r.whenProp != "" && !propSetContains(f, r.whenProp, r.whenValue) {
			continue
		}
		if propSetContains(f, r.mustProp, r.mustValue) {
			continue
		}
		scope := requireScope(r)
		detail := fmt.Sprintf("missing %s %s", r.mustProp, r.mustValue)
		if witness != "" {
			detail = fmt.Sprintf("%s edge to %s, missing %s %s", r.via, witness, r.mustProp, r.mustValue)
		}
		out = append(out, facts.Insight{
			Title:       r.titled(fmt.Sprintf("%s must have %s containing %s", name, r.mustProp, r.mustValue)),
			Description: fmt.Sprintf("%s is a member of %s%s, so it must have %s containing %s — and the measured fact does not. The rule is declared, membership is exact, and containment is whole-member, so this is a decided-rule breach, not a heuristic. Because: %s", name, r.require, scope, r.mustProp, r.mustValue, r.because),
			Confidence:  r.confidence(),
			Evidence: []facts.Evidence{{
				File:   f.File,
				Symbol: f.Name,
				Detail: detail,
			}},
			Actions: []string{
				fmt.Sprintf("Add the required %s if the rule stands", r.mustProp),
				"Amend the rule in its declaring file if the decision behind it changed",
			},
		})
	}
	if len(r.whenEdgeTo) > 0 && len(memberNames) > 0 && !edgeSelectedAny {
		return []facts.Insight{requireEdgeAntecedentSkipInsight(r, len(memberNames))}
	}
	return out
}

// factEdgeTo answers whether the fact makes an outgoing edge of the via kind
// at one of the declared literal targets, returning the target that witnessed
// it — the edge a reader opens the finding to see. With no edge antecedent
// declared every fact is selected and the witness is empty, so the caller
// reads one answer for both shapes of the form.
//
// The target is compared with intent.MatchBoundedName, the same matcher the
// validator's admission is defined against: an exact name, a prefix*, or a
// *suffix. The suffix form is what makes the dialect fit real graphs, where a
// call target arrives qualified — *.reactiveUnwrap matches
// ember_app/app/utils.reactiveUnwrap without the declaration having to know
// where the helper lives.
func factEdgeTo(f facts.Fact, via string, targets []string) (string, bool) {
	if len(targets) == 0 {
		return "", true
	}
	for _, rel := range f.Relations {
		if rel.Kind != via {
			continue
		}
		for _, target := range targets {
			if intent.MatchBoundedName(rel.Target, target) {
				return rel.Target, true
			}
		}
	}
	return "", false
}

// requireScope renders why a member is in the rule's scope, one clause per
// declared antecedent, so a verdict states the whole reason it was asked of
// this member and not of its neighbour.
func requireScope(r rule) string {
	var clauses []string
	if r.whenProp != "" {
		clauses = append(clauses, fmt.Sprintf("whose %s contains %s", r.whenProp, r.whenValue))
	}
	if len(r.whenEdgeTo) > 0 {
		clauses = append(clauses, fmt.Sprintf("that makes a %s edge to %s", r.via, strings.Join(r.whenEdgeTo, " or ")))
	}
	if len(clauses) == 0 {
		return ""
	}
	return " " + strings.Join(clauses, " and ")
}

// requireEdgeAntecedentSkipInsight is the honest degrade for the edge
// antecedent: it selected no member, so every member would have passed without
// being asked anything. That is the one way this form can be vacuous, and it
// is silent by construction — zero selected members is zero violations — so
// the advisory is the only thing standing between a rule that looked at
// nothing and a clean report. Two readings reach it and the advisory states
// both, because the graph cannot tell them apart: nobody makes the call, or
// the facts that would make it are not the facts this component selects.
//
// It fires on nobody selected, never on some, and that boundary is deliberate.
// A component where one member answers and the next is blind needs a notion of
// which fact owns which edge before a partial answer can be told from a real
// absence, and this form makes no ownership claim.
func requireEdgeAntecedentSkipInsight(r rule, memberCount int) facts.Insight {
	form, component, via := "require", r.require, r.via
	if r.requireEdge != "" {
		form, component, via = "require_edge", r.requireEdge, r.whenVia
	}
	return facts.Insight{
		Title:       fmt.Sprintf("%s rule %s skipped: no member of %s makes a %s edge the antecedent selects", form, r.id, component, via),
		Description: fmt.Sprintf("The rule selects members by the %s edges they make to %s, and not one of the %d members of %s makes one on the fact the rule reads — so the antecedent selected nobody and the rule reported nothing about anyone. Either no member makes that call, or the component's facts are not the facts that carry its edges: a class's calls ride its own methods' facts, so a component of classes cannot answer an edge antecedent even where the calls are measured. No verdict was reached — skipped, never silently compliant — and this advisory exists so that silence cannot be read as compliance. Where some members answer and others cannot, no advisory fires: telling a blind member from one that simply makes no such call needs a notion of which fact owns which edge that these facts do not carry. Because: %s", via, strings.Join(r.whenEdgeTo, " or "), memberCount, component, r.because),
		Confidence:  requireSkipConfidence,
		Evidence: []facts.Evidence{
			{Fact: "rule: " + r.id, Detail: "declared in " + r.source},
			{Fact: "component: " + component, Detail: fmt.Sprintf("%d member(s), none selected by the %s edge antecedent", memberCount, via)},
		},
		Actions: []string{
			"Select the facts that carry the edges — the methods rather than the classes that own them — if the rule should verdict here",
			"Check the targets name the far end as the graph writes it — a call target arrives qualified, so *suffix is the form that usually matches",
			"State the antecedent as when_prop_contains, over a prop these facts do carry, if their edges are measured elsewhere",
		},
	}
}

// definesMethods reports whether a symbol kind names a type that OWNS the
// methods a repository writes for it, which is the only kind require_defines
// can verdict. Two kinds qualify and the rest are excluded on purpose:
//
//   - class covers Ruby, Python, TypeScript, Java, Kotlin, PHP and the rest.
//   - struct covers the languages whose method owner is not spelled "class" —
//     Go, Rust, C++, C#. A Go struct that embeds another carries an implements
//     relation, so composition is excluded by the same fail-closed rule that
//     excludes an inheriting Ruby class, not by the kind gate.
//
// interface is excluded because it declares signatures rather than defining
// them, so "does not define" is never a breach there, and type is excluded
// because an alias defines nothing at all. Before struct was admitted this
// form resolved its members on a Go repository, verdicted nothing and emitted
// no advisory, which is the vacuous compliance the rest of this vocabulary
// exists to make loud.
func definesMethods(symbolKind string) bool {
	return symbolKind == facts.SymbolClass || symbolKind == facts.SymbolStruct
}

// verdictRequireDefines emits one violation per class-kind member of the
// component that visibly lacks a measured method symbol of the declared name,
// in either qualified shape the extractors emit — <Class>#<method> (instance)
// or <Class>.<method> (class-level). Only members every fact of which is a
// plain class participate: a class that inherits, includes or extends
// anything (an implements relation on the class symbol, or a mixin dependency
// carrier naming the class as its source) could receive the definition
// through composition the store does not resolve, so it is out of the rule's
// scope, not in breach of it — fail closed, never guessed.
func (e *Explainer) verdictRequireDefines(r rule, memberFacts map[string][]facts.Fact, members map[string]map[string]bool, definedNames, composed map[string]bool) []facts.Insight {
	classKind := map[string]bool{}
	for _, f := range memberFacts[r.requireDefines] {
		if f.Kind != facts.KindSymbol {
			continue
		}
		if sk, _ := f.PropAny("symbol_kind").(string); definesMethods(sk) {
			if _, seen := classKind[f.Name]; !seen {
				classKind[f.Name] = true
			}
		} else {
			classKind[f.Name] = false
		}
	}
	var out []facts.Insight
	first := firstFactByName(memberFacts[r.requireDefines])
	for _, name := range sortedMemberNames(members[r.requireDefines]) {
		if !classKind[name] || composed[name] {
			continue
		}
		if definesAny(definedNames, name, r.wantedMethods()) {
			continue
		}
		f := first[name]
		out = append(out, facts.Insight{
			Title:       r.titled(fmt.Sprintf("%s does not define %s", name, r.wantedSentence())),
			Description: fmt.Sprintf("%s is a class member of %s, so it must define %s — and no measured symbol %s#%s or %s.%s exists. Classes that inherit, include or extend anything are out of this rule's scope, so the definition is visibly absent, not composed in. The rule is declared and membership is exact, so this is a decided-rule breach, not a heuristic. Because: %s", name, r.requireDefines, r.method, name, r.method, name, r.method, r.because),
			Confidence:  r.confidence(),
			Evidence: []facts.Evidence{{
				File:   f.File,
				Symbol: f.Name,
				Detail: "no measured definition of " + r.method,
			}},
			Actions: []string{
				fmt.Sprintf("Define %s on the class if the rule stands", r.method),
				"Amend the rule on its declaring page if the decision behind it changed",
			},
		})
	}
	return out
}

// verdictRequireName emits one violation per member of the component whose
// name fails the declared bounded pattern — prefix*, *suffix, or an exact
// name, matched with the same intent.MatchBoundedName the validator's
// admission is defined against, so the two cannot part company. A
// name always exists on a member, so the form has no unmeasured case: every
// member is in scope, and the verdict is total over the membership.
func (e *Explainer) verdictRequireName(r rule, memberFacts map[string][]facts.Fact, members map[string]map[string]bool) []facts.Insight {
	if r.requires != "" {
		return e.verdictRequireNamePairs(r, memberFacts, members)
	}
	var out []facts.Insight
	first := firstFactByName(memberFacts[r.requireName])
	for _, name := range sortedMemberNames(members[r.requireName]) {
		if intent.MatchBoundedName(name, r.pattern) {
			continue
		}
		f := first[name]
		out = append(out, facts.Insight{
			Title:       r.titled(fmt.Sprintf("%s does not match %s", name, r.pattern)),
			Description: fmt.Sprintf("%s is a member of %s, so its name must match %s — and it does not. The rule is declared, membership is exact, and the pattern dialect is bounded, so this is a decided-rule breach, not a heuristic. Because: %s", name, r.requireName, r.pattern, r.because),
			Confidence:  r.confidence(),
			Evidence: []facts.Evidence{{
				File:   f.File,
				Symbol: f.Name,
				Detail: "name outside the declared convention " + r.pattern,
			}},
			Actions: []string{
				"Rename the member into the convention if the rule stands",
				"Amend the pattern on its declaring page if the decision behind it changed",
			},
		})
	}
	return out
}

// verdictForbidName is require_name's negative: one violation per member of
// the component whose name matches the declared bounded pattern. The pattern
// is tried against the member's full name and, for a method, its bare method
// name after the owner, so `get_*` reaches `Order#get_total` the way a
// reader means it. With surface: exported only members whose measured
// exported prop is true are in scope, since a private helper is not the
// convention's surface; without it every member is. The same bounded dialect
// and the same matcher, so the two forms cannot disagree about what a
// pattern means.
func (e *Explainer) verdictForbidName(r rule, memberFacts map[string][]facts.Fact, members map[string]map[string]bool) []facts.Insight {
	var out []facts.Insight
	first := firstFactByName(memberFacts[r.forbidName])
	for _, name := range sortedMemberNames(members[r.forbidName]) {
		if !intent.MatchBoundedName(name, r.pattern) && !intent.MatchBoundedName(memberShortName(name), r.pattern) {
			continue
		}
		f := first[name]
		if r.surface == "exported" && !f.PropBool("exported") {
			continue
		}
		out = append(out, facts.Insight{
			Title:       r.titled(fmt.Sprintf("%s matches the forbidden %s", name, r.pattern)),
			Description: fmt.Sprintf("%s is a member of %s, so its name must not match %s — and it does. The rule is declared, membership is exact, and the pattern dialect is bounded, so this is a decided-rule breach, not a heuristic. Because: %s", name, r.forbidName, r.pattern, r.because),
			Confidence:  r.confidence(),
			Evidence: []facts.Evidence{{
				File:   f.File,
				Symbol: f.Name,
				Detail: "name inside the forbidden pattern " + r.pattern,
			}},
			Actions: []string{
				"Rename the member out of the pattern if the rule stands",
				"Amend the pattern on its declaring page if the decision behind it changed",
			},
		})
	}
	return out
}

func memberShortName(name string) string {
	if i := strings.LastIndexAny(name, "#."); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}

// verdictRequireEdge emits one violation per member of the component with no
// measured edge of the via kind in the declared direction — the existential
// form: where every other edge form forbids an edge, this one demands one
// exists, so a member nothing points at (inbound) or that points at nothing
// (outbound) is a breach instead of invisible. With a to component the demand
// narrows to edges whose counterpart is one of its members; without one, any
// measured via-edge satisfies. Measurability fails closed on the extraction
// census: a demanded edge can only be verdicted absent where the snapshot
// demonstrates the searched side's (repo, file-extension) classes source
// via-kind edges at all — outbound skips members whose own class never
// demonstrably sources one, inbound skips every member when the searched
// sources include a class that sources other rule-via kinds but never this
// one, because a missing edge there cannot be told apart from one the
// extractor cannot see. Skipped members are counted in one named advisory,
// never silently compliant, never falsely violated. A source class that
// sources no rule-via edges anywhere is no edge source as far as the store
// can state, so it narrows nothing.
func (e *Explainer) verdictRequireEdge(r rule, graphWalk []facts.Fact, memberFacts, carriers map[string][]facts.Fact, members map[string]map[string]bool, census map[string]map[string]bool, resolve *resolver, ground *grounding) []facts.Insight {
	memberNames := sortedMemberNames(members[r.requireEdge])
	if len(memberNames) == 0 {
		return nil
	}
	first := firstFactByName(memberFacts[r.requireEdge])
	if len(r.whenEdgeTo) > 0 {
		var selected []string
		for _, name := range memberNames {
			if _, ok := factEdgeTo(first[name], r.whenVia, r.whenEdgeTo); ok {
				selected = append(selected, name)
			}
		}
		if len(selected) == 0 {
			return []facts.Insight{requireEdgeAntecedentSkipInsight(r, len(memberNames))}
		}
		memberNames = selected
	}

	satisfied := map[string]bool{}
	skipped := map[string]bool{}
	var blind []string
	if r.direction == "inbound" {
		sourceScope := graphWalk
		if r.to != "" {
			sourceScope = resolve.sources(r, r.to)
		}
		blind = blindSourceClasses(sourceScope, census, r.via)
		if len(blind) > 0 {
			for _, name := range memberNames {
				skipped[name] = true
			}
		} else {
			for _, f := range sourceScope {
				for _, rel := range f.Relations {
					if rel.Kind != r.via {
						continue
					}
					// The demand is satisfied for the MEMBER the target names,
					// which is the target itself when it is a member and the
					// declaring member when the declaration owns its methods.
					if member, named := resolve.memberBehind(r, r.requireEdge, rel.Target); named {
						satisfied[member] = true
						continue
					}
					// A file-granular target satisfies the demand for the member the
					// file it resolves to belongs to, not for the path it spells.
					for _, name := range groundedMembers(rel, f, memberFacts[r.requireEdge], ground) {
						satisfied[name] = true
					}
				}
			}
		}
	} else {
		carrierEdges := map[string][]facts.Relation{}
		for _, f := range carriers[r.requireEdge] {
			carrierEdges[f.File] = append(carrierEdges[f.File], f.Relations...)
		}
		accepted := func(rel facts.Relation, from facts.Fact) bool {
			if rel.Kind != r.via {
				return false
			}
			if r.to == "" {
				return true
			}
			_, landed := resolve.target(r, r.to, rel, from)
			return landed
		}
		measurable := map[string]bool{}
		for _, f := range memberFacts[r.requireEdge] {
			if census[edgeClassOf(f)][r.via] {
				measurable[f.Name] = true
			}
		}
		// The member's own edges, and — where the declaration says so — the edges
		// of its methods, which is where a Ruby class's calls live.
		for _, f := range resolve.sources(r, r.requireEdge) {
			member, own := resolve.memberOfSource(r, r.requireEdge, f)
			if !own {
				continue
			}
			for _, rel := range f.Relations {
				if accepted(rel, f) {
					satisfied[member] = true
				}
			}
		}
		for _, f := range memberFacts[r.requireEdge] {
			// The carrier shares the member's file, so it shares its repository —
			// which is the only thing the file-granular fallback reads off the fact.
			for _, rel := range carrierEdges[f.File] {
				if accepted(rel, f) {
					satisfied[f.Name] = true
				}
			}
		}
		for _, name := range memberNames {
			if !measurable[name] {
				skipped[name] = true
			}
		}
	}

	var out []facts.Insight
	for _, name := range memberNames {
		if satisfied[name] || skipped[name] {
			continue
		}
		f := first[name]
		out = append(out, facts.Insight{
			Title:       r.titled(requireEdgeWitness(r, name)),
			Description: fmt.Sprintf("%s is a member of %s%s, so %s — and the graph measures none. The rule is declared, membership is exact, and %s demonstrably source %s edges elsewhere in this snapshot, so the absence is measured, never extraction blindness. Because: %s", name, r.requireEdge, requireEdgeScope(r), requireEdgeDemand(r), requireEdgeProvers(r), r.via, r.because),
			Confidence:  r.confidence(),
			Evidence: []facts.Evidence{{
				File:   f.File,
				Symbol: f.Name,
				Detail: "no measured " + r.direction + " " + r.via + " edge",
			}},
			Actions: []string{
				requireEdgeAction(r),
				"Amend the rule on its declaring page if the decision behind it changed",
			},
		})
	}
	if len(skipped) > 0 {
		out = append(out, requireEdgeSkipInsight(r, skipped, blind, first))
	}
	return out
}

// requireEdgeScope renders the edge antecedent into the verdict, so a finding
// states why this member was asked and its neighbour was not.
func requireEdgeScope(r rule) string {
	if len(r.whenEdgeTo) == 0 {
		return ""
	}
	return fmt.Sprintf(" that makes a %s edge to %s", r.whenVia, strings.Join(r.whenEdgeTo, " or "))
}

func requireEdgeWitness(r rule, member string) string {
	switch {
	case r.direction == "inbound" && r.to != "":
		return fmt.Sprintf("%s has no inbound %s edge from %s", member, r.via, r.to)
	case r.direction == "inbound":
		return fmt.Sprintf("%s has no inbound %s edge", member, r.via)
	case r.to != "":
		return fmt.Sprintf("%s has no outbound %s edge into %s", member, r.via, r.to)
	default:
		return fmt.Sprintf("%s has no outbound %s edge", member, r.via)
	}
}

func requireEdgeDemand(r rule) string {
	switch {
	case r.direction == "inbound" && r.to != "":
		return fmt.Sprintf("at least one measured %s edge from %s must land on it", r.via, r.to)
	case r.direction == "inbound":
		return fmt.Sprintf("at least one measured %s edge must land on it", r.via)
	case r.to != "":
		return fmt.Sprintf("it must make at least one measured %s edge into %s", r.via, r.to)
	default:
		return fmt.Sprintf("it must make at least one measured %s edge", r.via)
	}
}

func requireEdgeProvers(r rule) string {
	if r.direction == "inbound" {
		return "the searched sources' file kinds"
	}
	return "facts of this member's file kind"
}

func requireEdgeAction(r rule) string {
	if r.direction == "inbound" {
		return "Wire a counterpart that makes the demanded edge, or retire the member, if the rule stands"
	}
	return "Add the demanded edge from the member, or retire it, if the rule stands"
}

// requireEdgeSkipInsight renders one advisory per rule covering every skipped
// member — the honest degrade the reach skip set the shape for: no verdict was
// reached for these members, and the count plus the named cause keep the
// silence visible.
func requireEdgeSkipInsight(r rule, skipped map[string]bool, blind []string, first map[string]facts.Fact) facts.Insight {
	names := sortedMemberNames(skipped)
	evidence := []facts.Evidence{{Fact: "rule: " + r.id, Detail: "declared in " + r.source}}
	for _, name := range names {
		f := first[name]
		evidence = append(evidence, facts.Evidence{
			File:   f.File,
			Symbol: name,
			Detail: r.via + " edge visibility unmeasured",
		})
	}
	cause := fmt.Sprintf("the snapshot measures no %s edges from any fact of the skipped members' file kinds, so a member making none cannot be told apart from one whose extractor does not measure them", r.via)
	if r.direction == "inbound" {
		cause = fmt.Sprintf("the searched sources include file kinds the snapshot measures other edge kinds from but never %s (%s), so a missing %s edge cannot be told apart from one those extractors cannot see", r.via, strings.Join(blind, ", "), r.via)
	}
	return facts.Insight{
		Title:       fmt.Sprintf("require_edge rule %s skipped: %s edge visibility unmeasured for %d member(s)", r.id, r.via, len(names)),
		Description: fmt.Sprintf("The rule demands an %s %s edge for every member of %s, but %s. The skipped member(s), in name order: %s. No verdict was reached for them — skipped, never silently compliant, never falsely violated — and this advisory exists so that the skip cannot be read as compliance. Because: %s", r.direction, r.via, r.requireEdge, cause, strings.Join(names, ", "), r.because),
		Confidence:  edgeSkipConfidence,
		Evidence:    evidence,
		Actions: []string{
			"Regenerate with an extractor that measures " + r.via + " edges for these file kinds if the rule should verdict here",
			"Narrow the component selectors on the declaring page to the measurable members",
		},
	}
}

func (e *Explainer) verdictProtocol(r rule, memberFacts, carriers map[string][]facts.Fact, members map[string]map[string]bool, census map[string]map[string]bool, resolve *resolver) []facts.Insight {
	memberNames := sortedMemberNames(members[r.protocol])
	if len(memberNames) == 0 {
		return nil
	}
	first := firstFactByName(memberFacts[r.protocol])
	carrierEdges := map[string][]facts.Relation{}
	for _, f := range carriers[r.protocol] {
		carrierEdges[f.File] = append(carrierEdges[f.File], f.Relations...)
	}

	touched := map[string]map[int]bool{}
	stepBasis := map[string]basis{}
	madeBasis := map[string]basis{}
	touch := func(member string, from facts.Fact, rel facts.Relation) {
		if rel.Kind != r.via {
			return
		}
		for i, step := range r.steps {
			onto, landed := resolve.target(r, step, rel, from)
			if !landed {
				continue
			}
			if touched[member] == nil {
				touched[member] = map[int]bool{}
			}
			touched[member][i] = true
			if onto > stepBasis[member] {
				stepBasis[member] = onto
			}
		}
	}
	measurable := map[string]bool{}
	for _, f := range memberFacts[r.protocol] {
		if census[edgeClassOf(f)][r.via] {
			measurable[f.Name] = true
		}
		// The carrier shares the member's file, so it shares its repository — the
		// only thing the file-granular fallback reads off the fact.
		for _, rel := range carrierEdges[f.File] {
			touch(f.Name, f, rel)
		}
	}
	// The member's own edges, and the edges of its methods where the
	// declaration says those are the member's.
	for _, f := range resolve.sources(r, r.protocol) {
		member, own := resolve.memberOfSource(r, r.protocol, f)
		if !own {
			continue
		}
		for _, rel := range f.Relations {
			touch(member, f, rel)
			if rel.Kind == r.via && member != f.Name {
				madeBasis[member] = ownedBasis
			}
		}
	}

	skipped := map[string]bool{}
	var out []facts.Insight
	for _, name := range memberNames {
		if !measurable[name] {
			skipped[name] = true
			continue
		}
		reached := touched[name]
		if len(reached) == 0 {
			continue
		}
		highest := 0
		for i := range reached {
			if i > highest {
				highest = i
			}
		}
		var missing []string
		highestMissing := -1
		for i := 0; i < highest; i++ {
			if !reached[i] {
				missing = append(missing, r.steps[i])
				highestMissing = i
			}
		}
		if len(missing) == 0 {
			continue
		}
		f := first[name]
		out = append(out, facts.Insight{
			Title:       r.titled(fmt.Sprintf("%s %s %s without %s", name, r.via, r.steps[highest], r.steps[highestMissing])),
			Description: fmt.Sprintf("%s is a member of %s and makes a measured %s edge into %s, step %d of the declared order %s — so it must also make %s edges into every earlier step, and the graph measures none into %s. This is structural protocol conformance, not runtime ordering: the verdict says the member references a later step's surface without referencing every prerequisite step's surface, which a static fact graph can decide; whether the steps execute in order at runtime it cannot see and does not claim. The rule is declared, %s, and facts of this member's file kind demonstrably source %s edges elsewhere in this snapshot, so each absence is measured, never extraction blindness. Because: %s", name, r.protocol, r.via, r.steps[highest], highest+1, strings.Join(r.steps, " -> "), r.via, strings.Join(missing, ", "), edgeBasis(madeBasis[name], stepBasis[name]), r.via, r.because),
			Confidence:  r.confidence(),
			Evidence: []facts.Evidence{{
				File:   f.File,
				Symbol: f.Name,
				Fact:   r.steps[highestMissing],
				Detail: fmt.Sprintf("reaches step %s with no %s edge into prerequisite %s", r.steps[highest], r.via, r.steps[highestMissing]),
			}},
			Actions: []string{
				fmt.Sprintf("Wire the member's %s edge(s) into %s if the rule stands", r.via, strings.Join(missing, ", ")),
				"Amend the step order on the declaring page if the decision behind it changed",
			},
		})
	}
	if len(skipped) > 0 {
		out = append(out, protocolSkipInsight(r, skipped, first))
	}
	return out
}

func protocolSkipInsight(r rule, skipped map[string]bool, first map[string]facts.Fact) facts.Insight {
	names := sortedMemberNames(skipped)
	evidence := []facts.Evidence{{Fact: "rule: " + r.id, Detail: "declared in " + r.source}}
	for _, name := range names {
		f := first[name]
		evidence = append(evidence, facts.Evidence{
			File:   f.File,
			Symbol: name,
			Detail: r.via + " edge visibility unmeasured",
		})
	}
	return facts.Insight{
		Title:       fmt.Sprintf("protocol rule %s skipped: %s edge visibility unmeasured for %d member(s)", r.id, r.via, len(names)),
		Description: fmt.Sprintf("The rule binds members of %s to the declared step order %s over %s edges, but the snapshot measures no %s edges from any fact of the skipped members' file kinds, so a member referencing no step cannot be told apart from a step-skipping caller whose extractor does not measure the references. The skipped member(s), in name order: %s. No verdict was reached for them — skipped, never silently compliant, never falsely violated — and this advisory exists so that the skip cannot be read as compliance. Because: %s", r.protocol, strings.Join(r.steps, " -> "), r.via, r.via, strings.Join(names, ", "), r.because),
		Confidence:  protocolSkipConfidence,
		Evidence:    evidence,
		Actions: []string{
			"Regenerate with an extractor that measures " + r.via + " edges for these file kinds if the rule should verdict here",
			"Narrow the protocol component's selectors on the declaring page to the measurable members",
		},
	}
}

// edgeClassCensus indexes which (repo, file-extension) classes of the store's
// edge-carrying facts demonstrably source edges, by rule-via kind — the
// measurability oracle the require_edge form fails closed on. A class appears
// only through measured edges, so "this class sources calls edges" is always
// the store's own testimony, never an assumption about what an extractor
// should have done.
func edgeClassCensus(graphWalk []facts.Fact) map[string]map[string]bool {
	viaKind := map[string]bool{}
	for _, kind := range reachVias {
		viaKind[kind] = true
	}
	census := map[string]map[string]bool{}
	for _, f := range graphWalk {
		for _, rel := range f.Relations {
			if !viaKind[rel.Kind] {
				continue
			}
			key := edgeClassOf(f)
			if census[key] == nil {
				census[key] = map[string]bool{}
			}
			census[key][rel.Kind] = true
		}
	}
	return census
}

func edgeClassOf(f facts.Fact) string {
	return f.Repo + "\x00" + strings.ToLower(path.Ext(f.File))
}

// blindSourceClasses lists the scope's (repo, file-extension) classes that the
// census shows sourcing some rule-via kind but never the demanded one —
// demonstrated partial blindness, the one case where absence of the demanded
// edge is unmeasurable rather than measured. A class sourcing no rule-via
// edges anywhere is no edge source as far as the store can state, so it never
// blinds a rule.
func blindSourceClasses(scope []facts.Fact, census map[string]map[string]bool, via string) []string {
	blind := map[string]bool{}
	for _, f := range scope {
		key := edgeClassOf(f)
		if demonstrated := census[key]; len(demonstrated) > 0 && !demonstrated[via] {
			blind[key] = true
		}
	}
	out := make([]string, 0, len(blind))
	for key := range blind {
		out = append(out, describeEdgeClass(key))
	}
	sort.Strings(out)
	return out
}

func describeEdgeClass(key string) string {
	repo, ext, _ := strings.Cut(key, "\x00")
	kind := "extensionless files"
	if ext != "" {
		kind = ext + " files"
	}
	if repo == "" {
		return kind
	}
	return kind + " in " + repo
}

// verdictGuide delivers a guidance rule's finding channel. Notify mode emits
// nothing ever — guidance lives only in the pre-edit contract then. Advisory
// mode emits ONE finding per guided component, at the same below-the-gate 0.9
// every advisory rule reports at, titled so it reads as steering rather than
// as a breach. One finding, never one per member: guidance is not a violation
// census, and a per-member fanout would turn advice into an indictment of
// every file the component contains.
func (e *Explainer) verdictGuide(r rule) []facts.Insight {
	if !r.advisory() {
		return nil
	}
	exemplars := ""
	if len(r.exemplars) > 0 {
		sorted := append([]string(nil), r.exemplars...)
		sort.Strings(sorted)
		exemplars = " Exemplars, in name order: " + strings.Join(sorted, ", ") + "."
	}
	return []facts.Insight{{
		Title:       fmt.Sprintf("Guidance for %s: %s", r.guide, r.id),
		Description: fmt.Sprintf("%s.%s Guidance steers, never enforces: this finding rides the report at advisory confidence and can fail nothing. Because: %s", strings.TrimSuffix(r.message, "."), exemplars, r.because),
		Confidence:  advisoryConfidence,
		Evidence:    []facts.Evidence{{Fact: "component: " + r.guide, Detail: "declared in " + r.source}},
		Actions: []string{
			"Consider the exemplars before writing a new shape in this component",
			"Write a law form on the declaring file if this guidance should graduate to enforcement",
		},
	}}
}

func componentRecipeProvenance(c component) string {
	if c.recipe == "" {
		return ""
	}
	return fmt.Sprintf(" This component binds role %s of recipe %s, instantiated in %s.", c.role, c.recipe, c.source)
}

// resolveMembership selects one component's member facts from the store: the
// canonical name set (what edge targets match against) and the facts
// themselves, sorted by name then file because store order reflects concurrent
// extraction and every walk over a membership must not. A service scope is an
// AND with every other narrowing: members are facts whose repo label equals
// the declared service exactly — the label multi-repo append mode stamps on
// every fact — and a fact with no label matches no service, fail closed. A
// where predicate ANDs in the same place and for the same reason: every
// narrowing on a component narrows, so a component carrying both a path scope
// and a predicate selects their intersection. A name pattern ANDs there too,
// read with intent.MatchBoundedName — the matcher intent.ValidNamePattern
// screens the declaration for, so the family this walk admits is exactly the
// family the declaration was allowed to name. Its starless case is string
// equality, which is what the walk did before the dialect reached it.
func resolveMembership(store *facts.Store, c component) (map[string]bool, []facts.Fact) {
	names := map[string]bool{}
	var members []facts.Fact
	var descendants map[string]bool
	if c.ancestor != "" {
		descendants = newResolvedAncestry(store).descendantsOf(c.ancestor)
	}
	var handlers map[string]bool
	if len(c.handles) > 0 {
		handlers = routeHandlers(store, c.handles)
	}
	var governed map[string]bool
	if c.governedBy != "" {
		governed = governedFiles(store, c.governedBy)
	}
	for _, kind := range membershipKinds(c) {
		for _, f := range store.ByKind(kind) {
			if c.service != "" && f.Repo != c.service {
				continue
			}
			if !matchMemberFile(f, c) {
				continue
			}
			if c.namePattern != "" && !intent.MatchBoundedName(f.Name, c.namePattern) {
				continue
			}
			if !matchesWhere(f, c.where) {
				continue
			}
			if descendants != nil && !descendants[f.Name] {
				continue
			}
			if handlers != nil && !handlers[f.Name] {
				continue
			}
			if governed != nil && !governedMember(governed, f) {
				continue
			}
			names[f.Name] = true
			members = append(members, f)
		}
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].Name != members[j].Name {
			return members[i].Name < members[j].Name
		}
		return members[i].File < members[j].File
	})
	return names, members
}

// membershipKinds lists the fact kinds one component's selector ranges over:
// the measured member kinds, narrowed when the component declares one. A
// kind-unnarrowed service component additionally ranges over the synthetic
// service node the crossrepo linker emits for its repo — the node carries the
// service-to-service depends_on edges, and its name is what those edges target,
// so a whole-service component must contain it for cross-repo rules to see
// them. The node's empty file keeps it out of any path-narrowed component.
func membershipKinds(c component) []string {
	if c.ancestor != "" || len(c.handles) > 0 {
		return []string{facts.KindSymbol}
	}
	for _, kind := range referenceMemberKinds {
		if c.kind == kind {
			return []string{kind}
		}
	}
	kinds := make([]string, 0, len(memberKinds)+1)
	for _, kind := range memberKinds {
		if c.kind == "" || c.kind == kind {
			kinds = append(kinds, kind)
		}
	}
	if c.service != "" && c.kind == "" {
		kinds = append(kinds, facts.KindService)
	}
	return kinds
}

// matchMemberFile joins a fact's file against a component's match patterns. A
// service-scoped component may declare none — the whole service — and a
// predicate-selected one may declare none either, because the predicate is the
// selector and a fact's file is not what it is being asked about; a component
// with neither patterns, service, nor predicate matches nothing, same as before
// either field existed.
func matchMemberFile(f facts.Fact, c component) bool {
	if len(c.match) == 0 {
		return c.service != "" || c.predicated()
	}
	return matchConstraintFile(f, c.match)
}

// carrierFor reports whether a dependency fact's edges walk as sourced from a
// component: its file joins the component's patterns — or, for a whole-service
// component, its repo label matches — without the fact becoming a member. A
// name-narrowed component gets no carriers, whatever its pattern admits: a
// file- or repo-level join cannot prove that a fact the name selector accepts
// made the edge, and a prefix or suffix family is no more demonstrable from a
// file than a single name was. Service scoping ANDs here exactly as it does in
// membership, fail closed on an unlabeled fact.
//
// A predicate component is carried only by a dependency fact that demonstrates
// the predicate ITSELF. Sharing a file with a member is not evidence: the
// property that made the member a member was measured on the member, and no
// other fact in that file has been asked the question. Measured on the
// monolith, 39,601 of the imports edges live on dependency facts and none of
// them carry superclass, symbol_kind, storage_kind or cyclomatic — which is why
// a predicate component cannot be party to an edge form at all, and why
// intent.predicateRoleProblems refuses one at declaration time rather than
// letting this join answer for a reach it does not have.
func carrierFor(f facts.Fact, c component) bool {
	if c.namePattern != "" {
		return false
	}
	if c.service != "" && f.Repo != c.service {
		return false
	}
	if !matchMemberFile(f, c) {
		return false
	}
	if !c.predicated() {
		return true
	}
	return matchesWhere(f, c.where)
}

// oneLevelSuperclassAdvisories reports a component selecting on the superclass
// property whose own members are named as the parent by classes it does not
// contain. `superclass:` is exactly what the extractor wrote down — the parent
// as the source spelled it, one level — so a rule over it judges the base
// classes and says nothing about what is written underneath them. Measured on
// the monolith, `superclass: ViewComponent::Base` names 269 of the 357 classes
// whose ancestry reaches it. Neither existing advisory can see that: the
// component is not empty and the property is measured, so the selector looks
// like it worked.
//
// The witnesses are lexical, like the property they read: a class counts here
// when it wrote a member's fact name as its parent and carries an implements
// edge to that same text. The count is therefore NEITHER a floor NOR a ceiling,
// and it errs in both directions. It misses: a subclass that spelled its parent
// relatively — the unqualified `Base` inside a module — writes text no member's
// fact name equals. And it over-attributes: the index is keyed on the parent AS
// WRITTEN and looked up by a member's RESOLVED fact name, so a module-scoped
// `Base` and a top-level `Base` are one key, and a class inheriting the first
// is named as a subclass of the second. Both are the same fact about the
// property — `superclass` is source text — and no reading of it can be
// transitive or namespace-aware without a resolution pass the extractor did not
// make. The advisory says so rather than claiming a bound it does not have.
func oneLevelSuperclassAdvisories(store *facts.Store, names []string, components map[string]component, members map[string]map[string]bool, unasked, unevaluable map[string]bool) []facts.Insight {
	var selecting []string
	for _, name := range names {
		if unasked[name] || unevaluable[name] {
			continue
		}
		for _, pair := range components[name].where {
			if pair.Prop == superclassProp && len(members[name]) > 0 {
				selecting = append(selecting, name)
				break
			}
		}
	}
	if len(selecting) == 0 {
		return nil
	}
	index := newAncestry(store)
	var out []facts.Insight
	for _, name := range selecting {
		outside := index.outsideChildren(members[name])
		if len(outside) == 0 {
			continue
		}
		c := components[name]
		out = append(out, facts.Insight{
			Title:       fmt.Sprintf("Constraint component %s selects one inheritance level and %d measured subclass(es) fall outside it", name, len(outside)),
			Description: fmt.Sprintf("The component tests the superclass property, which the extractor records exactly as the source wrote it — one level. The snapshot measures %d class(es) that name a member as their parent and are not members themselves, so every rule naming %s judges the parents and says nothing about them: %s. This is not a dead selector and not an unmeasured property; the selector worked and reaches less than the concept it names. The count is lexical, like the property: it is neither a floor nor a ceiling. It misses a subclass that spelled its parent relatively (the unqualified Base inside a module writes text no member's fact name equals), and it over-attributes when two differently-scoped parents were written with the same text, because the index is keyed on the parent as written and read by the member's resolved name. This vocabulary has no transitive spelling: a rule that must cover the hierarchy has to name each level, or the component has to be widened another way.%s", len(outside), name, strings.Join(cappedNames(outside), ", "), componentRecipeProvenance(c)),
			Confidence:  emptyComponentConfidence,
			Evidence: []facts.Evidence{
				{Fact: "component: " + name, Detail: "declared in " + c.source},
				{File: c.source, Fact: "component: " + name, Detail: "the declaring file"},
			},
			Actions: []string{
				fmt.Sprintf("Decide in %s whether the rule is about the classes naming that parent directly, or about the whole hierarchy", c.source),
				"Add the intermediate parents as their own components if the rule must reach the classes below them",
			},
		})
	}
	return out
}

// cappedNames bounds a witness list so a finding stays readable on a component
// with hundreds of subclasses. The count in the sentence is the full one.
func cappedNames(names []string) []string {
	const cap = 8
	if len(names) <= cap {
		return names
	}
	return append(append([]string{}, names[:cap]...), fmt.Sprintf("and %d more", len(names)-cap))
}

// sortFactsByNameThenFile orders a fact slice so every walk over it — and every
// verdict that walk produces — is a function of the graph and never of store
// order.
func sortFactsByNameThenFile(ff []facts.Fact) {
	sort.Slice(ff, func(i, j int) bool {
		if ff[i].Name != ff[j].Name {
			return ff[i].Name < ff[j].Name
		}
		return ff[i].File < ff[j].File
	})
}

// unaskedComponents applies the counterparty rule: a component naming a service
// no loaded fact carries the label of cannot be answered for by this snapshot.
// Shared between the explainer and `constraints lint` so the authoring loop and
// the gate silence the same components for the same reason.
func unaskedComponents(store *facts.Store, components map[string]component) map[string]bool {
	present := map[string]bool{}
	for _, f := range store.All() {
		if f.Repo != "" {
			present[f.Repo] = true
		}
	}
	unasked := map[string]bool{}
	for name, c := range components {
		if c.service != "" && !present[c.service] {
			unasked[name] = true
		}
	}
	return unasked
}

// namesUnasked reports whether a rule names any component in a silenced set —
// unasked (the counterparty rule: one side of the pair is a repo the snapshot
// does not contain) or unevaluable (its predicate names a property nothing
// measures). Either way one silenced side silences the whole rule, because a
// verdict about a pair one half of which was never resolved would be a guess.
func namesUnasked(r rule, unasked map[string]bool) bool {
	for name := range unasked {
		if r.names(name) {
			return true
		}
	}
	return false
}

// propSetContains reports whether a fact's named prop — a space-separated set,
// the shape every compiled census prop uses — contains value as a whole member.
func propSetContains(f facts.Fact, prop, value string) bool {
	for _, member := range strings.Fields(f.PropString(prop)) {
		if member == value {
			return true
		}
	}
	return false
}

// sortedMemberNames orders a membership set, so every verdict that walks one
// walks it the same way on every run.
func sortedMemberNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// firstFactByName indexes a sorted member-fact slice by name, keeping the
// first fact per name — the deterministic representative a name-keyed verdict
// evidences with.
func firstFactByName(sorted []facts.Fact) map[string]facts.Fact {
	first := make(map[string]facts.Fact, len(sorted))
	for _, f := range sorted {
		if _, seen := first[f.Name]; !seen {
			first[f.Name] = f
		}
	}
	return first
}

// intPropOf reads an int prop through the JSON round-trip a restored snapshot
// goes through, where an int survives as a float64. Copied from intentcheck
// rather than shared, for the same reason the path matcher is.
func intPropOf(f facts.Fact, key string) (int, bool) {
	switch v := f.PropAny(key).(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	}
	return 0, false
}

// matchConstraintPath applies the bounded glob dialect: exact path,
// `prefix/**` matching the prefix's whole subtree (and the prefix itself), or
// `**/<name>` matching a file by its own name at any depth. Copied from the
// declared-layer matcher rather than shared, because that ceiling is each
// vocabulary's own decision — widening one must not silently widen the other.
//
// The basename form is read first and exclusively. A pattern opening `**/` is
// a claim about a name, and the subtree branch below would otherwise read
// something like `**/a/**` as a prefix whose first segment is literally `**`.
func matchConstraintPath(path string, patterns []string) bool {
	for _, g := range patterns {
		if glob, ok := strings.CutPrefix(g, intent.BasenameGlobPrefix); ok {
			if matchConstraintBasename(path, glob) {
				return true
			}
			continue
		}
		if prefix, ok := strings.CutSuffix(g, "/**"); ok {
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				return true
			}
			continue
		}
		if path == g {
			return true
		}
	}
	return false
}

// matchConstraintBasename applies a `**/` pattern to the last segment of a
// path — the whole segment, so `*_controller.js` is a filename ending in that
// text and never a directory that does. A depth of zero counts: a file at the
// repository root has a name like any other.
//
// A glob outside the declared grammar matches nothing rather than being read
// some other way. The validator names it an error at declaration time, and a
// pattern that reached the evaluator by some other road — a hand-edited
// snapshot, an older declaration — gets the same answer here, never a
// half-honoured one.
func matchConstraintBasename(path, glob string) bool {
	if !intent.ValidBasenameGlob(glob) {
		return false
	}
	name := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		name = path[i+1:]
	}
	head, tail, starred := strings.Cut(glob, "*")
	if !starred {
		return name == glob
	}
	return len(name) >= len(head)+len(tail) &&
		strings.HasPrefix(name, head) && strings.HasSuffix(name, tail)
}

// matchConstraintFile joins a fact's file against the patterns in both the
// label-prefixed and repo-relative forms — the same double join intentcheck's
// anchors use, because a single trimmed form mis-fires when a real path starts
// with the repo's own name. A fact with no file matches nothing: fail closed.
func matchConstraintFile(f facts.Fact, patterns []string) bool {
	if f.File == "" {
		return false
	}
	if matchConstraintPath(f.File, patterns) {
		return true
	}
	if f.Repo != "" {
		if trimmed := strings.TrimPrefix(f.File, f.Repo+"/"); trimmed != f.File {
			return matchConstraintPath(trimmed, patterns)
		}
	}
	return false
}

// wantedMethods is what a require_defines rule asks for: the single method,
// or the any-of list.
func (r rule) wantedMethods() []string {
	if len(r.anyOf) > 0 {
		return r.anyOf
	}
	return []string{r.method}
}

func (r rule) wantedSentence() string {
	if len(r.anyOf) > 0 {
		return "any of " + strings.Join(r.anyOf, ", ")
	}
	return r.method
}

func definesAny(definedNames map[string]bool, class string, methods []string) bool {
	for _, m := range methods {
		if definedNames[class+"#"+m] || definedNames[class+"."+m] {
			return true
		}
	}
	return false
}

// verdictRequireNamePairs is the pairing reading of require_name: a member
// whose name matches the pattern must have a sibling in the same component
// named by the template with the captured base substituted. The base is what
// the pattern's one * stood for, taken on the member's own part of the name
// so `Order#with_tax` pairs with `Order#without_tax` and not with a method on
// another class.
func (e *Explainer) verdictRequireNamePairs(r rule, memberFacts map[string][]facts.Fact, members map[string]map[string]bool) []facts.Insight {
	var out []facts.Insight
	first := firstFactByName(memberFacts[r.requireName])
	for _, name := range sortedMemberNames(members[r.requireName]) {
		owner, short := splitOwner(name)
		base, ok := capturedBase(short, r.pattern)
		if !ok {
			continue
		}
		sibling := owner + strings.Replace(r.requires, "*", base, 1)
		if members[r.requireName][sibling] {
			continue
		}
		f := first[name]
		out = append(out, facts.Insight{
			Title:       r.titled(fmt.Sprintf("%s has no %s", name, sibling)),
			Description: fmt.Sprintf("%s matches %s, so the convention asks for %s beside it in %s, and no member of that name is measured. The rule is declared and membership is exact, so this is a decided-rule breach, not a heuristic. Because: %s", name, r.pattern, sibling, r.requireName, r.because),
			Confidence:  r.confidence(),
			Evidence:    []facts.Evidence{{File: f.File, Symbol: f.Name, Detail: "no measured " + sibling}},
			Actions: []string{
				fmt.Sprintf("Define %s if the convention stands", sibling),
				"Amend the rule on its declaring page if the decision behind it changed",
			},
		})
	}
	return out
}

func splitOwner(name string) (owner, short string) {
	if i := strings.LastIndexAny(name, "#."); i >= 0 {
		return name[:i+1], name[i+1:]
	}
	return "", name
}

// capturedBase returns what a bounded pattern's one * matched in name.
func capturedBase(name, pattern string) (string, bool) {
	i := strings.Index(pattern, "*")
	if i < 0 {
		return "", false
	}
	prefix, suffix := pattern[:i], pattern[i+1:]
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) || len(name) < len(prefix)+len(suffix) {
		return "", false
	}
	base := name[len(prefix) : len(name)-len(suffix)]
	return base, base != ""
}

// capEvidence carries the count and, when the rule allows growth, the
// allowance, so check can compare the count to the baseline's.
func capEvidence(r rule, evidence []facts.Evidence, count int) []facts.Evidence {
	if r.growth > 0 {
		evidence = append(evidence, facts.Evidence{Fact: fmt.Sprintf("count: %d", count), Detail: "members of " + r.cap})
		evidence = append(evidence, facts.Evidence{Fact: fmt.Sprintf("growth: %d", r.growth), Detail: "allowed over the baseline's count"})
	}
	return evidence
}
