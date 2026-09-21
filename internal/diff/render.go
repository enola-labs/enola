package diff

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// RenderSummary is the default, smallest view: the headline regressions and
// improvements plus a structural-change tally. It deliberately omits any listing
// of pre-existing state — only deltas appear.
func (d *SnapshotDiff) RenderSummary() string {
	var sb strings.Builder
	sb.WriteString("# Architecture diff\n\n")
	d.writeProvenance(&sb)

	if d.Empty() {
		sb.WriteString("**No architectural changes detected.** The change did not add, remove, or alter any facts, edges, or findings.\n")
		return sb.String()
	}

	// Regressions first — this is the signal an agent most needs.
	sb.WriteString(fmt.Sprintf("## Regressions introduced (%d)\n\n", len(d.FindingsNew)))
	if len(d.FindingsNew) == 0 {
		sb.WriteString("None — the change introduced no new findings.\n\n")
	} else {
		for _, in := range d.FindingsNew {
			fmt.Fprintf(&sb, "- [%s] %.2f — %s\n", insightSource(in), in.Confidence, oneLine(in.Title))
		}
		sb.WriteString("\n_Confidence < 1.0 is a candidate to verify, not a verdict. Use output_mode='compact' for descriptions, evidence, and caveats._\n\n")
	}

	if len(d.FindingsResolved) > 0 {
		sb.WriteString(fmt.Sprintf("## Improvements — findings resolved (%d)\n\n", len(d.FindingsResolved)))
		for _, in := range d.FindingsResolved {
			fmt.Fprintf(&sb, "- [%s] %s\n", insightSource(in), oneLine(in.Title))
		}
		sb.WriteString("\n")
	}

	d.writeAlteredFindings(&sb)
	d.writeIncidentalShifts(&sb)
	d.writeStructuralSummary(&sb)
	return sb.String()
}

