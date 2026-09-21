// Package intentcheck verdicts declared intent against the measured graph —
// the compile step's consumer. It reads only the fact store, like every
// explainer: declarations arrive as KindIntent facts (the compilation the
// engine performs at snapshot time), measured seams as the cross-repo
// dependency facts the linker draws.
//
// Verdicts are per-repo opt-in: a repo with no consumes declarations gets no
// seam verdicts (undeclared is unasked), and a declared seam whose target
// repo is absent from the graph is skipped, never failed — no counterparty
// means no claim is possible. Confidences are honest under the explainer
// doctrine: a mismatch that is pure set difference between stated and
// measured is proof-class (1.0); a missing seam is capped at 0.8, because
// the absent edge can be architectural drift or an extraction miss, and an
// estimate must never present as a certainty.
package intentcheck

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Explainer verdicts intent facts against measured cross-repo edges.
type Explainer struct{}

// New creates the explainer.
func New() *Explainer { return &Explainer{} }

// Name returns the explainer identifier; `check --fail-on=intent` selects it
// by this name.
func (e *Explainer) Name() string { return "intent" }

// missingSeamConfidence caps the one estimating verdict: an absent measured
// edge may be drift or an extraction miss.
const missingSeamConfidence = 0.8

type declaredSeam struct {
	target, via string
	source      string
}

type measuredEdge struct {
	target string
	vias   []string
	name   string
}

// Every scan in this file reads the store through FactsRef, never All.
//
// All returns an independent DEEP copy — every fact's Props map and Relations
// slice cloned — for callers that intend to mutate what they get back. Nothing
// here mutates: the facts that are retained (overridden, anchors, relations) are
// only ever read from, and the props are read through PropString. Six of these
// scans run per snapshot, and on a large graph each All was a full copy of the
// fact set that was read once and dropped. Facts retained from FactsRef share
// their Props map with the store, so a future edit here that WRITES to a prop
// has to take a copy first (Fact.CloneProps) or go back to All.

