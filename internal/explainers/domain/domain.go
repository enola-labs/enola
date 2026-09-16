// Package domain reports on what a Rails application's declarations say about
// its data and its API, rather than on the shape of its code.
//
// Every explainer before this one keys off symbols, modules and their
// dependency edges — 341 of the monolith's 341 insights came from layers,
// hotspots, cycles, god-class, exported-surface, complexity and depth. The
// route, storage and association facts the Ruby extractor emits fed nothing.
// These findings ask the questions those facts are the answers to.
package domain

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

const (
	// minInbound keeps small applications quiet: a model needs this many other
	// models pointing at it before its fan-in is worth a second look.
	minInbound = 10
	// stdDevK is how many standard deviations above the mean a model's inbound
	// association count must sit to be reported, matching god-class's shape so
	// the two read consistently.
	stdDevK = 2.0
	// maxHubs caps hub findings, matching the sibling explainers' caps.
	maxHubs = 15
	// maxEvidence caps how many dependents are listed per insight.
	maxEvidence = 8
	// maxHandlerFindings caps unresolved-handler findings. Unlike the hubs this
	// is a defect rather than a shape, so the cap is higher — but a repository
	// where hundreds of handlers do not resolve has one cause, not hundreds of
	// problems, and the summary says so rather than listing them all.
	maxHandlerFindings = 20
	// minOutboundCalls keeps a component off the inventory until it makes enough
	// calls to be a dependency rather than a one-off.
	minOutboundCalls = 2
	// maxOutbound caps the inventory, matching the sibling caps.
	maxOutbound = 20
)

// Explainer reports domain-level findings from route, storage and association
// facts.
type Explainer struct{}

func New() *Explainer { return &Explainer{} }

func (e *Explainer) Name() string { return "domain" }

func (e *Explainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	var out []facts.Insight
	out = append(out, modelHubs(store)...)
	out = append(out, unresolvedHandlers(store)...)
	out = append(out, tablesSharedAcrossNamespaces(store)...)
	out = append(out, outboundIntegrations(store)...)
	if len(out) == 0 {
		// The "nothing found, here is what was examined" note. It is a scope
		// statement rather than a finding list, so it is never capped: truncating
		// an account of what was looked at would misreport the scope.
		return append(out, examinedPopulations(store)...), nil
	}

	// Highest confidence first, so the budget goes to the strongest reading of the
	// domain rather than to whichever shape happened to be appended first.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Confidence > out[j].Confidence })
	out, omitted := common.Cap(out)
	if omitted > 0 {
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Additional domain observations: %d more", omitted),
			Description: fmt.Sprintf(
				"%d further observations about this repository's models, handlers, shared tables and "+
					"outbound integrations are not listed individually. They rank below those above.",
				omitted),
			Confidence:    0.5,
			Informational: true,
		})
	}
	return out, nil
}