// RenderCompact adds, on top of the summary, per-finding descriptions with an
// evidence sample, and capped lists of added/removed edges and facts so the
// caller can see exactly what changed without fetching the full JSON.
func (d *SnapshotDiff) RenderCompact() string {
	const sample = 25

	var sb strings.Builder
	sb.WriteString("# Architecture diff\n\n")
	d.writeProvenance(&sb)

	if d.Empty() {
		sb.WriteString("**No architectural changes detected.**\n")
		return sb.String()
	}

	sb.WriteString(fmt.Sprintf("## Regressions introduced (%d)\n\n", len(d.FindingsNew)))
	if len(d.FindingsNew) == 0 {
		sb.WriteString("None — the change introduced no new findings.\n\n")
	} else {
		for i, in := range d.FindingsNew {
			fmt.Fprintf(&sb, "### %d. %s\n", i+1, in.Title)
			fmt.Fprintf(&sb, "- explainer: %s · confidence: %.2f\n", insightSource(in), in.Confidence)
			if in.Description != "" {
				fmt.Fprintf(&sb, "- %s\n", in.Description)
			}
			writeEvidenceSample(&sb, in.Evidence, 8)
			if len(in.Actions) > 0 {
				sb.WriteString("- suggested actions:\n")
				for _, a := range in.Actions {
					fmt.Fprintf(&sb, "    - %s\n", a)
				}
			}
			sb.WriteString("\n")
		}
	}

	if len(d.FindingsResolved) > 0 {
		sb.WriteString(fmt.Sprintf("## Improvements — findings resolved (%d)\n\n", len(d.FindingsResolved)))
		for _, in := range d.FindingsResolved {
			fmt.Fprintf(&sb, "- [%s] %s\n", insightSource(in), oneLine(in.Title))
		}
		sb.WriteString("\n")
	}

	d.writeAlteredFindings(&sb)
	d.writeIncidentalShifts(&sb)
	d.writeStructuralSummary(&sb)

	// New coupling is the architecturally interesting structural change, so list
	// edges before the bulk fact lists.
	writeEdgeList(&sb, "New coupling — edges added", d.EdgesAdded, sample)
	writeEdgeList(&sb, "Coupling removed — edges removed", d.EdgesRemoved, sample)
	writeFactList(&sb, "Facts added", d.FactsAdded, sample)
	writeFactList(&sb, "Facts removed", d.FactsRemoved, sample)

	if len(d.FactsChanged) > 0 {
		fmt.Fprintf(&sb, "## Facts changed (%d)\n\n", len(d.FactsChanged))
		shown := d.FactsChanged
		if len(shown) > sample {
			shown = shown[:sample]
		}
		for _, c := range shown {
			fmt.Fprintf(&sb, "- %s %s (%s:%d)\n", c.After.Kind, c.After.Name, c.After.File, c.After.Line)
			// Name WHAT moved. The before/after facts have always been carried here and
			// never surfaced, so a changed fact rendered as a bare line and the reader
			// had to open the JSON to learn whether a signature, a complexity metric or
			// a coupling number had shifted. A count of changed facts is not a finding;
			// the delta inside them is.
			for _, d := range changedProps(c) {
				fmt.Fprintf(&sb, "    %s\n", d)
			}
		}
		if len(d.FactsChanged) > len(shown) {
			fmt.Fprintf(&sb, "- … and %d more (output_mode='full' for all)\n", len(d.FactsChanged)-len(shown))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// changedProps describes what actually differs between the two versions of a fact, as
// "key: before → after" lines.
//
// Props only. Kind and Name are the identity a FactChange is matched on and cannot
// differ; File and Line moving is noise on its own (a fact shifting down a file is not
// an architectural change) and is left to the location already printed above.
//
// Values are rendered with %v rather than compared numerically: props are map[string]any
// holding whatever an extractor put there, and a differ that assumed a type would go
// quiet the first time one changed.
func changedProps(c FactChange) []string {
	if c.Before.PropCount() == 0 && c.After.PropCount() == 0 {
		return nil
	}
	keys := make(map[string]bool, c.Before.PropCount()+c.After.PropCount())
	for k := range c.Before.Props {
		keys[k] = true
	}
	for k := range c.After.Props {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	// Sorted: the map iteration order varies between runs, and a renderer whose output
	// reshuffles makes two identical diffs look different.
	sort.Strings(names)

	var out []string
	for _, k := range names {
		before, hadBefore := c.Before.Prop(k)
		after, hadAfter := c.After.Prop(k)
		switch {
		case hadBefore && hadAfter:
			if fmt.Sprintf("%v", before) != fmt.Sprintf("%v", after) {
				out = append(out, fmt.Sprintf("%s: %v → %v", k, before, after))
			}
		case hadAfter:
			out = append(out, fmt.Sprintf("%s: (unset) → %v", k, after))
		case hadBefore:
			out = append(out, fmt.Sprintf("%s: %v → (unset)", k, before))
		}
	}
	return out
}

// writeIncidentalShifts lists findings that appeared or cleared without a
// structural cause in this change (a moving statistical threshold, or a top-N list
// re-ranking after some other finding left the window). They are surfaced so
// nothing is hidden, but kept out of the regression/improvement headline so they
// don't read as something the change caused.
func (d *SnapshotDiff) writeIncidentalShifts(sb *strings.Builder) {
	total := len(d.FindingsNewIncidental) + len(d.FindingsResolvedIncidental)
	if total == 0 {
		return
	}
	fmt.Fprintf(sb, "## Incidental finding shifts (%d)\n\n", total)
	heuristic, decided := false, false
	for _, in := range append(append([]facts.Insight{}, d.FindingsNewIncidental...), d.FindingsResolvedIncidental...) {
		if decidedRuleFinding(in) {
			decided = true
		} else {
			heuristic = true
		}
	}
	if heuristic {
		sb.WriteString("_Heuristic findings appeared or cleared with no structural cause in this change — a moving statistical threshold or a re-ranked top-N list. Likely NOT caused by this change; verify only if relevant._\n\n")
	}
	if decided {
		sb.WriteString("_Constraint verdicts at confidence 1.0 are decided rules, not statistical thresholds: a breach that appeared or cleared here changed state without a structural cause attributable to this change — check the rule's membership or its declaration rather than dismissing it as drift._\n\n")
	}
	for _, in := range d.FindingsNewIncidental {
		fmt.Fprintf(sb, "- appeared · [%s] %s\n", insightSource(in), oneLine(in.Title))
	}
	for _, in := range d.FindingsResolvedIncidental {
		fmt.Fprintf(sb, "- cleared · [%s] %s\n", insightSource(in), oneLine(in.Title))
	}
	sb.WriteString("\n")
}

func (d *SnapshotDiff) writeProvenance(sb *strings.Builder) {
	if d.BaselineGeneratedAt != "" || d.CurrentGeneratedAt != "" {
		fmt.Fprintf(sb, "_Baseline %s → current %s._\n\n", orDash(d.BaselineGeneratedAt), orDash(d.CurrentGeneratedAt))
	}
	// Comparability warnings appear ABOVE the delta: a mismatched baseline (different
	// repo, extractor set, enola version, or ignore globs) makes the numbers below
	// misleading, so the reader must see the caveat first.
	if len(d.Comparability.Warnings) > 0 {
		sb.WriteString("> ⚠️ **Comparability warnings** — read before trusting the delta:\n")
		for _, w := range d.Comparability.Warnings {
			fmt.Fprintf(sb, ">  - %s\n", w)
		}
		sb.WriteString("\n")
	}
}

// writeStructuralSummary renders the added/removed tally per fact kind plus edges.
func (d *SnapshotDiff) writeStructuralSummary(sb *strings.Builder) {
	added := KindCounts(d.FactsAdded)
	removed := KindCounts(d.FactsRemoved)

	sb.WriteString("## Structural changes\n\n")

	kinds := map[string]struct{}{}
	for k := range added {
		kinds[k] = struct{}{}
	}
	for k := range removed {
		kinds[k] = struct{}{}
	}
	if len(kinds) > 0 {
		ordered := make([]string, 0, len(kinds))
		for k := range kinds {
			ordered = append(ordered, k)
		}
		sort.Strings(ordered)
		for _, k := range ordered {
			fmt.Fprintf(sb, "- %s: +%d / -%d\n", k, added[k], removed[k])
		}
	}
	fmt.Fprintf(sb, "- edges: +%d / -%d\n", len(d.EdgesAdded), len(d.EdgesRemoved))
	if len(d.FactsChanged) > 0 {
		fmt.Fprintf(sb, "- facts changed (props): %d\n", len(d.FactsChanged))
	}
	sb.WriteString("\n")
}

func writeEdgeList(sb *strings.Builder, heading string, edges []Edge, limit int) {
	if len(edges) == 0 {
		return
	}
	fmt.Fprintf(sb, "## %s (%d)\n\n", heading, len(edges))
	shown := edges
	if len(shown) > limit {
		shown = shown[:limit]
	}
	for _, e := range shown {
		fmt.Fprintf(sb, "- %s --%s--> %s\n", e.Source, e.Kind, e.Target)
	}
	if len(edges) > len(shown) {
		fmt.Fprintf(sb, "- … and %d more (output_mode='full' for all)\n", len(edges)-len(shown))
	}
	sb.WriteString("\n")
}

func writeFactList(sb *strings.Builder, heading string, ff []facts.Fact, limit int) {
	if len(ff) == 0 {
		return
	}
	fmt.Fprintf(sb, "## %s (%d)\n\n", heading, len(ff))
	shown := ff
	if len(shown) > limit {
		shown = shown[:limit]
	}
	for _, f := range shown {
		fmt.Fprintf(sb, "- %s %s (%s:%d)\n", f.Kind, f.Name, f.File, f.Line)
	}
	if len(ff) > len(shown) {
		fmt.Fprintf(sb, "- … and %d more (output_mode='full' for all)\n", len(ff)-len(shown))
	}
	sb.WriteString("\n")
}

func writeEvidenceSample(sb *strings.Builder, evidence []facts.Evidence, limit int) {
	if len(evidence) == 0 {
		return
	}
	fmt.Fprintf(sb, "- evidence (%d):\n", len(evidence))
	shown := len(evidence)
	if shown > limit {
		shown = limit
	}
	for _, ev := range evidence[:shown] {
		fmt.Fprintf(sb, "    - %s\n", formatEvidence(ev))
	}
	if len(evidence) > shown {
		fmt.Fprintf(sb, "    - … and %d more (output_mode='full' for all)\n", len(evidence)-shown)
	}
}

// --- small formatting helpers (kept local to avoid coupling to the server package) ---

func decidedRuleFinding(in facts.Insight) bool {
	return in.Source == "constraints" && in.Confidence == 1.0
}

func insightSource(in facts.Insight) string {
	if in.Source == "" {
		return "—"
	}
	return in.Source
}

func formatEvidence(ev facts.Evidence) string {
	var parts []string
	for _, p := range []string{ev.Fact, ev.Symbol, ev.File} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	s := strings.Join(parts, " ")
	if ev.Detail != "" {
		if s != "" {
			s += " — " + ev.Detail
		} else {
			s = ev.Detail
		}
	}
	return s
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", "\\|")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// writeAlteredFindings reports findings that survived under the same identity but whose
// content moved. Capped, because on a real code change many findings carry a drifting
// count in their title (175 of 359 on a large Android app) and an uncapped list
// would bury the regressions above it. The count is always stated: it is the number
// the headline used to claim was zero.
func (d *SnapshotDiff) writeAlteredFindings(sb *strings.Builder) {
	if len(d.FindingsChanged) == 0 {
		return
	}
	const sample = 10
	fmt.Fprintf(sb, "## Findings altered (%d)\n\n", len(d.FindingsChanged))
	sb.WriteString("_Same finding, changed content — a metric in the title, the confidence, or the evidence moved. " +
		"Not a new or resolved finding; the subject is the same one._\n\n")
	for i, c := range d.FindingsChanged {
		if i >= sample {
			fmt.Fprintf(sb, "- … and %d more (output_mode='full' for all)\n", len(d.FindingsChanged)-sample)
			break
		}
		fmt.Fprintf(sb, "- [%s] %s\n    → %s\n", insightSource(c.After), oneLine(c.Before.Title), oneLine(c.After.Title))
	}
	sb.WriteString("\n")
}