// Explain computes seam verdicts and override notices.
func (e *Explainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	declared := map[string][]declaredSeam{}
	superseded := map[string][]declaredSeam{}
	var overridden []facts.Fact
	present := map[string]bool{}
	hasIntent := false
	retired := retiredPages(store)

	for _, f := range store.FactsRef() {
		if f.Repo != "" {
			present[f.Repo] = true
		}
		if f.Kind != facts.KindIntent {
			continue
		}
		hasIntent = true
		if f.PropString("overridden") != "" || f.PropAny("overridden") == true {
			overridden = append(overridden, f)
		}
		if f.PropString("intent_kind") != "consumes" {
			continue
		}
		owner := f.PropString("intent_owner")
		if owner == "" {
			owner = f.Repo
		}
		seam := declaredSeam{
			target: f.PropString("target"),
			via:    f.PropString("via"),
			source: f.PropString("source"),
		}
		if retired[seam.source] {
			superseded[owner] = append(superseded[owner], seam)
			continue
		}
		declared[owner] = append(declared[owner], seam)
	}
	if !hasIntent {
		return nil, nil
	}

	measured := map[string][]measuredEdge{}
	for _, f := range store.FactsRef() {
		if f.Kind != facts.KindDependency || f.PropString("type") != "cross_repo" {
			continue
		}
		parts := strings.Split(f.Name, " -> ")
		if len(parts) != 2 {
			continue
		}
		vias, _ := f.PropAny("via").([]string)
		if vias == nil {
			if raw, ok := f.PropAny("via").([]any); ok {
				for _, v := range raw {
					if sv, ok := v.(string); ok {
						vias = append(vias, sv)
					}
				}
			}
		}
		measured[f.Repo] = append(measured[f.Repo], measuredEdge{target: parts[1], vias: vias, name: f.Name})
	}

	var insights []facts.Insight
	repos := make([]string, 0, len(declared))
	for r := range declared {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	supersededSets := map[string]map[string]bool{}
	for repo, seams := range superseded {
		supersededSets[repo] = map[string]bool{}
		for _, s := range seams {
			supersededSets[repo][s.target+"\x00"+s.via] = true
		}
	}

	for _, repo := range repos {
		seams := declared[repo]
		declaredSet := map[string]bool{}
		declaredTargets := map[string]bool{}
		for _, s := range seams {
			declaredSet[s.target+"\x00"+s.via] = true
			declaredTargets[s.target] = true
		}
		measuredByTarget := map[string]map[string]bool{}
		for _, m := range measured[repo] {
			if measuredByTarget[m.target] == nil {
				measuredByTarget[m.target] = map[string]bool{}
			}
			for _, v := range m.vias {
				measuredByTarget[m.target][v] = true
			}
		}

		for _, m := range measured[repo] {
			for _, via := range m.vias {
				if declaredSet[m.target+"\x00"+via] {
					continue
				}
				if supersededSets[repo][m.target+"\x00"+via] {
					continue // the superseded-intent pass reports this with the precise diagnosis
				}
				if declaredTargets[m.target] {
					insights = append(insights, facts.Insight{
						Title:       fmt.Sprintf("Intent mis-via: %s reaches %s via %s", repo, m.target, via),
						Description: fmt.Sprintf("%s declares a seam to %s, but not via %q — the measured edge uses a mechanism the declaration does not name. Correct the declaration or the code; the mismatch itself is exact.", repo, m.target, via),
						Confidence:  1.0,
						Evidence:    []facts.Evidence{{Fact: m.name, Detail: fmt.Sprintf("measured via %s; declared vias for %s do not include it", via, m.target)}},
						Actions:     []string{"Add the via to the declaration if intended", "Remove or migrate the call sites if not"},
					})
				} else {
					insights = append(insights, facts.Insight{
						Title:       fmt.Sprintf("Unexpected seam: %s -> %s via %s", repo, m.target, via),
						Description: fmt.Sprintf("%s declares its consumed seams, and %s is not among them — the graph measures an edge the declaration does not admit. The mismatch is set difference between stated and measured: exact.", repo, m.target),
						Confidence:  1.0,
						Evidence:    []facts.Evidence{{Fact: m.name, Detail: "measured cross-repo edge with no matching declaration"}},
						Actions:     []string{"Declare the seam if it is intended", "Remove the dependency if it is not"},
					})
				}
			}
		}

		for _, s := range seams {
			if !present[s.target] {
				continue // no counterparty in this graph: not asked, never failed
			}
			if measuredByTarget[s.target][s.via] {
				continue
			}
			if declaredTargets[s.target] && len(measuredByTarget[s.target]) > 0 {
				continue // reaches the target by another via: reported above as mis-via
			}
			insights = append(insights, facts.Insight{
				Title:       fmt.Sprintf("Missing intended seam: %s -> %s via %s", repo, s.target, s.via),
				Description: fmt.Sprintf("%s declares it consumes %s via %s, and the graph measures no such edge. Either the architecture drifted from the declaration, or the call is dynamic and extraction cannot see it — this finding cannot tell which, so its confidence is capped.", repo, s.target, s.via),
				Confidence:  missingSeamConfidence,
				Evidence:    []facts.Evidence{{Fact: fmt.Sprintf("consumes %s via %s", s.target, s.via), Detail: "declared in " + s.source}},
				Actions:     []string{"Remove the declaration if the seam is gone", "Record an extraction-gap verdict if the call is dynamic"},
			})
		}
	}

	supRepos := make([]string, 0, len(superseded))
	for r := range superseded {
		supRepos = append(supRepos, r)
	}
	sort.Strings(supRepos)
	for _, repo := range supRepos {
		livingSet := map[string]bool{}
		for _, s := range declared[repo] {
			livingSet[s.target+"\x00"+s.via] = true
		}
		supSet := map[string]string{}
		for _, s := range superseded[repo] {
			supSet[s.target+"\x00"+s.via] = s.source
		}
		seen := map[string]bool{}
		for _, m := range measured[repo] {
			for _, via := range m.vias {
				key := m.target + "\x00" + via
				src, ok := supSet[key]
				if !ok || livingSet[key] || seen[key] {
					continue
				}
				seen[key] = true
				insights = append(insights, facts.Insight{
					Title:       fmt.Sprintf("Superseded intent still measured: %s -> %s via %s", repo, m.target, via),
					Description: "The graph measures this seam, and the only declaration covering it lives on a superseded page — the decision behind it was retired, and no living declaration replaced it. Either the code has not caught up with the superseding decision, or the successor's intent is not declared where enola can see it — this finding cannot tell which, so its confidence is capped.",
					Confidence:  supersededIntentConfidence,
					Evidence:    []facts.Evidence{{Fact: m.name, Detail: "matching declaration is on superseded page " + src}},
					Actions:     []string{"Migrate the code if the superseding decision retires the seam", "Declare the seam on the superseding page if it survives the decision"},
				})
			}
		}
	}

	insights = append(insights, claimVerdicts(store, present, retired)...)
	insights = append(insights, relationVerdicts(store)...)
	insights = append(insights, anchorVerdicts(store, present, retired)...)
	insights = append(insights, dependencyVerdicts(store)...)

	sort.Slice(overridden, func(i, j int) bool {
		if overridden[i].Repo != overridden[j].Repo {
			return overridden[i].Repo < overridden[j].Repo
		}
		return overridden[i].Name < overridden[j].Name
	})
	seenOverride := map[string]bool{}
	for _, f := range overridden {
		if seenOverride[f.Repo] {
			continue
		}
		seenOverride[f.Repo] = true
		insights = append(insights, facts.Insight{
			Title:         fmt.Sprintf("Intent override: cluster config replaces %s's own declaration", f.Repo),
			Description:   fmt.Sprintf("%s carries its own %s, and the cluster config's intent entry overrides it wholesale for this run — the repo file was not consulted. Informational: an override must never be only in a log.", f.Repo, "enola-intent.yaml"),
			Confidence:    1.0,
			Informational: true, // An override is a fact about the run, not a defect in the code.
			Evidence:      []facts.Evidence{{Fact: f.Name, Detail: "declaration source: cluster config (overriding repo file)"}},
			Actions:       []string{"Delete the cluster entry when the repo file should own its declaration"},
		})
	}

	return insights, nil
}

// supersededIntentConfidence caps the retired-intent estimate: a measured
// seam matched only by a superseded page may be code that has not caught up
// with the superseding decision, or a successor whose intent enola cannot
// see.
const supersededIntentConfidence = 0.8

// retiredPages returns the compiled pages retired from current intent, in
// both label-prefixed and repo-relative forms. Two signals mark a page
// retired: an outgoing superseded-by relation (enola's own closed
// vocabulary), or the status token "superseded" — the one status token
// enola reads meaning into; the rest of the status taxonomy stays the
// wiki's. A retired page's seams, claims, anchors and scope stop verdicting
// as current intent; its relations still verdict, because the supersession
// trail itself must not break.
func retiredPages(store *facts.Store) map[string]bool {
	out := map[string]bool{}
	mark := func(f facts.Fact) {
		out[f.File] = true
		out[strings.TrimPrefix(f.File, f.Repo+"/")] = true
	}
	for _, f := range store.FactsRef() {
		if f.Kind != facts.KindIntent {
			continue
		}
		switch f.PropString("intent_kind") {
		case "page":
			if f.PropString("status") == "superseded" {
				mark(f)
			}
		case "relation":
			if f.PropString("rel") == "superseded-by" {
				mark(f)
			}
		}
	}
	return out
}

// claimVerdicts evaluates claim-kind intent facts against the store: exact
// counting for fact-count, edge existence for seam. Only failures become
// findings (a passing claim is silence, like every agreeing verdict), and a
// failure is proof-class — the claim is stated, the count is counted. The
// counterparty rule applies before any counting: a claim about a repo
// absent from the graph is unasked, never failed — "measures 0 because the
// repo is not loaded" must not present as a failed claim (a partial-cluster
// snapshot fails every out-of-graph claim otherwise). Claims on retired
// pages are history, not current intent: skipped.
func claimVerdicts(store *facts.Store, present, retired map[string]bool) []facts.Insight {
	var out []facts.Insight
	all := store.FactsRef()
	for _, f := range all {
		if f.Kind != facts.KindIntent || f.PropString("intent_kind") != "claim" {
			continue
		}
		if retired[f.PropString("source")] {
			continue
		}
		switch f.PropString("metric") {
		case "fact-count":
			owner := f.PropString("intent_owner")
			if !present[owner] {
				continue // no counterparty in this graph: not asked, never failed
			}
			kind := f.PropString("fact_kind")
			fprefix := f.PropString("file_prefix")
			nprefix := f.PropString("name_prefix")
			want, ok := intPropOf(f, "value")
			if !ok {
				continue
			}
			got := 0
			for _, g := range all {
				if g.Kind != kind || g.Repo != owner {
					continue
				}
				gf := strings.TrimPrefix(g.File, owner+"/")
				if fprefix != "" && !strings.HasPrefix(gf, fprefix) {
					continue
				}
				if nprefix != "" && !strings.HasPrefix(g.Name, nprefix) {
					continue
				}
				got++
			}
			if got != want {
				out = append(out, facts.Insight{
					Title:       fmt.Sprintf("Claim failed: %s", f.Name),
					Description: fmt.Sprintf("The page claims %d and the graph measures %d. Either the architecture moved or the claim is stale — the mismatch itself is exact.", want, got),
					Confidence:  1.0,
					Evidence:    []facts.Evidence{{Fact: f.Name, Detail: "claimed in " + f.PropString("source")}},
					Actions:     []string{"Re-measure and update the claim", "Investigate the movement if the claim was current"},
				})
			}
		case "seam":
			owner := f.PropString("intent_owner")
			provider := f.PropString("provider")
			via := f.PropString("via")
			if !present[owner] || !present[provider] {
				continue // no counterparty in this graph: not asked, never failed
			}
			found := false
			for _, g := range all {
				if g.Kind == facts.KindDependency && g.PropString("type") == "cross_repo" &&
					g.Repo == owner && g.Name == owner+" -> "+provider {
					if vias, ok := g.PropAny("via").([]string); ok {
						for _, v := range vias {
							if v == via {
								found = true
							}
						}
					}
				}
			}
			if !found {
				out = append(out, facts.Insight{
					Title:       fmt.Sprintf("Claim failed: %s", f.Name),
					Description: "The claimed seam is not measured in this snapshot.",
					Confidence:  1.0,
					Evidence:    []facts.Evidence{{Fact: f.Name, Detail: "claimed in " + f.PropString("source")}},
					Actions:     []string{"Re-measure; remove or correct the claim if the seam is gone"},
				})
			}
		}
	}
	return out
}

// danglingRelationConfidence caps the knowledge-graph estimate: a relation
// target absent from the compiled set may be a deleted page or a page that
// simply does not opt in — the verdict cannot tell which.
const danglingRelationConfidence = 0.8

// relationVerdicts checks every page relation edge against the compiled page
// set of the same repo. Pages are opt-in, so an absent target is an estimate,
// never a certainty — but in a wiki where every page compiles, a dangling
// edge is a broken link the graph caught.
func relationVerdicts(store *facts.Store) []facts.Insight {
	pages := map[string]bool{}
	var relations []facts.Fact
	for _, f := range store.FactsRef() {
		if f.Kind != facts.KindIntent {
			continue
		}
		switch f.PropString("intent_kind") {
		case "page":
			pages[f.Repo+"\x00"+f.File] = true
			pages[f.Repo+"\x00"+strings.TrimPrefix(f.File, f.Repo+"/")] = true
		case "relation":
			relations = append(relations, f)
		}
	}
	var out []facts.Insight
	for _, f := range relations {
		to := f.PropString("to")
		if pages[f.Repo+"\x00"+to] {
			continue
		}
		out = append(out, facts.Insight{
			Title:       fmt.Sprintf("Dangling knowledge relation: %s", f.Name),
			Description: fmt.Sprintf("The page declares a %s relation to %s, and no compiled page exists at that path. Either the target was deleted or moved, or it does not carry a page declaration — this verdict cannot tell which, so its confidence is capped.", f.PropString("rel"), to),
			Confidence:  danglingRelationConfidence,
			Evidence:    []facts.Evidence{{Fact: f.Name, Detail: "declared in " + f.PropString("source")}},
			Actions:     []string{"Fix the path if the target moved", "Remove the relation if the target is gone", "Add a page declaration to the target if it should compile"},
		})
	}
	return out
}

// danglingAnchorConfidence caps the code-anchor estimate: a path no measured
// fact touches may be moved or deleted, or may simply be a file no extractor
// parses — the verdict cannot tell which.
const danglingAnchorConfidence = 0.8

// anchorVerdicts joins every declared code anchor against the measured graph:
// an anchor holds when some measured fact's file equals the anchored path or
// sits under it (a directory anchor). Unasked and dangling are kept apart on
// two axes, never conflated: a repo absent from the graph is unasked (same
// counterparty rule as seams), and a file anchor whose extension the repo
// never measures — a README, a manifest, a doc — is unasked too, because no
// extractor could have proven it either way. Only a path the graph plausibly
// measures and does not touch is dangling. Files are joined in both the
// label-prefixed and repo-relative forms, same as relation targets — a
// single trimmed form mis-fires when a real path starts with the repo's own
// name. Anchors on retired pages are historical citations: skipped.
func anchorVerdicts(store *facts.Store, present, retired map[string]bool) []facts.Insight {
	var anchors []facts.Fact
	measured := map[string]map[string]bool{}
	measuredExts := map[string]map[string]bool{}
	measuredBases := map[string]map[string]bool{}
	for _, f := range store.FactsRef() {
		if f.Kind == facts.KindIntent {
			if f.PropString("intent_kind") == "anchor" && !retired[f.PropString("source")] {
				anchors = append(anchors, f)
			}
			continue
		}
		if f.Repo == "" || f.File == "" {
			continue
		}
		if measured[f.Repo] == nil {
			measured[f.Repo] = map[string]bool{}
			measuredExts[f.Repo] = map[string]bool{}
			measuredBases[f.Repo] = map[string]bool{}
		}
		measured[f.Repo][f.File] = true
		measured[f.Repo][strings.TrimPrefix(f.File, f.Repo+"/")] = true
		if ext := fileExt(f.File); ext != "" {
			measuredExts[f.Repo][ext] = true
		} else {
			measuredBases[f.Repo][fileBase(f.File)] = true
		}
	}
	var out []facts.Insight
	unasked := map[[2]string]int{}
	for _, f := range anchors {
		owner := f.PropString("intent_owner")
		path := strings.TrimSuffix(f.PropString("path"), "/")
		if owner == "" || path == "" {
			continue
		}
		if !present[owner] {
			// No counterparty in this graph: not asked, never failed. Counted
			// so the silence is reported below rather than left to be noticed.
			unasked[[2]string{f.PropString("source"), owner}]++
			continue
		}
		if ext := fileExt(path); ext != "" {
			if !measuredExts[owner][ext] {
				continue // a file kind this repo's graph never measures: not asked, never failed
			}
		} else if !measuredBases[owner][fileBase(path)] {
			// An extensionless file's kind is its exact basename — a repo
			// measuring Rakefiles has not thereby measured Dockerfiles, and
			// the manifests this rule exists for (Gemfile, Procfile,
			// CODEOWNERS, version dotfiles) are extensionless almost by
			// convention. A directory anchor whose tree nothing measures
			// lands here too: the graph cannot tell an unmeasurable file
			// from a moved directory, and undecidable is unasked, never a
			// finding.
			continue
		}
		files := measured[owner]
		hit := files[path]
		if !hit {
			prefix := path + "/"
			for file := range files {
				if strings.HasPrefix(file, prefix) {
					hit = true
					break
				}
			}
		}
		if hit {
			continue
		}
		out = append(out, facts.Insight{
			Title:       fmt.Sprintf("Dangling code anchor: %s", f.Name),
			Description: fmt.Sprintf("The page anchors to %s in %s, the graph measures files of that kind in that repo, and none touches this path. The code moved or was deleted — or this one file eluded extraction — so the confidence is capped.", path, owner),
			Confidence:  danglingAnchorConfidence,
			Evidence:    []facts.Evidence{{Fact: f.Name, Detail: "declared in " + f.PropString("source")}},
			Actions:     []string{"Fix the path if the code moved", "Remove the anchor if the code is gone", "Record an extraction-gap verdict if the path is real but unparsed"},
		})
	}
	return append(out, unaskedAnchorNotices(unasked, present)...)
}

// absentAnchorRepoConfidence rates the notice that a page's anchors were not
// checked at all. It sits with the other advisories that report a question
// nobody asked rather than an answer.
const absentAnchorRepoConfidence = 0.4

// unaskedAnchorNotices reports the pages whose anchors name a repository this
// snapshot does not carry, so the stale-citation check skipped them.
//
// Skipping is right: a page legitimately anchors code in a repository that was
// not indexed, and accusing its citations of being stale would be a verdict
// about facts nobody measured. What was wrong is that the skip said nothing. A
// snapshot of one repository labels its facts with that repository's DIRECTORY
// name, so a clone, a worktree or a CI checkout under a different name silently
// switches this check off while `governed_by` and `require_governed` keep
// resolving the same anchors — they drop the label when one repository is
// loaded, because there is nothing else an anchor path could mean there. The
// notice is what keeps the two readings from being told apart only by someone
// who already knew to look.
func unaskedAnchorNotices(unasked map[[2]string]int, present map[string]bool) []facts.Insight {
	if len(unasked) == 0 {
		return nil
	}
	labels := make([]string, 0, len(present))
	for label := range present {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	carried := "none"
	if len(labels) > 0 {
		carried = strings.Join(labels, ", ")
	}

	keys := make([][2]string, 0, len(unasked))
	for k := range unasked {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})

	out := make([]facts.Insight, 0, len(keys))
	for _, k := range keys {
		page, owner := k[0], k[1]
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Anchors not checked: %s names repo %s, absent from this snapshot", page, owner),
			Description: fmt.Sprintf("%d anchor(s) on this page name the repo %s, and this snapshot carries %s. Their paths were neither joined nor reported dangling, because a snapshot cannot say whether a citation into a repository it never measured went stale. A single-repository snapshot takes its label from the repository directory's own name, so a clone or a checkout under a different name reaches this state without the declaration changing. Note that governed_by and require_governed still resolve these anchors: with one repository loaded they drop the label, since there is nothing else the path could mean.",
				unasked[k], owner, carried),
			Confidence: absentAnchorRepoConfidence,
			Evidence:   []facts.Evidence{{Fact: "page: " + page, File: page, Detail: "anchors name repo " + owner + "; snapshot carries " + carried}},
			Actions: []string{
				"Index the repository these anchors name beside this one, so the citations are checked",
				"Correct the anchors' repo if they mean the repository this snapshot measured",
			},
		})
	}
	return out
}

// fileExt returns the lowercased extension of a path, "" when it has none.
// A leading-dot name (.gitignore) is a bare name, not an extension.
func fileExt(path string) string {
	base := path[strings.LastIndexByte(path, '/')+1:]
	i := strings.LastIndexByte(base, '.')
	if i <= 0 {
		return ""
	}
	return strings.ToLower(base[i:])
}

// fileBase returns the lowercased final segment of a path — the kind
// identity of an extensionless file.
func fileBase(path string) string {
	return strings.ToLower(path[strings.LastIndexByte(path, '/')+1:])
}

// Scope and affects deliberately do NOT verdict against measured repo
// labels. A page's scope speaks the wiki's own repo vocabulary, and the
// wiki-to-cluster label mapping is the wiki's side of the boundary (a page
// about "ledger" may compile against a cluster labeled
// "ledger-service") — verdicting the raw names here fired on correct pages
// across a whole estate. Keeping the names checkable is the deriving
// toolchain's job, where the mapping is known.

func intPropOf(f facts.Fact, key string) (int, bool) {
	switch v := f.PropAny(key).(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	}
	return 0, false
}