// examinedPopulations says what this explainer looked at, and it speaks only
// when nothing else here did.
//
// Three of the four findings above read declarations that only Rails writes —
// an ActiveRecord association, a controller#action handler, a model's table —
// so on a Go or TypeScript service they examine an empty population and say
// nothing. A reader then cannot tell "nothing was found" from "this explainer
// does not examine your language", which is the same asked-versus-agreed
// confusion the whole estate is built to avoid, arriving through the explainer
// door.
//
// It fires only on silence, and that condition is deliberate rather than
// incidental: an insight emitted unconditionally would say one near-identical
// sentence on every repository in the corpus, which is precisely the shape the
// noise measurement condemned in `exported-surface` — one sentence twenty times
// at 100% repetition. Silence is the only case where this sentence carries
// information, and a finding of any kind proves the examination on its own.
func examinedPopulations(store *facts.Store) []facts.Insight {
	associations := len(store.ByKind(facts.KindAssociation))
	handlers, tables := 0, 0
	for _, fact := range store.ByKind(facts.KindRoute) {
		if handler, _ := fact.Props["handler"].(string); strings.Contains(handler, "#") {
			handlers++
		}
	}
	for _, fact := range store.ByKind(facts.KindStorage) {
		if kind, _ := fact.Props["storage_kind"].(string); kind == "model" {
			tables++
		}
	}
	if associations+handlers+tables > 0 {
		// The populations exist and produced no finding, which is a genuine
		// clean result rather than an inapplicable one.
		return nil
	}
	if len(store.ByKind(facts.KindRoute))+len(store.ByKind(facts.KindStorage)) == 0 {
		// Nothing to examine at all. An examiner that examined nothing must not
		// report a confident anything — the rule extcoverage exists to enforce.
		return nil
	}
	return []facts.Insight{{
		Title: "Domain findings do not apply to this repository",
		Description: "Three of this explainer's four findings read declarations only Rails " +
			"writes — associations, controller#action handlers, and a model's table — and " +
			"this repository declares none of them. That is not a clean result on those " +
			"three; they were not asked. Outbound integrations, the fourth, is " +
			"language-agnostic and did run.",
		Confidence: 1.0,
		Evidence: []facts.Evidence{{
			Detail: fmt.Sprintf("examined: %d associations, %d controller handlers, %d model tables",
				associations, handlers, tables),
		}},
	}}
}

// modelHubs reports models an unusual number of other models point at. A hub is
// not a defect — a Company that everything belongs to is a correct design — so
// the finding is framed as a change-risk concentration rather than as something
// to fix, the way the cycles explainer learned to frame an autoloaded cluster.
func modelHubs(store *facts.Store) []facts.Insight {
	inbound := map[string][]string{}
	for _, fact := range store.ByKind(facts.KindAssociation) {
		if macro, _ := fact.Props["macro"].(string); macro == "inherits" {
			continue
		}
		target, _ := fact.Props["target"].(string)
		model, _ := fact.Props["model"].(string)
		if target == "" || model == "" || target == model {
			continue
		}
		inbound[target] = append(inbound[target], model)
	}
	if len(inbound) == 0 {
		return nil
	}

	counts := make([]float64, 0, len(inbound))
	for _, sources := range inbound {
		counts = append(counts, float64(len(sources)))
	}
	threshold := mean(counts) + stdDevK*stdDev(counts)

	type hub struct {
		model   string
		sources []string
	}
	var hubs []hub
	for model, sources := range inbound {
		if len(sources) < minInbound || float64(len(sources)) < threshold {
			continue
		}
		hubs = append(hubs, hub{model: model, sources: dedupeSorted(sources)})
	}
	sort.Slice(hubs, func(i, j int) bool {
		if len(hubs[i].sources) != len(hubs[j].sources) {
			return len(hubs[i].sources) > len(hubs[j].sources)
		}
		return hubs[i].model < hubs[j].model
	})
	if len(hubs) > maxHubs {
		hubs = hubs[:maxHubs]
	}

	var out []facts.Insight
	for _, h := range hubs {
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("%s is referenced by %d other models", h.model, len(h.sources)),
			Description: fmt.Sprintf(
				"%d models declare an association pointing at %s, against a mean of %.1f across this "+
					"application. A hub like this is usually correct rather than wrong — a tenant or an "+
					"account that everything belongs to looks exactly like this — so read it as a "+
					"concentration of change risk: a migration or a validation added here reaches %d other "+
					"models.",
				len(h.sources), h.model, mean(counts), len(h.sources)),
			Confidence: 1,
			Evidence:   evidenceFor(h.sources, "declares an association pointing here"),
			Actions: []string{
				"Before changing this model's schema or validations, check what the associated models assume.",
			},
		})
	}
	return out
}

