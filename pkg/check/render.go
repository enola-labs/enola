package check

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
)

// kindMeaning explains, per warning kind, what it does to the delta — so the output
// says why a baseline is untrustworthy or merely caveated rather than only that it is.
var kindMeaning = map[diff.WarningKind]string{
	diff.WarnDifferentRepo:   "the two snapshots are of different repositories, so the delta is not about your change",
	diff.WarnRepoLabel:       "the same repository is labelled differently on the two sides, so no fact matches across them and the delta describes neither snapshot",
	diff.WarnVersionMismatch: "different enola builds extract or derive differently, so unchanged code can appear as churn",
	diff.WarnExtractorSet:    "a language present on one side only makes all of its facts appear added or removed",
	diff.WarnProviderSet:     "a provider that ran on one side only makes all of its facts appear added or removed",
	diff.WarnExplainerSet:    "an explainer present on one side only makes all of its findings appear new or resolved; the facts and coupling in this delta are unaffected",
	diff.WarnIgnoreGlobs:     "the set of files parsed changed, so some of this delta is exclusion changes, not code changes",
	diff.WarnUnclassified:    "an uncategorized caveat was raised; the gate fails closed rather than grade what it cannot judge",
	diff.WarnStaleBaseline:   "the delta is real, but it also contains whatever the repository itself changed since the baseline was pinned",
	diff.WarnPreReceipt:      "the baseline has no recorded version or extractor set, so comparability could not be fully verified",
	diff.WarnInvertedPair:    "the current snapshot predates the baseline, so it does not contain your change",
}

// DeclineReason is the short "why the gate could not grade this" line, for callers
// that need the reason without the delta — the Stop hook, which must hand an agent
// something actionable in a couple of sentences rather than a full report.
//
// Empty unless the verdict actually declined. It reuses kindMeaning above, so the
// wording an agent is given and the wording `enola check` prints cannot drift apart:
// the two surfaces disagreeing about why a baseline is unusable would be its own
// small betrayal of the exit-code contract.
func (v Verdict) DeclineReason() string {
	if v.Status != StatusIncomparable || len(v.BlockingKinds) == 0 {
		return ""
	}
	parts := make([]string, 0, len(v.BlockingKinds))
	for _, k := range v.BlockingKinds {
		if m := kindMeaning[k]; m != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", k, m))
		} else {
			parts = append(parts, string(k))
		}
	}
	return strings.Join(parts, "; ")
}

