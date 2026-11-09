// Package coverage provides an explainer that turns the per-service edge_coverage
// counts recorded by internal/linkers/crossrepo into "coverage gap" insights.
//
// A service node with no outbound dependencies can mean two very different things:
// it is genuinely a leaf, or enola detected outbound call sites it could not
// resolve to a target. This explainer surfaces the latter so a human knows where
// the map is thin and worth verifying against source, rather than assuming the
// service is isolated.
package coverage

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	httpsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/http"
)

// CoverageExplainer emits insights about detected-but-unresolved outbound edges.
type CoverageExplainer struct{}

// New creates a new CoverageExplainer.
func New() *CoverageExplainer {
	return &CoverageExplainer{}
}

// maxUnresolvedCallers caps the breakdown, matching the sibling explainers.
const maxUnresolvedCallers = 8

// unresolvedCallers names the components whose outbound calls match no route in
// any loaded repository, most-calls-first.
//
// It reads the linker's verdict on each call site (unmatched_by_server) rather than
// matching calls to routes itself. The breakdown is evidence for the unresolved
// count beside it, which the linker produced, so it has to count the same call
// sites; a second matcher with its own normalization listed calls the linker had
// resolved through "{id}" against ":id", or through a ".json" suffix.
func unresolvedCallers(store *facts.Store, repo string) []facts.Evidence {
	byComponent := map[string]int{}
	for _, fact := range store.ByKind(facts.KindRoute) {
		if fact.Repo != repo {
			continue
		}
		if role, _ := fact.PropAny("role").(string); role != "client" {
			continue
		}
		if isDouble, _ := fact.PropAny("test_double").(bool); isDouble {
			continue
		}
		if unmatched, _ := fact.PropAny(facts.PropUnmatchedByServer).(bool); !unmatched {
			continue
		}
		component := fact.File
		if idx := strings.LastIndex(component, "/"); idx > 0 {
			component = component[:idx]
		}
		byComponent[component]++
	}
	if len(byComponent) == 0 {
		return nil
	}

	components := make([]string, 0, len(byComponent))
	for component := range byComponent {
		components = append(components, component)
	}
	sort.Slice(components, func(i, j int) bool {
		if byComponent[components[i]] != byComponent[components[j]] {
			return byComponent[components[i]] > byComponent[components[j]]
		}
		return components[i] < components[j]
	})
	if len(components) > maxUnresolvedCallers {
		components = components[:maxUnresolvedCallers]
	}

	out := make([]facts.Evidence, 0, len(components))
	for _, component := range components {
		out = append(out, facts.Evidence{
			Fact:   component,
			Detail: fmt.Sprintf("%d unresolved outbound call site(s) from here", byComponent[component]),
		})
	}
	return out
}

func (e *CoverageExplainer) Name() string {
	return "coverage"
}

// Explain reads the edge_coverage props on service nodes and emits one insight per
// service with open outbound call sites (unresolved, or naming a service the
// snapshot does not hold), distinguishing a coverage gap from a service that is
// merely partially covered by facts.CoverageOf's class. It returns nothing for
// single-repo snapshots (no services).
func (e *CoverageExplainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	var insights []facts.Insight
	for _, svc := range store.ByKind(facts.KindService) {
		c := facts.CoverageOf(svc)
		if c.Open() == 0 {
			continue
		}

		evidence := []facts.Evidence{{Fact: svc.Name, Detail: coverageDetail(c.Entries)}}
		// "External target or unloaded repo" is the honest ambiguity, and it is
		// also the whole question: one means add a repository to the cluster, the
		// other means this is a third party and nothing is missing. Naming the
		// components that make the unresolved calls answers it on sight —
		// lib/opensearch is not a repository anyone forgot.
		reasons := unresolvedReasons(store, svc.Repo)
		evidence = append(evidence, reasonEvidence(svc.Name, reasons)...)
		evidence = append(evidence, unresolvedCallers(store, svc.Repo)...)
		actions := reasonActions(c.Unresolved, reasons, unextractedServers(store, svc.Repo))

		var insight facts.Insight
		if c.Class() == facts.ServiceCoverageGap {
			insight = facts.Insight{
				Title: fmt.Sprintf("Coverage gap: service %s appears isolated but has %s",
					svc.Name, openPhrase(c)),
				Description: fmt.Sprintf("enola detected %d outbound call site(s) from %s and resolved none of them "+
					"to a loaded service: %s. The service has no resolved outbound dependencies, but it is not "+
					"isolated. Details: %s.",
					c.Detected, svc.Name, openPhrase(c), coverageDetail(c.Entries)),
				Confidence: 0.9,
				Evidence:   evidence,
				Actions:    actions,
			}
		} else {
			insight = facts.Insight{
				Title: fmt.Sprintf("Partial coverage: service %s has %s",
					svc.Name, openPhrase(c)),
				Description: fmt.Sprintf("enola resolved some of %s's outbound dependencies but %d of %d detected "+
					"call site(s) did not resolve to a loaded service: %s. Details: %s.",
					svc.Name, c.Open(), c.Detected, openPhrase(c), coverageDetail(c.Entries)),
				Confidence: 0.75,
				Evidence:   evidence,
				Actions:    actions,
			}
		}
		insights = append(insights, insight)
	}
	return append(insights, silentClients(store)...), nil
}

