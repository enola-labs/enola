package constraints

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
)

const (
	CauseNoRuntimeCapture  = "no_runtime_capture"
	CauseNoCounterparty    = "no_counterparty"
	CauseNoCompiledPages   = "no_compiled_pages"
	runtimeQueriesPrefix   = "runtime-queries: "
	propUnmatchedByClients = "unmatched_by_clients"
	propMatchedByClients   = "matched_by_clients"
)

// routeHandlers returns the symbols a route with one of the methods reaches
// through handled_by.
func routeHandlers(store *facts.Store, methods []string) map[string]bool {
	wanted := map[string]bool{}
	for _, m := range methods {
		wanted[strings.ToUpper(m)] = true
	}
	out := map[string]bool{}
	for _, f := range store.ByKind(facts.KindRoute) {
		method := strings.ToUpper(f.PropString("method"))
		if !wanted[method] && !wanted["ANY"] {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelHandledBy {
				out[rel.Target] = true
			}
		}
	}
	return out
}

// governedFiles returns the repo-qualified files the pages selected by the
// governed_by value anchor. The selector is a page path or bounded glob,
// optionally followed by status:<status> or supersedes:<page>, walked through
// the compiled page and relation facts.
func governedFiles(store *facts.Store, selector string) map[string]bool {
	pageGlob, qualifier := intent.SplitGovernedBy(selector)
	pages := map[string]bool{}
	status := map[string]string{}
	supersedes := map[string][]string{}
	for _, f := range store.ByKind(facts.KindIntent) {
		switch f.PropString("intent_kind") {
		case "page":
			status[f.PropString("source")] = f.PropString("status")
		case "relation":
			if f.PropString("rel") == "supersedes" {
				supersedes[f.PropString("source")] = append(supersedes[f.PropString("source")], f.PropString("to"))
			}
		}
	}
	wantStatus := strings.TrimPrefix(qualifier, "status:")
	wantSupersedes := strings.TrimPrefix(qualifier, "supersedes:")
	for page := range status {
		if !matchPageGlob(page, pageGlob) {
			continue
		}
		switch {
		case strings.HasPrefix(qualifier, "status:"):
			if status[page] == wantStatus {
				pages[page] = true
			}
		case strings.HasPrefix(qualifier, "supersedes:"):
			for _, to := range supersedes[page] {
				if to == wantSupersedes {
					pages[page] = true
				}
			}
		default:
			pages[page] = true
		}
	}
	out := map[string]bool{}
	single := len(store.RepoLabels()) == 1
	for _, f := range store.ByKind(facts.KindIntent) {
		if f.PropString("intent_kind") != "anchor" || !pages[f.PropString("source")] {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelDependsOn {
				anchorKeys(out, rel.Target, single)
			}
		}
	}
	return out
}

// anchorKeys records a repo-qualified anchor target and, in a snapshot of
// one repository, its bare path too: the page names the repository by its
// remote identity and the snapshot by its directory, and with one
// repository loaded there is nothing else the path could mean.
func anchorKeys(out map[string]bool, target string, single bool) {
	out[target] = true
	if single {
		if i := strings.IndexByte(target, '/'); i > 0 {
			out[target[i+1:]] = true
		}
	}
}

func matchPageGlob(page, glob string) bool {
	if glob == page {
		return true
	}
	if ok, _ := path.Match(glob, page); ok {
		return true
	}
	if strings.HasSuffix(glob, "/**") {
		return strings.HasPrefix(page, strings.TrimSuffix(glob, "**"))
	}
	return false
}

func governedMember(governed map[string]bool, f facts.Fact) bool {
	if f.File == "" {
		return false
	}
	if governed[f.File] {
		return true
	}
	if f.Repo != "" && governed[f.Repo+"/"+f.File] {
		return true
	}
	if i := strings.IndexByte(f.File, '/'); i > 0 && governed[f.File[i+1:]] && f.Repo != "" && strings.HasPrefix(f.File, f.Repo+"/") {
		return true
	}
	return false
}

