package constraints

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// RadiusVerdict is one rule verdict that appears or vanishes once the files
// leave every part: the violation identity the rule titles with, the rule's
// reason, and the cut the verdict proposes.
type RadiusVerdict struct {
	Rule     string           `json:"rule"`
	Title    string           `json:"title"`
	Because  string           `json:"because,omitempty"`
	Cut      string           `json:"cut,omitempty"`
	Evidence []facts.Evidence `json:"evidence,omitempty"`
}

// NotComputed is a rule whose evaluation did not complete in one of the two
// passes. It is listed, never folded into "unchanged": a rule that did not run
// has said nothing about the move.
type NotComputed struct {
	Rule  string `json:"rule"`
	Cause string `json:"cause"`
}

// BlastRadius answers which verdicts change if the named files stopped
// belonging to every part: one constraints re-evaluation over the loaded store
// with those files' facts subtracted from every membership, compared to the
// verdicts the store gives today, by violation identity.
type BlastRadius struct {
	Files       []string        `json:"files"`
	RulesRun    int             `json:"rules_run"`
	Appear      []RadiusVerdict `json:"appear"`
	Vanish      []RadiusVerdict `json:"vanish"`
	NotComputed []NotComputed   `json:"not_computed,omitempty"`
}

// BlastRadiusFor runs the two passes and compares them. Both passes run under
// the guard, so a rule that panics in either is named as not computed in the
// radius and excluded from the comparison.
func BlastRadiusFor(store *facts.Store, files []string) BlastRadius {
	return blastRadius(store, files, nil)
}

// BlastRadiusAgainst compares one guarded pass with the file's facts removed
// against verdicts a snapshot already holds, the same evaluator's output from
// the generate that wrote it, so the answer costs one pass rather than two.
// A snapshot whose insights predate the declarations now on disk is the
// caller's to refuse; the origin line names it.
func BlastRadiusAgainst(store *facts.Store, files []string, snapshot []facts.Insight) BlastRadius {
	byRule := map[string][]facts.Insight{}
	for _, in := range snapshot {
		if in.Source != New().Name() || !isVerdict(in) {
			continue
		}
		id := ruleOfTitle(in.Title)
		byRule[id] = append(byRule[id], in)
	}
	return blastRadius(store, files, byRule)
}

func blastRadius(store *facts.Store, files []string, snapshot map[string][]facts.Insight) BlastRadius {
	trimmed := make([]string, 0, len(files))
	set := map[string]bool{}
	for _, f := range files {
		f = strings.TrimPrefix(f, "./")
		if f == "" || set[f] {
			continue
		}
		set[f] = true
		trimmed = append(trimmed, f)
	}
	sort.Strings(trimmed)
	out := BlastRadius{Files: trimmed, Appear: []RadiusVerdict{}, Vanish: []RadiusVerdict{}}
	exclude := func(f facts.Fact) bool {
		return set[f.File] || set[f.IdentityFile()]
	}
	e := New()
	after := e.evaluate(store, evaluation{guard: true, exclude: exclude})
	before := evaluated{byRule: snapshot, rules: after.rules}
	if snapshot == nil {
		before = e.evaluate(store, evaluation{guard: true})
	}
	out.RulesRun = len(after.rules)

	skipped := map[string]bool{}
	for _, nc := range append(append([]NotComputed{}, before.notComputed...), after.notComputed...) {
		if skipped[nc.Rule] {
			continue
		}
		skipped[nc.Rule] = true
		out.NotComputed = append(out.NotComputed, nc)
	}
	sort.Slice(out.NotComputed, func(i, j int) bool { return out.NotComputed[i].Rule < out.NotComputed[j].Rule })

	because := map[string]string{}
	for _, r := range before.rules {
		because[r.id] = r.because
	}
	for _, r := range before.rules {
		if skipped[r.id] {
			continue
		}
		beforeTitles := verdictTitles(before.byRule[r.id])
		afterTitles := verdictTitles(after.byRule[r.id])
		for _, in := range after.byRule[r.id] {
			if !beforeTitles[in.Title] && isVerdict(in) {
				out.Appear = append(out.Appear, radiusVerdict(r.id, because[r.id], in))
			}
		}
		for _, in := range before.byRule[r.id] {
			if !afterTitles[in.Title] && isVerdict(in) {
				out.Vanish = append(out.Vanish, radiusVerdict(r.id, because[r.id], in))
			}
		}
	}
	sortRadius(out.Appear)
	sortRadius(out.Vanish)
	return out
}

// isVerdict keeps the comparison to breaches: the skip and exemption
// advisories a pass emits describe the pass, not the code, and a move that
// makes a rule skip is reported through the rule's vanished verdicts.
func isVerdict(in facts.Insight) bool {
	return strings.Contains(in.Title, " violated: ")
}

func verdictTitles(insights []facts.Insight) map[string]bool {
	titles := make(map[string]bool, len(insights))
	for _, in := range insights {
		titles[in.Title] = true
	}
	return titles
}

func radiusVerdict(id, because string, in facts.Insight) RadiusVerdict {
	v := RadiusVerdict{Rule: id, Title: in.Title, Because: because, Evidence: in.Evidence}
	if len(in.Actions) > 0 {
		v.Cut = in.Actions[0]
	}
	return v
}

func sortRadius(verdicts []RadiusVerdict) {
	sort.Slice(verdicts, func(i, j int) bool { return verdicts[i].Title < verdicts[j].Title })
}

// ruleOfTitle reads the rule id back out of a verdict title, the inverse of
// rule.titled for the three enforcement weights.
func ruleOfTitle(title string) string {
	for _, prefix := range []string{"Strict constraint ", "Advisory constraint ", "Constraint "} {
		if rest, ok := strings.CutPrefix(title, prefix); ok {
			if id, _, found := strings.Cut(rest, " violated:"); found {
				return id
			}
		}
	}
	return ""
}