// unresolvedHandlers reports routes whose controller this repository does not
// define. It is the first finding that can read a route fact, and it exists
// because a handler naming a controller that is not there is either a dead
// endpoint or a renamed class nobody finished renaming — both actionable, and
// neither visible until routes carried handlers.
func unresolvedHandlers(store *facts.Store) []facts.Insight {
	controllers := map[string]bool{}
	for _, fact := range store.ByKind(facts.KindSymbol) {
		if kind, _ := fact.Props["symbol_kind"].(string); kind != facts.SymbolClass {
			continue
		}
		controllers[controllerKey(fact.Name)] = true
	}
	if len(controllers) == 0 {
		return nil
	}

	missing := map[string][]string{}
	for _, fact := range store.ByKind(facts.KindRoute) {
		handler, _ := fact.Props["handler"].(string)
		if handler == "" {
			continue
		}
		if role, _ := fact.Props["role"].(string); role == "client" {
			continue
		}
		controller, _, found := strings.Cut(handler, "#")
		if !found || controller == "" {
			continue
		}
		if controllers[controllerKey(controller)] {
			continue
		}
		missing[controller] = append(missing[controller], fact.Name)
	}
	if len(missing) == 0 {
		return nil
	}

	names := make([]string, 0, len(missing))
	for controller := range missing {
		names = append(names, controller)
	}
	sort.Slice(names, func(i, j int) bool {
		if len(missing[names[i]]) != len(missing[names[j]]) {
			return len(missing[names[i]]) > len(missing[names[j]])
		}
		return names[i] < names[j]
	})

	// A repository where most handlers do not resolve has one cause — an
	// unextracted directory, an engine, a naming convention this extractor does
	// not read — and reporting it once is more useful than reporting it a
	// thousand times.
	total := 0
	for _, routes := range missing {
		total += len(routes)
	}
	if len(names) > maxHandlerFindings {
		return []facts.Insight{{
			Title: fmt.Sprintf("%d routes name a controller this repository does not define", total),
			Description: fmt.Sprintf(
				"%d distinct controllers are named by routes and not found among this repository's classes. "+
					"At that scale it is one cause rather than %d problems — an engine, a directory this "+
					"extractor does not read, or a naming convention it does not follow — and it should be "+
					"diagnosed before any individual route is investigated.",
				len(names), len(names)),
			Confidence: 0.6,
			Evidence:   evidenceFor(names[:maxHandlerFindings], "named by a route, not defined here"),
		}}
	}

	var out []facts.Insight
	for _, controller := range names {
		routes := dedupeSorted(missing[controller])
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("No controller named %s, but %d route(s) point at it", controller, len(routes)),
			Description: fmt.Sprintf(
				"%d route(s) are declared with a handler on %s, and no class by that name exists in this "+
					"repository. Either the endpoint is dead and the declaration outlived its controller, or "+
					"the class was renamed and the route was not.",
				len(routes), controller),
			Confidence: 0.8,
			Evidence:   evidenceFor(routes, "declared with this handler"),
			Actions: []string{
				"Check whether the endpoint still serves anything before deleting either side.",
			},
		})
	}
	return out
}

// tablesSharedAcrossNamespaces reports a physical table reached through models
// in more than one namespace. Rails allows it and it is sometimes deliberate —
// an API resource and an admin resource over one table — but it is also how two
// teams end up owning the same rows without either knowing.
func tablesSharedAcrossNamespaces(store *facts.Store) []facts.Insight {
	byTable := map[string][]string{}
	for _, fact := range store.ByKind(facts.KindStorage) {
		if kind, _ := fact.Props["storage_kind"].(string); kind != "model" {
			continue
		}
		table, _ := fact.Props["table"].(string)
		if table == "" {
			continue
		}
		byTable[table] = append(byTable[table], fact.Name)
	}

	var tables []string
	for table, models := range byTable {
		if len(namespacesOf(models)) > 1 {
			tables = append(tables, table)
		}
	}
	sort.Strings(tables)

	var out []facts.Insight
	for _, table := range tables {
		models := dedupeSorted(byTable[table])
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Table %s is mapped by models in %d namespaces", table, len(namespacesOf(models))),
			Description: fmt.Sprintf(
				"%s is reached through %s. Rails permits this and it is often deliberate — one table behind "+
					"an API resource and an admin resource, or a single-table hierarchy — but it also means a "+
					"validation or a callback added in one namespace does not run for writes made through the "+
					"other.",
				table, strings.Join(models, ", ")),
			Confidence: 0.7,
			Evidence:   evidenceFor(models, "maps this table"),
		})
	}
	return out
}