func anchoredFiles(store *facts.Store) (map[string]bool, bool) {
	out := map[string]bool{}
	compiled := false
	single := len(store.RepoLabels()) == 1
	for _, f := range store.ByKind(facts.KindIntent) {
		switch f.PropString("intent_kind") {
		case "page":
			compiled = true
		case "anchor":
			for _, rel := range f.Relations {
				if rel.Kind == facts.RelDependsOn {
					anchorKeys(out, rel.Target, single)
				}
			}
		}
	}
	return out, compiled
}

func unevaluableRule(r rule, cause, title, description, action string) facts.Insight {
	return facts.Insight{
		Title:       fmt.Sprintf("Constraint rule %s cannot be evaluated: %s", r.id, title),
		Description: description + " Because: " + r.because,
		Confidence:  unmeasuredPropConfidence,
		Evidence:    []facts.Evidence{{Fact: "rule: " + r.id, Detail: "declared in " + r.source + "; cause " + cause}},
		Actions:     []string{action},
	}
}

// verdictStorageStaysHome names every edge from a member to a storage fact's
// model that is not itself a member, with the table the far end owns.
func (e *Explainer) verdictStorageStaysHome(r rule, store *facts.Store, memberFacts map[string][]facts.Fact, members map[string]map[string]bool, resolve *resolver) []facts.Insight {
	tableOf := map[string]string{}
	storageFile := map[string]string{}
	for _, f := range store.ByKind(facts.KindStorage) {
		tableOf[f.Name] = f.PropString("table")
		storageFile[f.Name] = f.File
	}
	if len(tableOf) == 0 {
		return nil
	}
	home := members[r.storageStaysHome]
	var out []facts.Insight
	for _, f := range resolve.sources(r, r.storageStaysHome) {
		if _, sourced := resolve.source(r, r.storageStaysHome, f); !sourced {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind != facts.RelCalls && rel.Kind != facts.RelDependsOn {
				continue
			}
			model := ownerName(rel.Target)
			table, isStorage := tableOf[model]
			if !isStorage || home[model] {
				continue
			}
			out = append(out, facts.Insight{
				Title:       r.titled(fmt.Sprintf("%s reaches table %s through %s", f.Name, table, model)),
				Description: fmt.Sprintf("%s keeps to the storage it owns, and %s %s %s, whose model %s owns table %s outside the part. The rule is declared, the storage fact is measured, and membership is exact, so this is a decided-rule breach, not a heuristic. Because: %s", r.storageStaysHome, f.Name, rel.Kind, rel.Target, model, table, r.because),
				Confidence:  r.confidence(),
				Evidence:    []facts.Evidence{{File: f.File, Symbol: f.Name, Fact: rel.Target, Detail: "reaches storage " + model + " (table " + table + ") declared in " + storageFile[model]}},
				Actions: []string{
					cutForStorage(r, resolve, model, table),
					"Amend the rule on its declaring page if the decision behind it changed",
				},
			})
		}
	}
	return dedupVerdicts(out)
}

func ownerName(name string) string {
	if i := strings.IndexAny(name, "#."); i > 0 {
		return name[:i]
	}
	return name
}

