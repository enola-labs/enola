package constraints

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
)

// A component's where predicate is evaluated here: the conjunction of property
// tests that decides membership by what a fact carries rather than by where its
// file sits. Every pair must hold — there is no or and no negation — and each
// pair holds when the declared value is one whole member of the prop's measured
// value, which is exactly equality for the scalar props and containment for the
// space-joined set props (columns, fk_constraints, decorators) the require
// form's when_prop_contains already reads that way. One semantic, not two: a
// scalar decomposes into a single token, so "contains that token" and "equals
// that value" are the same statement about it.
//
// A value written as a numeric comparison — ">=5" — is a threshold instead, and
// holds when the prop's measured value is a number satisfying it. The grammar
// is shared with the validator (intent.ParseThreshold), so a threshold that
// parsed at declaration time parses the same way here.

// unmeasuredPropConfidence is deliberately the gate-failing 1.0 rather than the
// 0.4 the dead-selector and absent-service advisories sit at, and the
// difference is the point. Those two report a selector that resolved to nothing
// for a reason the snapshot cannot distinguish from a moved tree or an unloaded
// repo. A where naming a property no measured fact carries is not ambiguous: it
// is a declaration this snapshot cannot evaluate at all, and the resulting
// membership would be empty for a reason that has nothing to do with the code.
// Every rule over an empty component holds, which reads exactly like
// compliance, so this one is stated at full confidence and the rules naming the
// component are silenced rather than allowed to pass vacuously.
const unmeasuredPropConfidence = 1.0

// The causes a selector can be unevaluable for. They are kept apart because the
// remedy differs: a typo is fixed in the declaration, an unmeasured property may
// mean this snapshot simply does not cover that language, a threshold on a
// non-numeric property is a category error, and an undecodable field means the
// compiled predicate does not say what the declaration said.
const (
	CauseUnmeasuredProp      = "unmeasured_property"
	CauseNonNumericThreshold = "non_numeric_threshold"
	CauseUndecodable         = "undecodable_predicate"
	// CauseNoResolvedAncestry: the component selects by ancestor, and no
	// provider contributed resolved ancestry to this snapshot, so the chain
	// the selector walks does not exist here. Read as unevaluable rather than
	// as empty: a hierarchy rule that holds because nobody resolved the
	// hierarchy is the asked-versus-agreed confusion this surface refuses.
	CauseNoResolvedAncestry = "no_resolved_ancestry"
)

// UnevaluableSelector is one component predicate this snapshot cannot answer —
// reported by `constraints lint` before any rule built on it verdicts anything,
// and by the explainer as a 1.0 finding that silences the rules naming it.
type UnevaluableSelector struct {
	Component string   `json:"component"`
	Prop      string   `json:"prop"`
	Value     string   `json:"value,omitempty"`
	Cause     string   `json:"cause"`
	Source    string   `json:"source,omitempty"`
	NearMiss  []string `json:"near_miss,omitempty"`
}

// Problem renders the defect as the sentence both the lint surface and the
// finding state, so the authoring loop and the gate cannot describe the same
// declaration differently.
func (u UnevaluableSelector) Problem() string {
	switch u.Cause {
	case CauseNoResolvedAncestry:
		return fmt.Sprintf("ancestor %s needs resolved ancestry, and no provider contributed any to this snapshot — configure a resolving provider such as rubydex, or select another way", u.Value)
	case CauseNonNumericThreshold:
		return fmt.Sprintf("where compares %s against the threshold %q, and no measured fact carries %s as a number — the comparison can never hold", u.Prop, u.Value, u.Prop)
	case CauseUndecodable:
		return fmt.Sprintf("the compiled predicate carries the field %q, which is not a property test — the declaration and the fact it compiled to do not say the same thing", u.Value)
	default:
		return fmt.Sprintf("where names property %q, which no measured fact carries", u.Prop)
	}
}

// matchesWhere decides one fact against a component's predicate. An absent prop
// fails: the fact cannot demonstrate what the predicate asks about, and a
// missing measurement is never a match.
func matchesWhere(f facts.Fact, where []intent.WherePair) bool {
	for _, pair := range where {
		if !matchesWherePair(f, pair) {
			return false
		}
	}
	return true
}

func matchesWherePair(f facts.Fact, pair intent.WherePair) bool {
	if pair.Unsatisfiable || pair.Prop == "" {
		return false
	}
	if pair.Value == intent.WhereAnyValue {
		return len(propTokens(f, pair.Prop)) > 0
	}
	if op, declared, ok := intent.ParseThreshold(pair.Value); ok {
		measured, numeric := propNumber(f, pair.Prop)
		return numeric && intent.SatisfiesThreshold(op, measured, declared)
	}
	for _, token := range propTokens(f, pair.Prop) {
		if token == pair.Value {
			return true
		}
	}
	return false
}