// controllerKey reduces a controller path and a class name to one spelling, so
// `connect/onboarding_dashboards` and `Connect::OnboardingDashboardsController`
// compare equal.
func controllerKey(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "::", "/")
	name = strings.TrimSuffix(name, "controller")
	name = strings.TrimSuffix(name, "_")
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '_' {
			continue
		}
		b.WriteByte(name[i])
	}
	return b.String()
}

func namespacesOf(models []string) map[string]bool {
	out := map[string]bool{}
	for _, model := range models {
		namespace := ""
		if idx := strings.LastIndex(model, "::"); idx >= 0 {
			namespace = model[:idx]
		}
		out[namespace] = true
	}
	return out
}

// outboundEvidence cites the route fact behind each endpoint label.
//
// It does not use evidenceFor, and that is the whole point. evidenceFor puts the
// string it is given in Evidence.Fact, which means "a fact's name" — and these
// strings are display labels with the HTTP method glued on the front, so every
// one of them named a fact that does not exist. Measured on one repository, 46
// of its evidence entries pointed at nothing. The label still drives the count
// and the prose; only the citation changes, and the method it carried moves into
// the detail so a reader does not lose it.
//
// The route's file and line ride along, which evidenceFor could never supply:
// these findings previously gave a reader no way to reach the call site.
func outboundEvidence(labels []string, byLabel map[string]facts.Fact) []facts.Evidence {
	if len(labels) > maxEvidence {
		labels = labels[:maxEvidence]
	}
	out := make([]facts.Evidence, 0, len(labels))
	for _, label := range labels {
		fact, ok := byLabel[label]
		if !ok {
			continue
		}
		detail := "called from here"
		if method, _ := fact.Props["method"].(string); method != "" {
			detail = method + " " + detail
		}
		out = append(out, facts.Evidence{Fact: fact.Name, File: fact.File, Line: fact.Line, Detail: detail})
	}
	return out
}

func evidenceFor(items []string, detail string) []facts.Evidence {
	if len(items) > maxEvidence {
		items = items[:maxEvidence]
	}
	out := make([]facts.Evidence, 0, len(items))
	for _, item := range items {
		out = append(out, facts.Evidence{Fact: item, Detail: detail})
	}
	return out
}

func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func stdDev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := mean(xs)
	sum := 0.0
	for _, x := range xs {
		sum += (x - m) * (x - m)
	}
	variance := sum / float64(len(xs)-1)
	root := variance
	for i := 0; i < 40; i++ {
		if root == 0 {
			break
		}
		root = 0.5 * (root + variance/root)
	}
	return root
}