// DeclineKey identifies WHICH decline this is, so a caller can tell a repeat from a
// new problem without diffing prose. It is the sorted set of blocking kinds: a
// version mismatch giving way to a different-repository mismatch is a different
// problem and should be reported again, while the same mismatch recurring is not.
func (v Verdict) DeclineKey() string {
	if v.Status != StatusIncomparable || len(v.BlockingKinds) == 0 {
		return ""
	}
	kinds := make([]string, 0, len(v.BlockingKinds))
	for _, k := range v.BlockingKinds {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	return strings.Join(kinds, ",")
}

// ReportKey identifies WHAT this verdict would say out loud, so a caller can tell a
// repeat from something new without diffing prose.
//
// DeclineKey answers the same question for the one path that already had it. The other
// two paths a Stop hook speaks on, a regression and findings no policy enforced, had no
// identity at all, and that omission is what let the identical report re-fire at every
// Stop until the harness overrode the hook.
//
// Keyed on the findings rather than on the rendered text: the render carries counts and
// a delta that move with almost every edit, so prose would make each repeat look new.
// Status is part of the key because the same finding failing and the same finding merely
// being reported are different sentences.
func (v Verdict) ReportKey() string {
	if v.Status == StatusIncomparable {
		return v.DeclineKey()
	}
	findings := v.Failures
	if v.Status != StatusRegression {
		findings = v.UnenforcedAtFloor()
	}
	if len(findings) == 0 {
		return ""
	}
	ids := make([]string, 0, len(findings))
	for _, in := range findings {
		ids = append(ids, in.Source+":"+in.Title)
	}
	sort.Strings(ids)
	return string(v.Status) + "|" + strings.Join(ids, ",")
}

// Render is the human-readable verdict: the headline, why the gate did or did not
// grade, then the delta itself from internal/diff.
func (v Verdict) Render() string {
	var sb strings.Builder

	switch v.Status {
	case StatusClean, StatusPartialClean:
		pass := "PASS"
		warnOnly := "PASS (--warn-only)"
		graded := "no structural regression"
		if v.Status == StatusPartialClean {
			pass = "PASS (partial verdict)"
			warnOnly = "PASS (partial verdict, --warn-only)"
			graded = "no structural regression in the graded intersection"
		}
		switch {
		case len(v.Failures) > 0:
			// --warn-only. Say what the policy WOULD have done: reporting "no structural
			// regression" here would be false, and it is the line a reader skims.
			fmt.Fprintf(&sb, "%s — %s reported, not failed.\n", warnOnly,
				plural(len(v.Failures), "structural regression", "structural regressions"))
		case !v.Policy.Enforcing() && len(v.Advisories) > 0:
			// Nothing was enforced AND the change introduced findings. "No structural
			// regression" would be a lie by omission here: the run had no grounds to call
			// anything a regression, which is not the same as having looked and found none.
			fmt.Fprintf(&sb, "%s — %s reported, nothing enforced: no policy set.\n", pass,
				plural(len(v.Advisories), "new finding", "new findings"))
		case len(v.Advisories) > 0 || len(v.Suppressed) > 0 || len(v.Exempted) > 0 || len(v.Silenced) > 0 || len(v.Undeclared) > 0 || len(v.Declared) > 0 || len(v.Unattributed) > 0 || v.EdgesAdded > 0 || v.FactsAdded > 0 || v.FactsRemoved > 0:
			fmt.Fprintf(&sb, "%s — %s.\n", pass, graded)
		case v.Status == StatusPartialClean:
			fmt.Fprintf(&sb, "%s — no architectural change in the graded intersection.\n", pass)
		default:
			sb.WriteString("PASS — no architectural change.\n")
		}
	case StatusRegression, StatusPartialRegression:
		// Breaches count toward the headline. A change that trips only a measurement
		// threshold has zero failing FINDINGS, and reporting "0 structural regressions
		// introduced" above a FAIL is the kind of contradiction that makes a reader stop
		// believing the first line.
		n := len(v.Failures)
		for _, b := range v.Breaches {
			if b.Fatal {
				n++
			}
		}
		fail := "FAIL"
		if v.Status == StatusPartialRegression {
			fail = "FAIL (partial verdict)"
		}
		fmt.Fprintf(&sb, "%s — %s introduced.\n", fail, plural(n, "structural regression", "structural regressions"))
	case StatusUsageError:
		sb.WriteString("ERROR — the gate could not run.\n")
	case StatusIncomparable:
		sb.WriteString("DECLINED — refusing to grade: the baseline is not comparable to the current snapshot.\n")
		sb.WriteString("This is NOT a statement about your change. The delta below would describe how the two\nsnapshots were produced, not what you edited.\n")
	}

	// What the run could not see, on every outcome, PASS included: a PASS over a
	// graph that skipped the files the change touched must not read like one
	// that resolved them.
	if v.Census != nil {
		sb.WriteString(v.Census.Line() + "\n")
	}

	// How much of the declared law is being excused rather than obeyed, beside
	// what the run could not see, for the same reason: a PASS under a law whose
	// breaches are mostly signed away is not the same run as a PASS under one
	// nobody has had to excuse. Silent where nothing is declared.
	if line := v.Law.Line(); line != "" {
		sb.WriteString(line + "\n")
	}

	v.writeIntersection(&sb)
	v.writeComparability(&sb)
	v.writeBreaches(&sb)

	if len(v.Failures) > 0 {
		// The header has to agree with the verdict. Labelling these "(fail)" under a
		// DECLINED or ERROR status contradicts the headline that just said the gate did
		// not grade — and the whole point of a distinct exit code is that a caller is
		// never told a change is bad when the comparison was untrustworthy.
		var header string
		switch {
		case v.Status == StatusIncomparable || v.Status == StatusUsageError:
			header = "Findings in this delta (NOT graded — see above)"
		case v.Policy.WarnOnly:
			header = "Regressions (would fail; --warn-only)"
		default:
			header = "Regressions (fail)"
		}
		fmt.Fprintf(&sb, "\n%s:\n", header)
		writeFindings(&sb, v.Failures)
		if v.Status != StatusIncomparable && v.Status != StatusUsageError {
			fmt.Fprintf(&sb, "\nPolicy: fail on new findings from [%s] at confidence >= %.2f.\n",
				strings.Join(v.Policy.failExplainers(), ", "), v.Policy.minConfidence())
		}
	}

	if len(v.Advisories) > 0 {
		// "below the failure policy" presumes there is one. With no --fail-on these
		// findings are not below anything — they are simply unenforced, and saying
		// otherwise invites the reader to go looking for the threshold they missed.
		// Enforcing() is the wrong question here: a spillover threshold grades scope,
		// not findings, so it leaves these just as unenforced as no policy at all.
		if len(v.Policy.failExplainers()) > 0 {
			sb.WriteString("\nNew findings (advisory — below the failure policy):\n")
		} else {
			sb.WriteString("\nNew findings (reported — no failure policy set):\n")
		}
		writeFindings(&sb, v.Advisories)
		sb.WriteString(advisoryNote(v.Advisories, v.Policy))
	}

	if len(v.Descriptive) > 0 {
		sb.WriteString("\nDescriptive (never graded) — what the change declared or renamed, not a problem:\n")
		writeFindings(&sb, v.Descriptive)
	}

	if len(v.Suppressed) > 0 {
		// Its own section, never folded into advisories: these findings are real
		// and someone signed them away. The header names the ledger so an auditor
		// knows where the signatures live.
		fmt.Fprintf(&sb, "\nSuppressed (%d) — excused by %s, never failed:\n", len(v.Suppressed), SuppressionsFileName)
		writeFindings(&sb, v.Suppressed)
	}

	if len(v.Exempted) > 0 {
		fmt.Fprintf(&sb, "\nExempted by declaration (%d) — carve-outs the rules themselves declare, never failed:\n", len(v.Exempted))
		writeFindings(&sb, v.Exempted)
	}

	if len(v.Resolved) > 0 {
		fmt.Fprintf(&sb, "\nResolved by this change (%d):\n", len(v.Resolved))
		writeFindings(&sb, v.Resolved)
	}

	if len(v.Silenced) > 0 {
		fmt.Fprintf(&sb, "\nNo longer verdicted (%d) — the code these breaches named is still measured and\nno longer selected by the component its rule binds. The rule lost its subject;\nnothing was fixed:\n", len(v.Silenced))
		writeFindings(&sb, v.Silenced)
	}

	if len(v.Declared) > 0 {
		fmt.Fprintf(&sb, "\nNewly declared (%d) — a rule this change declares, re-forms or un-exempts\nreports these on code the change did not touch. The baseline the rule starts\nfrom, not regressions the change made:\n", len(v.Declared))
		writeFindings(&sb, v.Declared)
	}
	if len(v.Undeclared) > 0 {
		fmt.Fprintf(&sb, "\nNo longer declared (%d) — the rule that reported these was deleted, re-formed\nunder the same id, or carved out by an exemption. The breaching code is\nunchanged; the law stopped asking:\n", len(v.Undeclared))
		writeFindings(&sb, v.Undeclared)
	}

	if len(v.Unattributed) > 0 {
		fmt.Fprintf(&sb, "\nNot attributable to this change (%d) — the repository these breaches were\nmeasured in is absent from this snapshot, or the baseline carried the finding\nwithout the declaration that produced it. Nothing here was compared; whether\nthe code was fixed is not something these two snapshots can say:\n", len(v.Unattributed))
		writeFindings(&sb, v.Unattributed)
	}

	v.writeGuidance(&sb)
	v.writeReviewers(&sb)

	// Findings first (graded, then resolved, then merely moved), structure after: the
	// reader is asking "is anything wrong?" before "what did I touch?".
	v.writeAlteredFindings(&sb)

	if len(v.Incidental) > 0 {
		fmt.Fprintf(&sb, "\nIncidental shifts (%d) — findings that moved with no structural cause in this\nchange (a drifting statistical threshold or a re-ranked list). Never graded.\n",
			len(v.Incidental))
	}

	v.writeWhatChanged(&sb)

	return sb.String()
}

// advisoryNote explains why the findings just listed did not fail the build. There are
// exactly two reasons, and one line cannot honestly cover both: a finding under the
// confidence floor is an estimate, while a finding that MET the floor landed here only
// because its explainer is outside --fail-on.
//
// The single line this replaced said "Confidence < 1.00 is a candidate to verify" over
// lists whose every entry read `1.00` — a declared-layer violation or an intent set
// difference, both proven by construction. The contradiction was visible one line above
// the claim, which is the worst place for a tool that sells exactness to be sloppy. The
// floor is also printed rather than hardcoded, so --min-confidence=0.5 no longer prints
// a sentence about 1.00.
func advisoryNote(ins []facts.Insight, p Policy) string {
	floor := p.minConfidence()
	// No --fail-on at all. The two branches below both end "...outside [%s]", which with
	// an empty policy renders as "outside []" — a bracket pair the reader has to decode
	// into "nothing was enforced". Say it in words instead, and say what to type.
	//
	// The two cases differ by one clause that has to be right: with a --max-spillover
	// threshold set, this run CAN fail, and it may already have — printing "nothing in
	// this run could fail the build" under a FAIL headline is the flattest contradiction
	// the report is capable of.
	if len(p.failExplainers()) == 0 {
		// Counted from the same registry --fail-on validates against, so this can never
		// name a stale total — see pkg/command/check.go's usage text for the sibling fix.
		n := len(config.KnownExplainers)
		if len(p.Thresholds) > 0 {
			return "\nNo --fail-on policy is set, so no FINDING could fail this run — only the threshold\n" +
				"above grades it. These are reported for you to judge; enforce the ones you want\n" +
				fmt.Sprintf("enforced: --fail-on=layers (`enola check --help` lists all %d).\n", n)
		}
		return "\nNo --fail-on policy is set, so nothing in this run could fail the build. These are\n" +
			"reported for you to judge. Enforce the ones you want enforced: --fail-on=layers\n" +
			fmt.Sprintf("(`enola check --help` lists all %d).\n", n)
	}
	var belowFloor, metFloor bool
	for _, in := range ins {
		if in.Confidence < floor {
			belowFloor = true
		} else {
			metFloor = true
		}
	}
	switch {
	case belowFloor && metFloor:
		return fmt.Sprintf("\nMixed: the ones under %.2f are candidates to verify, not verdicts. The rest met the\nfloor and were not failed because their explainer is outside [%s].\n",
			floor, strings.Join(p.failExplainers(), ", "))
	case metFloor:
		return fmt.Sprintf("\nThese met the %.2f confidence floor. They did not fail the build because their\nexplainer is outside [%s] — add it to --fail-on to enforce them.\n",
			floor, strings.Join(p.failExplainers(), ", "))
	default:
		return fmt.Sprintf("\nConfidence < %.2f is a candidate to verify, not a verdict.\n", floor)
	}
}

// UnenforcedAtFloor returns the new findings that met the confidence floor and were
// reported rather than failed, only because the policy does not name their explainer.
//
// It exists because "nothing failed" and "nothing was found" stopped being the same
// sentence when the default policy became empty. A surface that speaks only on a
// FAILING verdict — the Stop hook — would otherwise go permanently quiet on an
// out-of-the-box install, which is the loop silently not running. These are the findings
// enola computes exactly (a declared-layer violation, an intent set difference, a cycle
// if that explainer is on): worth telling an agent about even when no policy asked for
// them. Estimates below the floor are not, or every re-ranked hotspot list becomes a
// session report.
func (v Verdict) UnenforcedAtFloor() []facts.Insight {
	floor := v.Policy.minConfidence()
	var out []facts.Insight
	for _, in := range v.Advisories {
		if in.Confidence >= floor {
			out = append(out, in)
		}
	}
	return out
}

// listCap is how many entries each list prints before summarizing the rest. Enough to
// recognize what a change touched; short enough that a clean run stays scannable.
const listCap = 12

// writeWhatChanged renders the structural delta as the thing it describes rather than as
// two numbers. "+4/0 facts, +15/0 edges" is true and nearly useless — it tells you
// something moved without telling you what, so the only way to find out was to re-run with
// --detail or go read the files, which is the work the gate exists to replace.
func (v Verdict) writeWhatChanged(sb *strings.Builder) {
	if v.FactsAdded == 0 && v.FactsRemoved == 0 && v.FactsChanged == 0 && v.EdgesAdded == 0 && v.EdgesRemoved == 0 {
		return
	}

	sb.WriteString("\nWhat changed\n")
	for _, kind := range sortedKinds(v.AddedByKind, v.RemovedByKind) {
		fmt.Fprintf(sb, "  %-12s %s\n", pluralKind(kind), signed(v.AddedByKind[kind], v.RemovedByKind[kind]))
	}
	if v.FactsChanged > 0 {
		fmt.Fprintf(sb, "  %-12s %d altered (a signature, an export, a metric)\n", "attributes", v.FactsChanged)
	}
	if v.EdgesAdded > 0 || v.EdgesRemoved > 0 {
		fmt.Fprintf(sb, "  %-12s %s%s\n", "edges", signed(v.EdgesAdded, v.EdgesRemoved),
			edgeKindBreakdown(v.EdgeKindsAdded, v.EdgeKindsRemoved))
	}

	if v.Diff == nil {
		return
	}
	writeFactLines(sb, "Added", v.Diff.FactsAdded)
	writeFactLines(sb, "Removed", v.Diff.FactsRemoved)
	writeEdgeLines(sb, "New coupling", v.Diff.EdgesAdded)
	writeEdgeLines(sb, "Removed coupling", v.Diff.EdgesRemoved)

	if v.EdgesAdded > 0 {
		sb.WriteString("\nNew coupling is reported, not failed: an added call edge is what ordinary work\nlooks like. Inspect the list above if it is more than you expected.\n")
	}
}

// writeAlteredFindings surfaces findings that survived but moved. Without it a rollup
// swinging by thousands of symbols lands nowhere, and the gate prints a confident PASS.
func (v Verdict) writeAlteredFindings(sb *strings.Builder) {
	if len(v.FindingsChanged) == 0 {
		return
	}
	// Split the ones with something to SHOW from the ones that merely re-ranked their
	// evidence. Listing both together printed a dozen near-identical lines whose only
	// visible delta was "confidence 0.77 -> 0.77" — a difference real in float but not at
	// any precision worth reading, which trains the reader to skip the section.
	var shown []diff.InsightChange
	evidenceOnly := 0
	for _, ch := range v.FindingsChanged {
		if displayDiffers(ch) {
			shown = append(shown, ch)
		} else {
			evidenceOnly++
		}
	}

	if len(shown) > 0 {
		fmt.Fprintf(sb, "\nAltered findings (%d) — same finding, moved content:\n", len(shown))
		for i, ch := range shown {
			if i == listCap {
				fmt.Fprintf(sb, "  … %d more (--detail for the full list)\n", len(shown)-listCap)
				break
			}
			source := ch.After.Source
			if source == "" {
				source = "unknown"
			}
			fmt.Fprintf(sb, "  - [%s] %s\n", source, oneLine(ch.After.Title))
			if before, after := oneLine(ch.Before.Title), oneLine(ch.After.Title); before != after {
				fmt.Fprintf(sb, "      was: %s\n", before)
			}
			if b, a := conf(ch.Before.Confidence), conf(ch.After.Confidence); b != a {
				fmt.Fprintf(sb, "      confidence %s -> %s\n", b, a)
			}
		}
	}
	if evidenceOnly > 0 {
		fmt.Fprintf(sb, "\n%s changed only in supporting evidence (same title and confidence) —\ntypically a fan-in or top-N list gaining a member. Use --detail to see them.\n",
			plural(evidenceOnly, "finding", "findings"))
	}
}

// displayDiffers reports whether a finding's change is visible at the precision this
// report prints. A confidence that moved in the third decimal is a change the diff is
// right to record and this report has nothing useful to say about.
func displayDiffers(ch diff.InsightChange) bool {
	return oneLine(ch.Before.Title) != oneLine(ch.After.Title) ||
		conf(ch.Before.Confidence) != conf(ch.After.Confidence)
}

func conf(f float64) string { return fmt.Sprintf("%.2f", f) }

// writeFactLines lists facts grouped by kind, so added modules do not hide among symbols.
func writeFactLines(sb *strings.Builder, heading string, ff []facts.Fact) {
	if len(ff) == 0 {
		return
	}
	fmt.Fprintf(sb, "\n%s (%d):\n", heading, len(ff))
	shown := 0
	for _, kind := range sortedKinds(diff.KindCounts(ff), nil) {
		for _, f := range ff {
			if f.Kind != kind {
				continue
			}
			if shown == listCap {
				fmt.Fprintf(sb, "  … %d more (--detail for the full list)\n", len(ff)-shown)
				return
			}
			fmt.Fprintf(sb, "  %-10s %-44s %s\n", f.Kind, truncate(f.Name, 44), location(f))
			shown++
		}
	}
}

// edgeRank orders relations by how much they say about coupling. `declares` is last: it is
// the mechanical symbol→module link every new symbol brings with it, so it dominates the
// count on any change that adds code while saying nothing about what got coupled to what.
var edgeRank = map[string]int{
	"imports": 0, "depends_on": 1, "calls": 2, "implements": 3,
	"instantiates": 4, "injects": 5, "has_method": 6,
	"declares": rankDeclares,
}

const (
	// rankUnknown is where a relation this list has not been taught about sorts: after
	// the known meaningful ones, but still ahead of `declares`. A relation kind we do not
	// recognize is more likely to be worth reading than the mechanical one we do.
	rankUnknown = 50
	// rankDeclares is last — see the comment on edgeRank.
	rankDeclares = 99
)

func rankOf(kind string) int {
	if r, ok := edgeRank[kind]; ok {
		return r
	}
	return rankUnknown
}

// edgeKindBreakdown renders " (calls +38, declares +16)" so the headline edge count is
// readable rather than merely large.
func edgeKindBreakdown(added, removed map[string]int) string {
	kinds := map[string]bool{}
	for k := range added {
		kinds[k] = true
	}
	for k := range removed {
		kinds[k] = true
	}
	if len(kinds) == 0 {
		return ""
	}
	ordered := make([]string, 0, len(kinds))
	for k := range kinds {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool {
		ri, rj := rankOf(ordered[i]), rankOf(ordered[j])
		if ri != rj {
			return ri < rj
		}
		return ordered[i] < ordered[j]
	})
	parts := make([]string, 0, len(ordered))
	for _, k := range ordered {
		parts = append(parts, k+" "+signed(added[k], removed[k]))
	}
	return "  (" + strings.Join(parts, ", ") + ")"
}

func writeEdgeLines(sb *strings.Builder, heading string, edges []diff.Edge) {
	if len(edges) == 0 {
		return
	}
	// Copy before sorting: the caller owns this slice (it is the diff's own field), and
	// reordering it in place would mutate the delta that --detail and --json then render.
	ordered := append([]diff.Edge(nil), edges...)
	sort.SliceStable(ordered, func(i, j int) bool {
		ri, rj := rankOf(ordered[i].Kind), rankOf(ordered[j].Kind)
		if ri != rj {
			return ri < rj
		}
		if ordered[i].Source != ordered[j].Source {
			return ordered[i].Source < ordered[j].Source
		}
		return ordered[i].Target < ordered[j].Target
	})

	fmt.Fprintf(sb, "\n%s (%d):\n", heading, len(ordered))
	for i, e := range ordered {
		if i == listCap {
			fmt.Fprintf(sb, "  … %d more (--detail for the full list)\n", len(ordered)-listCap)
			return
		}
		fmt.Fprintf(sb, "  %-44s --%s--> %s\n", truncate(edgeSource(e), 44), e.Kind, e.Target)
	}
}

// edgeSource strips the embedded target from a dependency fact's name. Those are named
// "<importer> -> <imported>", so rendering one verbatim produced
// "pkg/check -> sort --imports--> sort" — the target stated twice, once in an arrow
// notation that collides with the one this line already uses.
func edgeSource(e diff.Edge) string {
	if i := strings.Index(e.Source, " -> "); i > 0 {
		return e.Source[:i]
	}
	return e.Source
}

// location renders where a fact lives, with the line only when there is one — a module has
// no meaningful line, and printing ":0" for it looks like a bug.
func location(f facts.Fact) string {
	switch {
	case f.File == "":
		return ""
	case f.Line > 0:
		return fmt.Sprintf("%s:%d", f.File, f.Line)
	default:
		return f.File
	}
}

// sortedKinds returns the union of the two maps' keys in a stable, meaningful order:
// architectural kinds in their conceptual order first, anything unrecognized after,
// alphabetically. Map iteration order would otherwise make the output non-deterministic,
// which for a tool that sells reproducibility would be a poor look.
func sortedKinds(a, b map[string]int) []string {
	order := map[string]int{
		facts.KindService: 0, facts.KindModule: 1, facts.KindSymbol: 2,
		facts.KindRoute: 3, facts.KindStorage: 4, facts.KindDependency: 5,
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]int{a, b} {
		for k, n := range m {
			if n > 0 && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oki := order[out[i]]
		oj, okj := order[out[j]]
		if oki != okj {
			return oki
		}
		if oki && okj && oi != oj {
			return oi < oj
		}
		return out[i] < out[j]
	})
	return out
}

// signed renders an added/removed pair, omitting a zero side so "+3" is not written "+3/-0".
func signed(added, removed int) string {
	switch {
	case added > 0 && removed > 0:
		return fmt.Sprintf("+%d / -%d", added, removed)
	case removed > 0:
		return fmt.Sprintf("-%d", removed)
	default:
		return fmt.Sprintf("+%d", added)
	}
}

// kindPlural spells the architectural kinds out, because appending "s" produces
// "dependencys" and "storages".
var kindPlural = map[string]string{
	facts.KindService:    "services",
	facts.KindModule:     "modules",
	facts.KindSymbol:     "symbols",
	facts.KindRoute:      "routes",
	facts.KindStorage:    "storage",
	facts.KindDependency: "dependencies",
}

func pluralKind(kind string) string {
	if p, ok := kindPlural[kind]; ok {
		return p
	}
	if strings.HasSuffix(kind, "s") {
		return kind
	}
	return kind + "s"
}

// truncate shortens an identifier from the LEFT, keeping its tail.
//
// Fact and edge names here are path-like — "airflow-core/src/airflow/pkg/__init__.helper"
// — and everything that distinguishes one from another lives at the end: the file, the
// symbol. Cutting the tail produced output where genuinely different entries rendered
// identically, so a list of seven distinct new edges read as though it contained
// duplicates. The shared prefix is the part a reader can afford to lose.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	// Byte slicing is safe against a multi-byte boundary here because the cut point is
	// re-derived from a rune count, not from the byte index.
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return "…" + string(runes[len(runes)-(max-1):])
}