// propTokens decomposes a measured prop into the whole members a predicate
// compares against: a string splits on whitespace (the space-joined set shape
// every compiled census prop uses, and a single token for every scalar), a list
// yields one token per element, and a number or a bool yields its canonical
// rendering — so `cyclomatic: 10` meets a measured 10 as the same token instead
// of failing silently the way a string-only read would.
func propTokens(f facts.Fact, prop string) []string {
	if f.Props == nil {
		return nil
	}
	switch v := f.PropAny(prop).(type) {
	case string:
		return strings.Fields(v)
	case bool:
		return []string{strconv.FormatBool(v)}
	case int:
		return []string{strconv.Itoa(v)}
	case int64:
		return []string{strconv.FormatInt(v, 10)}
	case float64:
		return []string{strconv.FormatFloat(v, 'f', -1, 64)}
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, element := range v {
			out = append(out, propTokens(facts.Fact{Props: map[string]any{prop: element}}, prop)...)
		}
		return out
	}
	return nil
}

// propNumber reads a prop as the number a threshold compares against. A numeric
// prop survives the snapshot's JSON round-trip as a float64, and an extractor
// that wrote its count as a string is read by parsing it rather than by
// guessing — a parse is a measurement, not an inference.
func propNumber(f facts.Fact, prop string) (float64, bool) {
	if f.Props == nil {
		return 0, false
	}
	switch v := f.PropAny(prop).(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return parsed, err == nil
	}
	return 0, false
}

// propCensus is what one scope of the snapshot measured: which property names
// appear at all, and which of them ever appear as a number. The second half is
// what tells a threshold that can never hold from one that merely does not
// hold here.
type propCensus struct {
	present map[string]bool
	numeric map[string]bool
	facts   int
}

// measureProps censuses one scope. Intent facts are excluded: a component must
// not be able to validate its selector against the declaration that declared
// it. A service scopes the census to that repo's facts, which is the fix for a
// component naming a service whose props only that service measures — censusing
// the whole union would report a property as unmeasured because the OTHER repo
// does not carry it.
func measureProps(store *facts.Store, service string) propCensus {
	census := propCensus{present: map[string]bool{}, numeric: map[string]bool{}}
	for _, f := range store.All() {
		if f.Kind == facts.KindIntent {
			continue
		}
		if service != "" && f.Repo != service {
			continue
		}
		census.facts++
		for prop := range f.Props {
			census.present[prop] = true
			if _, ok := propNumber(f, prop); ok {
				census.numeric[prop] = true
			}
		}
	}
	return census
}

// unevaluableSelectors reports every declared component whose predicate this
// snapshot cannot answer, in component then property order. A scope carrying no
// measured facts at all reports nothing: "absent" and "never looked" must never
// render the same, the same distinction the guidance exemplars draw between
// absent and unmeasured.
//
// Unasked components are skipped outright. A component naming a service the
// snapshot does not contain is already silenced by the counterparty rule, and
// its honest trace is the 0.4 absent-service advisory — reporting it here
// instead replaced "the repo was not loaded" with a 1.0 "the property is not
// measured", which is a different claim about a different thing and the one
// claim this snapshot has no standing to make.
func unevaluableSelectors(store *facts.Store, components map[string]component, unasked map[string]bool) []UnevaluableSelector {
	names := make([]string, 0, len(components))
	var byAncestor []string
	for name, c := range components {
		if unasked[name] {
			continue
		}
		if len(c.where) > 0 {
			names = append(names, name)
		}
		if c.ancestor != "" {
			byAncestor = append(byAncestor, name)
		}
	}
	if (len(names) == 0 && len(byAncestor) == 0) || !storeMeasured(store) {
		return nil
	}
	sort.Strings(names)
	sort.Strings(byAncestor)
	var out []UnevaluableSelector
	if len(byAncestor) > 0 && !newResolvedAncestry(store).any() {
		for _, name := range byAncestor {
			c := components[name]
			out = append(out, UnevaluableSelector{Component: name, Prop: "ancestor", Value: c.ancestor, Cause: CauseNoResolvedAncestry, Source: c.source})
		}
	}
	censuses := map[string]propCensus{}
	censusFor := func(service string) propCensus {
		if census, cached := censuses[service]; cached {
			return census
		}
		census := measureProps(store, service)
		censuses[service] = census
		return census
	}

	for _, name := range names {
		c := components[name]
		census := censusFor(c.service)
		if census.facts == 0 {
			continue
		}
		for _, pair := range c.where {
			switch {
			case pair.Unsatisfiable:
				out = append(out, UnevaluableSelector{Component: name, Value: pair.Value, Cause: CauseUndecodable, Source: c.source})
			case !census.present[pair.Prop]:
				out = append(out, UnevaluableSelector{
					Component: name,
					Prop:      pair.Prop,
					Cause:     CauseUnmeasuredProp,
					Source:    c.source,
					NearMiss:  nearMissProps(pair.Prop, census.present),
				})
			case isThreshold(pair.Value) && !census.numeric[pair.Prop]:
				out = append(out, UnevaluableSelector{
					Component: name,
					Prop:      pair.Prop,
					Value:     pair.Value,
					Cause:     CauseNonNumericThreshold,
					Source:    c.source,
				})
			}
		}
	}
	return out
}