// reasonOrder is the order reasons are reported in: the ones that point at a defect
// in the caller first, then the ones about what is loaded, then enola's own limits.
var reasonOrder = []string{
	httpsignal.ReasonMethodMismatch,
	httpsignal.ReasonVersionMismatch,
	httpsignal.ReasonPathUnknown,
	httpsignal.ReasonAliasNotServing,
	httpsignal.ReasonOutsideSnapshot,
	httpsignal.ReasonAmbiguousProvider,
	httpsignal.ReasonDeclaredTarget,
	httpsignal.ReasonNoMethod,
	httpsignal.ReasonGenericPath,
	httpsignal.ReasonThinPath,
}

// unresolvedReasons counts a repository's unresolved client calls by the reason the
// linker recorded for each (unmatched_reason).
func unresolvedReasons(store *facts.Store, repo string) map[string]int {
	out := map[string]int{}
	for _, fact := range store.ByKind(facts.KindRoute) {
		if fact.Repo != repo {
			continue
		}
		if unmatched, _ := fact.PropAny(facts.PropUnmatchedByServer).(bool); !unmatched {
			continue
		}
		if isDouble, _ := fact.PropAny("test_double").(bool); isDouble {
			continue
		}
		reason, _ := fact.PropAny("unmatched_reason").(string)
		out[reason]++
	}
	return out
}

func reasonEvidence(service string, reasons map[string]int) []facts.Evidence {
	var out []facts.Evidence
	for _, reason := range reasonOrder {
		if n := reasons[reason]; n > 0 {
			out = append(out, facts.Evidence{Fact: service, Detail: fmt.Sprintf("%s: %d call site(s)", reason, n)})
		}
	}
	return out
}

// unextractedServers names the loaded services that serve no route and make no
// outbound call either. When calls go unresolved by path, one of these is the likely
// target, loaded but with its routes unextracted: a backend whose framework no
// extractor reads looks exactly like this. A service that makes calls of its own is a
// client (a mobile or web app) and is not named.
func unextractedServers(store *facts.Store, self string) []string {
	serving := map[string]bool{}
	for _, fact := range store.ByKind(facts.KindRoute) {
		if role, _ := fact.PropAny("role").(string); role != "client" {
			serving[fact.Repo] = true
		}
	}
	var out []string
	for _, svc := range store.ByKind(facts.KindService) {
		if svc.Repo == self || serving[svc.Repo] {
			continue
		}
		if c := facts.CoverageOf(svc); c.Detected > 0 || c.Outbound > 0 {
			continue
		}
		out = append(out, svc.Name)
	}
	sort.Strings(out)
	return out
}