func (v Verdict) writeIntersection(sb *strings.Builder) {
	g := v.Intersection
	if g == nil {
		return
	}
	sb.WriteString("\nPartial verdict — the two snapshots were produced by different producer sets, so only\nfacts from producers present in BOTH snapshots were graded. This is NOT a full verdict.\n")
	fmt.Fprintf(sb, "  Graded over the shared producer set (%s: %s).\n",
		plural(g.Families(), "family", "families"), strings.Join(sharedFamilyNames(g), ", "))
	for _, ex := range g.Excluded {
		fmt.Fprintf(sb, "  Excluded from grading: %s (%s lacks it) — %s.\n",
			producerLabel(ex), ex.LackedBy, exclusionTally(ex))
	}
	sb.WriteString("  A regression among an excluded producer's facts cannot be graded here and is NOT reported.\n")
}

func sharedFamilyNames(g *IntersectionGrading) []string {
	names := append([]string(nil), g.SharedExtractors...)
	for _, p := range g.SharedProviders {
		names = append(names, p+" provider")
	}
	return names
}

func producerLabel(ex ExcludedProducer) string {
	if ex.Kind == ProducerProvider {
		return ex.Name + " provider"
	}
	return ex.Name
}

func exclusionTally(ex ExcludedProducer) string {
	var parts []string
	side := func(label string, factN, findingN int) {
		if factN == 0 && findingN == 0 {
			return
		}
		s := fmt.Sprintf("%s %s", plural(factN, "fact", "facts"), label)
		if findingN > 0 {
			s = fmt.Sprintf("%s and %s %s", plural(factN, "fact", "facts"), plural(findingN, "finding", "findings"), label)
		}
		parts = append(parts, s)
	}
	side("on the baseline side", ex.BaselineFactsExcluded, ex.BaselineFindingsExcluded)
	side("on the current side", ex.CurrentFactsExcluded, ex.CurrentFindingsExcluded)
	if len(parts) == 0 {
		return "no facts on either side matched it"
	}
	return strings.Join(parts, ", ") + " not graded"
}