func isThreshold(value string) bool {
	_, _, ok := intent.ParseThreshold(value)
	return ok
}

// UnevaluableSelectors is the lint entry point to the same census the explainer
// verdicts with, so the authoring loop and the gate can never disagree about
// which selector this snapshot cannot evaluate — including which components the
// counterparty rule silenced before the question was even asked.
func UnevaluableSelectors(store *facts.Store) []UnevaluableSelector {
	components, _ := declarations(store)
	return unevaluableSelectors(store, components, unaskedComponents(store, components))
}

// UnaskedComponents is the lint entry point to the counterparty rule: each
// component the snapshot cannot answer for, mapped to the service it names.
// The authoring loop reads it so a component silenced for an absent repo shows
// as unasked rather than as a broken selector — the distinction the gate makes,
// made in the same words, from the same store.
func UnaskedComponents(store *facts.Store) map[string]string {
	components, _ := declarations(store)
	out := map[string]string{}
	for name := range unaskedComponents(store, components) {
		out[name] = components[name].service
	}
	return out
}

// nearMissProps suggests measured property names close enough to the declared
// one to be what the author meant: an edit distance of at most two, or a shared
// prefix of at least four characters. Bounded and sorted, capped at three, so
// the suggestion is a help rather than a second haystack.
func nearMissProps(prop string, measured map[string]bool) []string {
	const distanceCap = 2
	const prefixCap = 4
	var out []string
	for candidate := range measured {
		switch {
		case editDistance(prop, candidate) <= distanceCap:
		case len(prop) >= prefixCap && len(candidate) >= prefixCap && prop[:prefixCap] == candidate[:prefixCap]:
		default:
			continue
		}
		out = append(out, candidate)
	}
	sort.Strings(out)
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, min(current[j-1]+1, previous[j-1]+cost))
		}
		copy(previous, current)
	}
	return previous[len(b)]
}

// unevaluableSelectorTitle names the finding one unevaluable selector produces.
// The title is the diff's identity for a finding, so it carries the component
// and the property and nothing that moves between runs.
func unevaluableSelectorTitle(u UnevaluableSelector) string {
	switch u.Cause {
	case CauseNoResolvedAncestry:
		return fmt.Sprintf("Constraint component %s selects by ancestor %s without resolved ancestry", u.Component, u.Value)
	case CauseNonNumericThreshold:
		return fmt.Sprintf("Constraint component %s compares non-numeric property %s against a threshold", u.Component, u.Prop)
	case CauseUndecodable:
		return fmt.Sprintf("Constraint component %s compiled to a predicate that is not a property test", u.Component)
	default:
		return fmt.Sprintf("Constraint component %s selects on unmeasured property %s", u.Component, u.Prop)
	}
}

// unevaluableSelectorInsight states one unevaluable selector as a finding. It
// names the property rather than the component's member count, because the
// count is zero for a reason no reader could otherwise recover.
func unevaluableSelectorInsight(u UnevaluableSelector, c component) facts.Insight {
	suggestion := ""
	if len(u.NearMiss) > 0 {
		suggestion = fmt.Sprintf(" Measured properties with similar names: %s.", strings.Join(u.NearMiss, ", "))
	}
	return facts.Insight{
		Title:       unevaluableSelectorTitle(u),
		Description: fmt.Sprintf("The component's selector cannot be evaluated against this snapshot: %s. Its membership is therefore empty for a reason that has nothing to do with the code, and every rule naming %s would hold vacuously. Those rules emitted no verdict here: an unevaluable selector must not read as compliance.%s%s", u.Problem(), u.Component, suggestion, componentRecipeProvenance(c)),
		Confidence:  unmeasuredPropConfidence,
		Evidence: []facts.Evidence{
			{Fact: "component: " + u.Component, Detail: "declared in " + c.source},
			{File: c.source, Fact: "component: " + u.Component, Detail: "the declaring file"},
		},
		Actions: []string{
			fmt.Sprintf("Fix the selector in %s", c.source),
			"Select on a property this snapshot measures, or narrow the component with match/service instead",
		},
	}
}