// outboundIntegrations inventories what this repository calls on somebody else.
//
// A client call site that matches no route in this repository is not a broken
// call — on the monolith 101 of 106 are outbound, to Fastly, OpenSearch,
// Coresignal, AssemblyAI, Recall, an insights service and eight job-board
// channel partners. Nothing aggregated them, so they read as unmatched routes
// and the question they answer went unasked: which third parties does this
// application depend on, and which files break when one of them changes.
//
// Grouping is by the calling component's directory, because that is where the
// code lives rather than something inferred. The provider hint the extractor
// already derives is reported when it has one.
func outboundIntegrations(store *facts.Store) []facts.Insight {
	type integration struct {
		endpoints []string
		// byLabel keeps the ROUTE FACT behind each display label. The label
		// ("DELETE /v1/datasets/{}") is what the finding counts and prints; the
		// fact is what its evidence has to cite, because evidence names a fact
		// and no fact is called "DELETE /v1/datasets/{}".
		byLabel map[string]facts.Fact
		hints   map[string]bool
	}
	byComponent := map[string]*integration{}

	// The claim is that nothing here serves these, so it has to be checked
	// rather than assumed. Without the check the inventory sweeps up the
	// frontend calling its own backend — /cookie-policy/accept is this
	// application's route, not a third party's.
	served := map[string]bool{}
	for _, fact := range store.ByKind(facts.KindRoute) {
		props := fact.Props
		if role, _ := props["role"].(string); role == "client" {
			continue
		}
		method, _ := props["method"].(string)
		served[endpointKey(method, fact.Name)] = true
	}

	for _, fact := range store.ByKind(facts.KindRoute) {
		props := fact.Props
		if role, _ := props["role"].(string); role != "client" {
			continue
		}
		if isDouble, _ := props["test_double"].(bool); isDouble {
			continue
		}
		if t, _ := props["type"].(string); t == "graphql" {
			continue
		}
		method, _ := props["method"].(string)
		if served[endpointKey(method, fact.Name)] {
			continue
		}
		component := callingComponent(fact)
		if component == "" {
			continue
		}
		entry := byComponent[component]
		if entry == nil {
			entry = &integration{byLabel: map[string]facts.Fact{}, hints: map[string]bool{}}
			byComponent[component] = entry
		}
		label := strings.TrimSpace(method + " " + fact.Name)
		entry.endpoints = append(entry.endpoints, label)
		if _, seen := entry.byLabel[label]; !seen {
			entry.byLabel[label] = fact
		}
		if hint, _ := props["target_hint"].(string); hint != "" {
			entry.hints[hint] = true
		}
	}

	components := make([]string, 0, len(byComponent))
	for component := range byComponent {
		if len(dedupeSorted(byComponent[component].endpoints)) < minOutboundCalls {
			continue
		}
		components = append(components, component)
	}
	sort.Slice(components, func(i, j int) bool {
		a, b := byComponent[components[i]], byComponent[components[j]]
		if len(a.endpoints) != len(b.endpoints) {
			return len(a.endpoints) > len(b.endpoints)
		}
		return components[i] < components[j]
	})
	if len(components) > maxOutbound {
		components = components[:maxOutbound]
	}

	var out []facts.Insight
	for _, component := range components {
		entry := byComponent[component]
		endpoints := dedupeSorted(entry.endpoints)
		named := ""
		if hints := sortedSet(entry.hints); len(hints) > 0 {
			named = fmt.Sprintf(" It names %s.", strings.Join(hints, ", "))
		}
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("%s calls %d outbound endpoint(s)", component, len(endpoints)),
			Description: fmt.Sprintf(
				"%s issues HTTP calls to %d distinct endpoint(s) that no route in this repository serves, "+
					"so they go to a third party or a sibling service.%s This is the dependency that breaks "+
					"when the other side changes, and it is not visible from this repository's own routes.",
				component, len(endpoints), named),
			Confidence: 0.9,
			Evidence:   outboundEvidence(endpoints, entry.byLabel),
		})
	}
	return out
}

// callingComponent is the directory the call is made from, taken from the
// `declares` relation the extractor already attaches.
// endpointKey compares the two dialects of a path: a client writes {} where a
// server writes :id, and neither spelling is a difference in which endpoint is
// meant.
func endpointKey(method, path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment == "{}" || strings.HasPrefix(segment, ":") || strings.HasPrefix(segment, "*") ||
			(strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}")) {
			segments[i] = ":param"
		}
	}
	return strings.ToUpper(method) + " " + strings.TrimSuffix(strings.Join(segments, "/"), "/")
}

func callingComponent(fact facts.Fact) string {
	for _, rel := range fact.Relations {
		if rel.Kind == facts.RelDeclares {
			return rel.Target
		}
	}
	if idx := strings.LastIndex(fact.File, "/"); idx > 0 {
		return fact.File[:idx]
	}
	return ""
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