// writeComparability prints every warning verbatim plus what its category means for
// the delta, so "stale" or "incomparable" is never asserted without its reason.
func (v Verdict) writeComparability(sb *strings.Builder) {
	if len(v.ComparabilityWarnings) == 0 {
		return
	}
	sb.WriteString("\nBaseline comparability:\n")
	for _, w := range v.ComparabilityWarnings {
		fmt.Fprintf(sb, "  - %s\n", w)
	}
	if len(v.BlockingKinds) > 0 {
		sb.WriteString("\n  Blocking (the gate declined to grade):\n")
		writeKinds(sb, v.BlockingKinds)
	}
	if len(v.AdvisoryKinds) > 0 {
		sb.WriteString("\n  Advisory (graded anyway):\n")
		writeKinds(sb, v.AdvisoryKinds)
	}
}

func writeKinds(sb *strings.Builder, kinds []diff.WarningKind) {
	for _, k := range kinds {
		if m := kindMeaning[k]; m != "" {
			fmt.Fprintf(sb, "    %s — %s\n", k, m)
		} else {
			fmt.Fprintf(sb, "    %s\n", k)
		}
	}
}

func (v Verdict) writeGuidance(sb *strings.Builder) {
	if len(v.Guidance) == 0 {
		return
	}
	fmt.Fprintf(sb, "\nGuidance for this change (%d) — advice for files this delta touched; steering, never graded:\n", len(v.Guidance))
	for _, g := range v.Guidance {
		fmt.Fprintf(sb, "  guidance %s [%s]: %s\n      because: %s\n", g.Rule, g.Mode, g.Message, g.Because)
		for _, ex := range g.Exemplars {
			fmt.Fprintf(sb, "      exemplar %s (%s)\n", ex.Exemplar, ex.Label())
		}
		for i, f := range g.MatchedFiles {
			if i == listCap {
				fmt.Fprintf(sb, "      … %d more changed files in %s\n", len(g.MatchedFiles)-listCap, g.Component)
				break
			}
			fmt.Fprintf(sb, "      changed: %s\n", f)
		}
	}
}

