package plan

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/constraints"
)

func (r *Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

func (r *Report) Render() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Plan report for %s\n", r.Repo)
	renderSnapshotInfo(&sb, r.Snapshot)
	sb.WriteString("\n")

	if !r.ConstraintsDeclared {
		sb.WriteString("No constraint components are declared for this repository — no rule governs these targets.\n")
	}
	for _, t := range r.Targets {
		renderTarget(&sb, t, r.ConstraintsDeclared)
	}
	if r.Counterfactual != nil {
		renderCounterfactual(&sb, r.Counterfactual)
	}
	sb.WriteString("\nA report, never a gate: these verdicts are for the caller to weigh before editing.\n")
	return sb.String()
}

func renderSnapshotInfo(sb *strings.Builder, info SnapshotInfo) {
	switch {
	case info.Note != "":
		fmt.Fprintf(sb, "Snapshot: %s\n", info.Note)
	case info.Staleness != "":
		fmt.Fprintf(sb, "Snapshot: generated %s — STALE relative to the working tree: %s\n", info.GeneratedAt, info.Staleness)
	default:
		fmt.Fprintf(sb, "Snapshot: generated %s, matches the working tree\n", info.GeneratedAt)
	}
}

func renderTarget(sb *strings.Builder, t TargetReport, declared bool) {
	fmt.Fprintf(sb, "\nTarget %s (%s)\n", t.Target, t.Kind)
	if !t.Measured {
		if t.Kind == TargetSymbol {
			sb.WriteString("  Nothing measured carries this name — the snapshot cannot answer for it.\n")
		} else {
			sb.WriteString("  No measured fact lives at this path — governance below covers code about to be written.\n")
		}
	}
	if declared {
		if t.NoRuleGoverns {
			sb.WriteString("  No rule governs this target.\n")
		}
		for _, c := range t.Components {
			source := ""
			if c.Source != "" {
				source = " (declared in " + c.Source + ")"
			}
			fmt.Fprintf(sb, "  Component %s%s\n", c.Component, source)
			for _, rule := range c.Rules {
				unchecked := ""
				if rule.NotChecked != "" {
					unchecked = " (not checked in this snapshot: " + strings.TrimSuffix(rule.NotChecked, " in this snapshot") + ")"
				}
				fmt.Fprintf(sb, "    rule %s [%s]: %s%s\n      because: %s\n", rule.Rule, rule.Mode, rule.Statement, unchecked, rule.Because)
				for _, ex := range rule.Exemptions {
					fmt.Fprintf(sb, "      exempt: %s (by %s since %s — %s)\n", ex.Witness, ex.Owner, ex.Since, ex.Because)
				}
			}
			for _, g := range c.Guidance {
				fmt.Fprintf(sb, "    guidance %s [%s]: %s\n      because: %s\n", g.Rule, g.Mode, g.Message, g.Because)
				for _, ex := range g.Exemplars {
					fmt.Fprintf(sb, "      exemplar %s (%s)\n", ex.Exemplar, ex.Label())
				}
			}
		}
	}
	if t.BlastRadius != nil {
		fmt.Fprintf(sb, "  Blast radius: fan-in %d, fan-out %d\n", t.BlastRadius.FanIn, t.BlastRadius.FanOut)
		if len(t.BlastRadius.In) > 0 {
			fmt.Fprintf(sb, "    in:  %s\n", strings.Join(t.BlastRadius.In, ", "))
		}
		if len(t.BlastRadius.Out) > 0 {
			fmt.Fprintf(sb, "    out: %s\n", strings.Join(t.BlastRadius.Out, ", "))
		}
		if t.BlastRadius.Truncated {
			fmt.Fprintf(sb, "    (samples capped at %d; the counts are exact)\n", BlastSampleCap)
		}
	}
	if t.Radius != nil {
		renderRadius(sb, *t.Radius)
	}
}

// renderRadius prints the verdicts that would change if the target left every
// part, the no-patch counterfactual beside the scratch-copy one.
func renderRadius(sb *strings.Builder, r constraints.BlastRadius) {
	fmt.Fprintf(sb, "  If it left every part (%d rule(s) re-run over the loaded snapshot):\n", r.RulesRun)
	renderRadiusList(sb, "would start failing", r.Appear)
	renderRadiusList(sb, "would stop being checked", r.Vanish)
	for _, nc := range r.NotComputed {
		fmt.Fprintf(sb, "    not computed: rule %s (%s)\n", nc.Rule, nc.Cause)
	}
}

func renderRadiusList(sb *strings.Builder, heading string, verdicts []constraints.RadiusVerdict) {
	if len(verdicts) == 0 {
		fmt.Fprintf(sb, "    %s: nothing\n", heading)
		return
	}
	fmt.Fprintf(sb, "    %s:\n", heading)
	for _, v := range verdicts {
		fmt.Fprintf(sb, "      %s\n", v.Title)
		if v.Because != "" {
			fmt.Fprintf(sb, "        because: %s\n", v.Because)
		}
		if v.Cut != "" {
			fmt.Fprintf(sb, "        cut: %s\n", v.Cut)
		}
	}
}

func renderCounterfactual(sb *strings.Builder, cf *Counterfactual) {
	fmt.Fprintf(sb, "\nCounterfactual over a scratch copy — the working tree was not touched.\n")
	fmt.Fprintf(sb, "Patched files: %s\n", strings.Join(cf.PatchFiles, ", "))
	if !cf.ConstraintsDeclared {
		sb.WriteString("No constraints are declared, so the patch can breach nothing declared.\n")
	}
	renderBucket(sb, "NEW — this patch WOULD introduce", cf.New)
	renderBucket(sb, "RESOLVED — this patch would clear", cf.Resolved)
	if len(cf.Unchanged) == 0 {
		sb.WriteString("\nUnchanged constraint findings: none.\n")
	} else {
		fmt.Fprintf(sb, "\nUnchanged constraint findings: %d (present before and after).\n", len(cf.Unchanged))
	}
}

func renderBucket(sb *strings.Builder, heading string, verdicts []Verdict) {
	if len(verdicts) == 0 {
		fmt.Fprintf(sb, "\n%s: nothing.\n", heading)
		return
	}
	fmt.Fprintf(sb, "\n%s:\n", heading)
	for _, v := range verdicts {
		fmt.Fprintf(sb, "  %s\n", v.Title)
		if v.Because != "" {
			fmt.Fprintf(sb, "    because: %s\n", v.Because)
		}
		for _, ev := range v.Evidence {
			parts := make([]string, 0, 4)
			for _, p := range []string{ev.File, ev.Symbol, ev.Fact, ev.Detail} {
				if p != "" {
					parts = append(parts, p)
				}
			}
			fmt.Fprintf(sb, "    witness: %s\n", strings.Join(parts, " · "))
		}
	}
}