// selectorSummary renders a component's selector for the authoring loop: the
// predicate beside the paths, so `constraints lint` shows what a count came
// from rather than only what it came to.
func selectorSummary(c component) string {
	var parts []string
	if c.service != "" {
		parts = append(parts, "service "+c.service)
	}
	if len(c.match) > 0 {
		parts = append(parts, "match "+strings.Join(c.match, " "))
	}
	if c.kind != "" {
		parts = append(parts, "kind "+c.kind)
	}
	if c.namePattern != "" {
		parts = append(parts, "name "+c.namePattern)
	}
	if c.ancestor != "" {
		parts = append(parts, "ancestor "+c.ancestor)
	}
	for _, pair := range c.where {
		if pair.Unsatisfiable {
			parts = append(parts, "where <undecodable "+pair.Value+">")
			continue
		}
		parts = append(parts, "where "+pair.Prop+"="+pair.Value)
	}
	return strings.Join(parts, ", ")
}

// superclassProp is the measured property the one-level advisory reads. It is
// named here rather than inlined because two surfaces have to agree on it: the
// ancestry index and the advisory that reports what one level left behind.
const superclassProp = "superclass"

// ancestry indexes the measured inheritance the snapshot carries: for each
// superclass name AS IT WAS WRITTEN, the class facts that declared it. The key
// is source text, not a resolved fact name, which is the whole reason nothing
// in this package walks it transitively — a chain crosses the two namespaces at
// every hop.
//
// A link is recorded only where BOTH halves are measured and agree — the class
// fact carries the superclass property AND carries an implements edge to that
// same name. The corroboration is what keeps mixins and interfaces out: Ruby
// puts include/extend/prepend on dependency facts, and a Java class's
// implements clause targets an interface its superclass property never names,
// so requiring the pair narrows the index to inheritance without any rule about
// what a name looks like.
type ancestry struct {
	childrenOf map[string][]string
}

func newAncestry(store *facts.Store) *ancestry {
	a := &ancestry{childrenOf: map[string][]string{}}
	for _, f := range store.ByKind(facts.KindSymbol) {
		parent := f.PropString(superclassProp)
		if parent == "" {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelImplements && rel.Target == parent {
				a.childrenOf[parent] = append(a.childrenOf[parent], f.Name)
				break
			}
		}
	}
	for parent := range a.childrenOf {
		sort.Strings(a.childrenOf[parent])
	}
	return a
}

// outsideChildren lists the classes that wrote a member's name as their parent
// and are not members themselves — what a one-level superclass selector leaves
// unjudged. It is the evidence behind the advisory, so the count in the finding
// is a measurement rather than an estimate. One hop only: the index is keyed by
// written text and a member's name is a resolved fact name, so a second hop
// would be comparing two different namespaces.
func (a *ancestry) outsideChildren(members map[string]bool) []string {
	var out []string
	for parent := range members {
		for _, child := range a.childrenOf[parent] {
			if !members[child] {
				out = append(out, child)
			}
		}
	}
	sort.Strings(out)
	return out
}

// resolvedAncestry indexes the ancestry a provider resolved: every dependency
// fact carrying an implements edge at resolution level resolved, keyed by the
// parent the edge names, so a walk from an ancestor reaches each class whose
// chain includes it. It is separate from the one-level ancestry index because
// it is a different measurement: that one reads superclass text as the source
// wrote it, this one reads a chain a resolver linearised, with mixins in
// resolution order and names already qualified.
type resolvedAncestry struct {
	childrenOf map[string][]string
}

func newResolvedAncestry(store *facts.Store) *resolvedAncestry {
	a := &resolvedAncestry{childrenOf: map[string][]string{}}
	for _, f := range store.ByKind(facts.KindDependency) {
		if f.PropString("resolution_level") != "resolved" {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind != facts.RelImplements {
				continue
			}
			source, ok := strings.CutSuffix(f.Name, " -> "+rel.Target)
			if !ok {
				continue
			}
			if i := strings.LastIndex(source, ": "); i >= 0 {
				source = source[i+2:]
			}
			a.childrenOf[rel.Target] = append(a.childrenOf[rel.Target], source)
		}
	}
	for parent := range a.childrenOf {
		sort.Strings(a.childrenOf[parent])
	}
	return a
}

func (a *resolvedAncestry) any() bool { return len(a.childrenOf) > 0 }

// descendantsOf walks the resolved chains from the named ancestor. The
// provider emits the whole linearised chain per class, so one hop already
// reaches every class whose ancestry includes the name; the walk continues
// anyway, which costs nothing on a complete chain and keeps the reading
// correct for a provider that emitted only direct parents.
func (a *resolvedAncestry) descendantsOf(ancestor string) map[string]bool {
	out := map[string]bool{}
	queue := []string{ancestor}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range a.childrenOf[parent] {
			if out[child] {
				continue
			}
			out[child] = true
			queue = append(queue, child)
		}
	}
	return out
}
