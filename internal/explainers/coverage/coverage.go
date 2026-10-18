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
// service that has unresolved outbound call sites, distinguishing a service that
// appears isolated (no resolved outbound edges at all) from one that is merely
// partially covered. It returns nothing for single-repo snapshots (no services).
func (e *CoverageExplainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	var insights []facts.Insight
	for _, svc := range store.ByKind(facts.KindService) {
		cov := readCoverage(svc)
		var unresolved, detected, resolved int
		for _, c := range cov {
			detected += c.detected
			resolved += c.resolved
			unresolved += c.unresolved
		}
		if unresolved == 0 {
			continue
		}

		outbound := facts.DependsOnCount(svc)
		evidence := []facts.Evidence{{Fact: svc.Name, Detail: coverageDetail(cov)}}
		// "External target or unloaded repo" is the honest ambiguity, and it is
		// also the whole question: one means add a repository to the cluster, the
		// other means this is a third party and nothing is missing. Naming the
		// components that make the unresolved calls answers it on sight —
		// lib/opensearch is not a repository anyone forgot.
		reasons := unresolvedReasons(store, svc.Repo)
		evidence = append(evidence, reasonEvidence(svc.Name, reasons)...)
		evidence = append(evidence, unresolvedCallers(store, svc.Repo)...)
		actions := reasonActions(reasons, unextractedServers(store, svc.Repo))

		var insight facts.Insight
		if facts.ClassifyService(outbound, detected, resolved, unresolved) == facts.ServiceCoverageGap {
			insight = facts.Insight{
				Title: fmt.Sprintf("Coverage gap: service %s appears isolated but has %d unresolved outbound call site(s)",
					svc.Name, unresolved),
				Description: fmt.Sprintf("enola detected %d outbound call site(s) from %s but could not resolve "+
					"%d of them to a loaded service, and the service has no resolved outbound dependencies. "+
					"It may not be isolated — verify these call sites against source. Details: %s.",
					detected, svc.Name, unresolved, coverageDetail(cov)),
				Confidence: 0.9,
				Evidence:   evidence,
				Actions:    actions,
			}
		} else {
			insight = facts.Insight{
				Title: fmt.Sprintf("Partial coverage: service %s has %d unresolved outbound call site(s)",
					svc.Name, unresolved),
				Description: fmt.Sprintf("enola resolved some of %s's outbound dependencies but %d of %d detected "+
					"call site(s) did not resolve to a loaded service (external target or unloaded repo). Details: %s.",
					svc.Name, unresolved, detected, coverageDetail(cov)),
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
	httpsignal.ReasonAmbiguousProvider,
	httpsignal.ReasonDeclaredTarget,
	httpsignal.ReasonNoMethod,
	httpsignal.ReasonGenericPath,
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
		detected := 0
		for _, c := range readCoverage(svc) {
			detected += c.detected
		}
		if detected > 0 || facts.DependsOnCount(svc) > 0 {
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
func reasonActions(reasons map[string]int, unextracted []string) []string {
	out := []string{"Verify the unresolved call sites against source"}
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
		case httpsignal.ReasonAmbiguousProvider:
			out = append(out, fmt.Sprintf("%d call site(s) are served by more than one loaded repository, and nothing says which "+
				"one is called: declare the target", n))
		case httpsignal.ReasonDeclaredTarget:
			out = append(out, fmt.Sprintf("%d call site(s) are attributed to the declared http-client seam by intent, not by a "+
				"matching route", n))
		}
	}
	if declined := reasons[httpsignal.ReasonNoMethod] + reasons[httpsignal.ReasonGenericPath]; declined > 0 {
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
		calls := 0
		for _, c := range readCoverage(f) {
			calls += c.detected
		}
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

// coverageEntry is one edge_type's tally, read back tolerantly from a service node.
type coverageEntry struct {
	edgeType   string
	detected   int
	resolved   int
	unresolved int
	external   int
}

// coverageDetail renders the per-edge-type counts for an insight description. The
// external count (calls to hardcoded third-party hosts) is shown only when present,
// since it is expected rather than a blind spot.
func coverageDetail(cov []coverageEntry) string {
	out := ""
	for i, c := range cov {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s %d/%d resolved (%d unresolved", c.edgeType, c.resolved, c.detected, c.unresolved)
		if c.external > 0 {
			out += fmt.Sprintf(", %d external", c.external)
		}
		out += ")"
	}
	return out
}

// readCoverage extracts the edge_coverage entries from a service node's props,
// tolerating both the in-memory shape ([]map[string]any with int values) and the
// shape that survives a facts.jsonl JSON round-trip ([]any of map[string]any with
// float64 values).
func readCoverage(svc facts.Fact) []coverageEntry {
	if svc.Props == nil {
		return nil
	}
	var raw []map[string]any
	switch v := svc.PropAny("edge_coverage").(type) {
	case []map[string]any:
		raw = v
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				raw = append(raw, m)
			}
		}
	default:
		return nil
	}

	out := make([]coverageEntry, 0, len(raw))
	for _, m := range raw {
		edgeType, _ := m["edge_type"].(string)
		out = append(out, coverageEntry{
			edgeType:   edgeType,
			detected:   asInt(m["detected"]),
			resolved:   asInt(m["resolved"]),
			unresolved: asInt(m["unresolved"]),
			external:   asInt(m["external"]),
		})
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