// verdictCapRuntime reads runtime-queries facts for frames inside member
// files and names every frame over the budget; without a capture it refuses.
func (e *Explainer) verdictCapRuntime(r rule, store *facts.Store, members map[string]map[string]bool) []facts.Insight {
	var frames []facts.Fact
	for _, f := range store.ByKind(facts.KindRoute) {
		if strings.HasPrefix(f.Name, runtimeQueriesPrefix) {
			frames = append(frames, f)
		}
	}
	for _, kind := range []string{facts.KindSymbol, facts.KindDependency, facts.KindFileRef} {
		for _, f := range store.ByKind(kind) {
			if strings.HasPrefix(f.Name, runtimeQueriesPrefix) {
				frames = append(frames, f)
			}
		}
	}
	if len(frames) == 0 {
		return []facts.Insight{unevaluableRule(r, CauseNoRuntimeCapture, "no runtime capture in this snapshot",
			"cap_runtime reads the frames a runtime capture measured, and no runtime provider contributed any to this snapshot. The rule emitted no verdict: a budget nobody measured must not read as kept. Produce a capture with the runtime provider's spec-suite runner, configure the provider, then regenerate.",
			"Run the capture and configure the runtime provider, then regenerate the snapshot")}
	}
	memberFiles := map[string]bool{}
	for name := range members[r.capRuntime] {
		for _, f := range store.ByName(name) {
			if f.File != "" {
				memberFiles[f.File] = true
			}
		}
	}
	var out []facts.Insight
	for _, f := range frames {
		if !memberFiles[f.File] {
			continue
		}
		count, ok := propNumber(f, r.metric)
		if !ok || int(count) <= r.max {
			continue
		}
		label := f.PropString("frame_label")
		out = append(out, facts.Insight{
			Title:       r.titled(fmt.Sprintf("%s issues %d %s against a budget of %d", label, int(count), r.metric, r.max)),
			Description: fmt.Sprintf("A runtime capture measured %d %s in the frame %s, and %s keeps each frame within %d. The count is observed, not estimated, so this is a decided-rule breach. Because: %s", int(count), r.metric, label, r.capRuntime, r.max, r.because),
			Confidence:  r.confidence(),
			Evidence:    []facts.Evidence{{File: f.File, Symbol: label, Fact: f.Name, Detail: fmt.Sprintf("%d %s observed via %s", int(count), r.metric, f.PropString("observed_via"))}},
			Actions: []string{
				"Batch or preload what the frame queries so it fits the budget",
				"Raise max on the declaring page if the decision behind it changed",
			},
		})
	}
	return out
}

// verdictRequireConsumer names every member route no client in the snapshot
// calls; in a single-repository snapshot it refuses.
func (e *Explainer) verdictRequireConsumer(r rule, store *facts.Store, memberFacts map[string][]facts.Fact) []facts.Insight {
	if len(store.RepoLabels()) < 2 {
		return []facts.Insight{unevaluableRule(r, CauseNoCounterparty, "no counterparty in this snapshot",
			"require_consumer asks whether a client elsewhere in the cluster calls each route, and this snapshot holds one repository. The rule emitted no verdict: a route with no client in a snapshot that loaded no clients must not read as unconsumed.",
			"Generate the snapshot over the cluster that holds the route's clients")}
	}
	var out []facts.Insight
	for _, f := range memberFacts[r.requireConsumer] {
		if f.Kind != facts.KindRoute || f.PropAny(propUnmatchedByClients) != true {
			continue
		}
		out = append(out, facts.Insight{
			Title:       r.titled(fmt.Sprintf("%s %s has no consumer in the cluster", f.PropString("method"), f.Name)),
			Description: fmt.Sprintf("Every route in %s must have a client, and the cross-repository match found none for %s %s across the loaded repositories. The rule is declared and the match is measured, so this is a decided-rule breach. Because: %s", r.requireConsumer, f.PropString("method"), f.Name, r.because),
			Confidence:  r.confidence(),
			Evidence:    []facts.Evidence{{File: f.File, Symbol: f.Name, Line: f.Line, Detail: "unmatched by every loaded client"}},
			Actions: []string{
				"Retire the route if nothing calls it, or load the repository that does into the snapshot",
				"Amend the rule on its declaring page if the decision behind it changed",
			},
		})
	}
	return out
}