// writeReviewers prints the routing section: who owns what this change touched, and
// where the actor is a stranger to code somebody else knows.
//
// Text and JSON only. SARIF and the CI annotation hosts are deliberately not given
// this: both place entries on a diff line and both read as problems, and "ask Ada
// about this package" is neither a defect nor a thing that belongs pinned to a line.
func (v Verdict) writeReviewers(sb *strings.Builder) {
	r := v.Reviewers
	if r == nil {
		return
	}
	if cause := reviewerCause(r.Cause); cause != "" {
		fmt.Fprintf(sb, "\nReviewers for this change — not measured: %s\n", cause)
		return
	}
	if len(r.Routes) == 0 {
		return
	}
	fmt.Fprintf(sb, "\nReviewers for this change (%d) — authorship over the last %d commits;\nsteering, never graded:\n", len(r.Routes), r.Window)
	// A shallow clone still yields real shares, over a shorter span than was asked
	// for. Reported rather than suppressed: the numbers are usable, and a reader who
	// is not told the window was truncated will read them as the whole story.
	if r.Cause == AuthorshipShallow {
		sb.WriteString("  note: shallow clone — the window closed early, so shares cover less history than asked for\n")
	}
	if r.ActorUnknown {
		fmt.Fprintf(sb, "  note: %q has no commits in the window — reporting owners only\n", r.Actor)
	}
	for i, route := range r.Routes {
		if i == listCap {
			fmt.Fprintf(sb, "  … %d more touched module(s)\n", len(r.Routes)-listCap)
			break
		}
		if route.Owner != "" {
			fmt.Fprintf(sb, "  %s — owner: %s (%.0f%%), %d minor contributor(s) of %d\n",
				route.Module, route.Owner, route.OwnerShare*100, route.Minor, route.Total)
		} else {
			fmt.Fprintf(sb, "  %s — no single contributor above %.0f%%, %d minor contributor(s) of %d\n",
				route.Module, ownedThreshold*100, route.Minor, route.Total)
		}
		if r.Actor == "" || !route.ActorIsMinor {
			continue
		}
		fmt.Fprintf(sb, "      you are a minor contributor here (%.0f%%)\n", route.ActorShare*100)
		for j, d := range route.ViaDependents {
			if j == 3 {
				fmt.Fprintf(sb, "      … and %d more module(s) you own that import it\n", len(route.ViaDependents)-3)
				break
			}
			fmt.Fprintf(sb, "      you own %s (%.0f%%), which imports %s  [major-minor-dependency]\n",
				d.Dependent, d.Share*100, route.Module)
		}
		if route.Owner != "" && route.Owner != r.Actor {
			fmt.Fprintf(sb, "      suggested reviewer: %s\n", route.Owner)
		}
	}
}