// reasonActions turns the reasons into what to do about them. It used to be one fixed
// sentence telling the reader to append the called service's repository, which is the
// wrong advice when that service is loaded with its routes unextracted, and points away
// from a method mismatch, which is usually a defect in the caller.
func reasonActions(unresolved int, reasons map[string]int, unextracted []string) []string {
	var out []string
	if unresolved > 0 {
		out = append(out, "Verify the unresolved call sites against source")
	}
	for _, reason := range reasonOrder {
		n := reasons[reason]
		if n == 0 {
			continue
		}
		switch reason {
		case httpsignal.ReasonMethodMismatch:
			// Not "likely a defect": on a real cluster the set held all three causes, a
			// caller and server that disagree, a method the extractor read wrong, and a
			// different route that only shares the path's ending.
			out = append(out, fmt.Sprintf("%d call site(s) reach a path a loaded service serves, with a method it does not serve: "+
				"compare the method on both sides (a caller and server that disagree, a method enola read wrong, "+
				"or a different route sharing the path's ending)", n))
		case httpsignal.ReasonVersionMismatch:
			out = append(out, fmt.Sprintf("%d call site(s) use an API version no loaded route serves for that path and method: "+
				"check the version the caller targets", n))
		case httpsignal.ReasonPathUnknown:
			if len(unextracted) > 0 {
				out = append(out, fmt.Sprintf("%d call site(s) match no route in any loaded service, and %s %s loaded but serves no routes: "+
					"if these calls target it, its routes are not being extracted (check that its framework is supported)",
					n, strings.Join(unextracted, ", "), isAre(len(unextracted))))
			} else {
				out = append(out, fmt.Sprintf("%d call site(s) match no route in any loaded service: "+
					"append the repository that serves them, or check that its routes are extracted", n))
			}
		case httpsignal.ReasonAliasNotServing:
			out = append(out, fmt.Sprintf("%d call site(s) go through a configured service alias whose repository serves none of "+
				"the matching routes: check the alias", n))
		case httpsignal.ReasonOutsideSnapshot:
			out = append(out, fmt.Sprintf("%d call site(s) name a service that is not loaded (an env var, a client name): "+
				"add its repository to the snapshot to resolve them", n))
		case httpsignal.ReasonAmbiguousProvider:
			out = append(out, fmt.Sprintf("%d call site(s) are served by more than one loaded repository, and nothing says which "+
				"one is called: declare the target", n))
		case httpsignal.ReasonDeclaredTarget:
			out = append(out, fmt.Sprintf("%d call site(s) are attributed to the declared http-client seam by intent, not by a "+
				"matching route", n))
		}
	}
	if declined := reasons[httpsignal.ReasonNoMethod] + reasons[httpsignal.ReasonGenericPath] + reasons[httpsignal.ReasonThinPath]; declined > 0 {
		out = append(out, fmt.Sprintf("%d call site(s) enola declined to match (no usable method, or a path too short to match "+
			"safely): not evidence of a missing service", declined))
	}
	return out
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// silentClients reports each in-house client the config declares (clients:) that matched
// no call site in any loaded repository.
//
// In one repository, finding nothing is ordinary: a server calls no client. Across every
// loaded repository it is almost always the config, and one of the cheapest mistakes to
// make in it: a receiver type written the way the interface is named rather than the way
// classes inject it. The receiver count says which half of the declaration is wrong.
func silentClients(store *facts.Store) []facts.Insight {
	type tally struct {
		receivers, calls int
		evidence         []facts.Evidence
	}
	bySpec := map[string]*tally{}
	for _, f := range store.ByKind(facts.KindExtraction) {
		spec := f.PropString(facts.PropClientSpec)
		if spec == "" {
			continue
		}
		t := bySpec[spec]
		if t == nil {
			t = &tally{}
			bySpec[spec] = t
		}
		receivers := asInt(f.PropAny("receivers"))
		calls := facts.CoverageOf(f).Detected
		t.receivers += receivers
		t.calls += calls
		where := f.Repo
		if where == "" {
			where = f.File
		}
		t.evidence = append(t.evidence, facts.Evidence{Fact: f.Name,
			Detail: fmt.Sprintf("%s: %d receiver(s), %d call site(s)", where, receivers, calls)})
	}

	specs := make([]string, 0, len(bySpec))
	for spec := range bySpec {
		specs = append(specs, spec)
	}
	sort.Strings(specs)

	var out []facts.Insight
	for _, spec := range specs {
		t := bySpec[spec]
		if t.calls > 0 {
			continue
		}
		sort.Slice(t.evidence, func(i, j int) bool { return t.evidence[i].Detail < t.evidence[j].Detail })
		why := "no class in any loaded repository declares a member of its receiver types"
		action := "Check receiver_types against the type a class injects (constructor parameter or inject() field)"
		if t.receivers > 0 {
			why = fmt.Sprintf("%d member(s) of its receiver types exist, but none of its declared methods is called on them", t.receivers)
			action = "Check the method names under methods against the calls made on those members"
		}
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Declared client %s matched no call site", spec),
			Description: fmt.Sprintf("The config declares the in-house client %s, but %s, so it contributed no client route. "+
				"The dependencies it would carry are missing from the graph, not absent from the code.", spec, why),
			Confidence: 0.9,
			Evidence:   t.evidence,
			Actions:    []string{action},
		})
	}
	return out
}

// openPhrase names a service's open call sites by bucket: the unresolved ones, and
// the ones naming a service the snapshot does not hold, which are not unresolved.
func openPhrase(c facts.ServiceCoverage) string {
	unresolved := fmt.Sprintf("%d unresolved outbound call site(s)", c.Unresolved)
	outside := fmt.Sprintf("%d outbound call site(s) to a service that is not loaded", c.Outside)
	switch {
	case c.Outside == 0:
		return unresolved
	case c.Unresolved == 0:
		return outside
	}
	return fmt.Sprintf("%d unresolved outbound call site(s) and %d to a service that is not loaded", c.Unresolved, c.Outside)
}

// coverageDetail renders the per-edge-type counts for an insight description. The
// external, declared and outside buckets are shown only when present.
func coverageDetail(cov []facts.EdgeCoverage) string {
	out := ""
	for i, c := range cov {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s %d/%d resolved (%d unresolved", c.EdgeType, c.Resolved, c.Detected, c.Unresolved)
		if c.Outside > 0 {
			out += fmt.Sprintf(", %d outside the snapshot", c.Outside)
		}
		if c.External > 0 {
			out += fmt.Sprintf(", %d external", c.External)
		}
		if c.Declared > 0 {
			out += fmt.Sprintf(", %d declared", c.Declared)
		}
		out += ")"
	}
	return out
}

// asInt reads an int-valued prop, tolerating the float64 form that survives a JSON
// round-trip through facts.jsonl.
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}