// verdictUniqueAcross names every value of the shared property that members
// in two different repositories carry.
func (e *Explainer) verdictUniqueAcross(r rule, memberFacts map[string][]facts.Fact) []facts.Insight {
	owners := map[string]map[string][]facts.Fact{}
	for _, f := range memberFacts[r.uniqueAcross] {
		value := f.Name
		if r.by != "name" {
			value = f.PropString(r.by)
		}
		if value == "" {
			continue
		}
		if owners[value] == nil {
			owners[value] = map[string][]facts.Fact{}
		}
		owners[value][f.Repo] = append(owners[value][f.Repo], f)
	}
	repos := map[string]bool{}
	for _, f := range memberFacts[r.uniqueAcross] {
		repos[f.Repo] = true
	}
	if len(repos) < 2 {
		return []facts.Insight{unevaluableRule(r, CauseNoCounterparty, "no counterparty in this snapshot",
			"unique_across compares the members of different repositories, and the members of "+r.uniqueAcross+" come from one. The rule emitted no verdict: a property nobody else could share must not read as unique.",
			"Generate the snapshot over the cluster that holds the other repositories")}
	}
	values := make([]string, 0, len(owners))
	for v := range owners {
		values = append(values, v)
	}
	sort.Strings(values)
	var out []facts.Insight
	for _, value := range values {
		if len(owners[value]) < 2 {
			continue
		}
		names := make([]string, 0, len(owners[value]))
		var evidence []facts.Evidence
		for repo := range owners[value] {
			names = append(names, repo)
		}
		sort.Strings(names)
		for _, repo := range names {
			for _, f := range owners[value][repo] {
				evidence = append(evidence, facts.Evidence{File: f.File, Symbol: f.Name, Line: f.Line, Detail: r.by + " " + value + " in " + repo})
			}
		}
		out = append(out, facts.Insight{
			Title:       r.titled(fmt.Sprintf("%s %s is owned by %s", r.by, value, strings.Join(names, " and "))),
			Description: fmt.Sprintf("No two repositories may share a %s among %s, and %s is declared in %s. The rule is declared and every owner is measured, so this is a decided-rule breach. Because: %s", r.by, r.uniqueAcross, value, strings.Join(names, " and "), r.because),
			Confidence:  r.confidence(),
			Evidence:    evidence,
			Actions: []string{
				"Give the " + r.by + " one owner and have the others read it through that owner's interface",
				"Amend the rule on its declaring page if the decision behind it changed",
			},
		})
	}
	return out
}

// verdictRequireGoverned names every member file no compiled page anchors.
func (e *Explainer) verdictRequireGoverned(r rule, store *facts.Store, memberFacts map[string][]facts.Fact) []facts.Insight {
	anchored, compiled := anchoredFiles(store)
	if !compiled {
		return []facts.Insight{unevaluableRule(r, CauseNoCompiledPages, "no compiled pages in this snapshot",
			"require_governed reads the anchors compiled from knowledge pages, and this snapshot carries none. The rule emitted no verdict: a file with no page in a snapshot that compiled no pages must not read as ungoverned.",
			"Index the repository that carries the pages beside this one, then regenerate")}
	}
	files := map[string]facts.Fact{}
	for _, f := range memberFacts[r.requireGoverned] {
		if f.File == "" || governedMember(anchored, f) {
			continue
		}
		if _, seen := files[f.File]; !seen {
			files[f.File] = f
		}
	}
	names := make([]string, 0, len(files))
	for file := range files {
		names = append(names, file)
	}
	sort.Strings(names)
	var out []facts.Insight
	for _, file := range names {
		out = append(out, facts.Insight{
			Title:       r.titled(fmt.Sprintf("%s has no governing page", file)),
			Description: fmt.Sprintf("Every file of %s must be anchored by a knowledge page, and no compiled page anchors %s. The rule is declared and the anchors are compiled from the pages themselves, so this is a decided-rule breach. Because: %s", r.requireGoverned, file, r.because),
			Confidence:  r.confidence(),
			Evidence:    []facts.Evidence{{File: file, Symbol: files[file].Name, Detail: "no anchor from any compiled page"}},
			Actions: []string{
				"Write or extend the page that decides this file and anchor it",
				"Narrow the component if the file is not meant to be governed",
			},
		})
	}
	return out
}

// stampSince records the rule's date on each verdict so check can split the
// breaches against the history revision at that date.
func stampSince(r rule, verdicts []facts.Insight) []facts.Insight {
	for i := range verdicts {
		verdicts[i].Description += fmt.Sprintf(" The rule holds since %s; check grades a breach introduced after that date and reports one present before it.", r.since)
		verdicts[i].Evidence = append(verdicts[i].Evidence, facts.Evidence{Fact: "since: " + r.since, Detail: "rule " + r.id + " dated"})
	}
	return verdicts
}