// reviewerCause turns a declined authorship read into the sentence a reader can act
// on. A routing section that simply printed nothing would be indistinguishable from a
// repository where everybody owns everything.
func reviewerCause(cause string) string {
	switch cause {
	case AuthorshipNoGit:
		return "no readable git repository here"
	case AuthorshipEmpty:
		return "no commits in the window"
	}
	return ""
}

// writeBreaches reports the measurement thresholds this change met.
//
// Measurements the caller supplied but no threshold gates are deliberately NOT printed
// here: they are carried in the JSON for a consumer that wants them, and a text verdict
// that listed every number it chose not to act on would bury the ones it did.
func (v Verdict) writeBreaches(sb *strings.Builder) {
	if len(v.Breaches) == 0 {
		return
	}
	sb.WriteString("\nMeasurements over threshold:\n")
	for _, b := range v.Breaches {
		severity := "warn"
		if b.Fatal {
			severity = "fail"
		}
		fmt.Fprintf(sb, "  - [%s] %d %s\n", severity, b.Measurement.Count, b.Measurement.Label)
	}
}

func writeFindings(sb *strings.Builder, ins []facts.Insight) {
	for _, in := range ins {
		source := in.Source
		if source == "" {
			source = "unknown"
		}
		fmt.Fprintf(sb, "  - [%s] %.2f — %s\n", source, in.Confidence, oneLine(in.Title))
		for _, ev := range in.Evidence {
			if d := strings.TrimSpace(ev.Detail); d != "" {
				fmt.Fprintf(sb, "      %s\n", oneLine(d))
				writeFrame(sb, ev)
				break
			}
		}
	}
}

// frameRoot is where a located finding's source is read from. It is the
// process's working directory, which is the repository a check runs in; a
// frame is a courtesy for the reader in front of the code, so a file that
// cannot be read prints nothing rather than an error.
var frameRoot = "."

// writeFrame prints the line a finding is about with its span underlined.
// Nothing is printed when the extractor measured no position, when the file
// is unreadable, or when the recorded line is past the file's end: an invented
// frame would be worse than none, since a reader trusts what it points at.
func writeFrame(sb *strings.Builder, ev facts.Evidence) {
	if ev.Line <= 0 || ev.File == "" {
		return
	}
	src, err := os.ReadFile(filepath.Join(frameRoot, filepath.FromSlash(repoRelative(ev.File)))) //factpath:host
	if err != nil {
		return
	}
	lines := strings.Split(string(src), "\n")
	if ev.Line > len(lines) {
		return
	}
	text := strings.TrimRight(lines[ev.Line-1], "\r")
	fmt.Fprintf(sb, "      %s:%d\n", ev.File, ev.Line)
	fmt.Fprintf(sb, "      %s\n", text)
	if marks := underline(text, ev); marks != "" {
		fmt.Fprintf(sb, "      %s\n", marks)
	}
}

// underline marks the span's columns under the source line, tabs kept as tabs
// so the marks line up in the reader's terminal. A span that ends on a later
// line is underlined to the end of this one, which is what a reader looking at
// the first line of a multi-line definition wants.
func underline(text string, ev facts.Evidence) string {
	if ev.Column <= 0 || ev.Column > len(text)+1 {
		return ""
	}
	end := ev.EndColumn
	if ev.EndLine > ev.Line || end <= ev.Column {
		end = len(text) + 1
	}
	if end > len(text)+1 {
		end = len(text) + 1
	}
	var b strings.Builder
	for i := 0; i < ev.Column-1; i++ {
		if i < len(text) && text[i] == '\t' {
			b.WriteByte('\t')
			continue
		}
		b.WriteByte(' ')
	}
	b.WriteString(strings.Repeat("^", end-ev.Column))
	return b.String()
}

// repoRelative drops the repository label a union snapshot prefixes onto a
// fact's file, so a path resolves inside the repository the check runs in.
func repoRelative(file string) string {
	if _, rest, ok := strings.Cut(file, "/"); ok {
		if _, err := os.Stat(filepath.Join(frameRoot, filepath.FromSlash(file))); err != nil { //factpath:host
			return rest
		}
	}
	return file
}

// Detail returns the full delta report from internal/diff, for callers that want the
// changed edges and facts under the verdict rather than only the graded findings.
func (v Verdict) Detail() string {
	if v.Diff == nil {
		return ""
	}
	return v.Diff.RenderCompact()
}

// JSON is the machine-readable verdict, for a CI step that wants to post the result
// rather than parse the text.
func (v Verdict) JSON() ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// oneLine flattens prose to a single line so a multi-line description cannot break
// the one-finding-per-line shape the output relies on.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
