package diff

import (
	"fmt"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// CompareMeta reports whether two snapshots were generated over equivalent
// inputs (see compareMeta), for callers that hold only the two receipts.
//
// It is not everything diff_snapshot checks. A union's members are labels on its
// facts, not fields of its meta, so a union and its last member alone compare
// clean here; a caller holding both snapshots uses CompareSnapshotReceipts.
func CompareMeta(base, cur facts.SnapshotMeta) Comparability {
	return compareMeta(base, cur)
}

// MetricDelta is a single receipt metric's before/after with its change.
type MetricDelta struct {
	Name   string `json:"name"`
	Before int    `json:"before"`
	After  int    `json:"after"`
	Delta  int    `json:"delta"`
}

// ReceiptComparison is the result of comparing two snapshot receipts: whether
// they are comparable at all, the metric deltas between them, and any
// extraction-quality regressions (the loop signal — thinner extraction between
// baseline and current). It is the "compare receipt A vs B before trusting the
// diff" answer, and doubles as an agent's polling entry point for self-improvement.
type ReceiptComparison struct {
	Comparability      Comparability `json:"comparability"`
	BaselineID         string        `json:"baseline_id,omitempty"`
	CurrentID          string        `json:"current_id,omitempty"`
	Identical          bool          `json:"identical"` // same snapshot_id: byte-identical graph over identical inputs
	Deltas             []MetricDelta `json:"deltas,omitempty"`
	QualityRegressions []string      `json:"quality_regressions,omitempty"`
}

// CompareReceipts compares a baseline snapshot's receipt against the current one.
// It computes comparability, per-metric deltas, and quality regressions. Its
// comparability is meta-only (see CompareMeta).
func CompareReceipts(base, cur facts.SnapshotMeta) *ReceiptComparison {
	rc := &ReceiptComparison{
		Comparability: compareMeta(base, cur),
		BaselineID:    base.SnapshotID,
		CurrentID:     cur.SnapshotID,
		Identical:     base.SnapshotID != "" && base.SnapshotID == cur.SnapshotID,
	}

	baseUnresolved, curUnresolved := unresolvedEdges(base), unresolvedEdges(cur)
	baseGaps, curGaps := coverageGaps(base), coverageGaps(cur)

	rc.Deltas = []MetricDelta{
		delta("files_seen", base.FilesSeen, cur.FilesSeen),
		delta("files_parsed", base.FilesParsed, cur.FilesParsed),
		delta("files_skipped", base.FilesSkipped, cur.FilesSkipped),
		delta("dirs_skipped", base.DirsSkipped, cur.DirsSkipped),
		delta("parse_errors", base.ParseErrors, cur.ParseErrors),
		delta("coverage_gaps", baseGaps, curGaps),
		delta("unresolved_edges", baseUnresolved, curUnresolved),
		delta("fact_count", base.FactCount, cur.FactCount),
		delta("insight_count", base.InsightCount, cur.InsightCount),
	}

	// Quality-regression rules (conservative — a candidate to investigate, not a
	// verdict). These are the signal that enola's OWN extraction got thinner.
	if cur.ParseErrors > base.ParseErrors {
		rc.QualityRegressions = append(rc.QualityRegressions, fmt.Sprintf(
			"parse errors rose (%d → %d) — an extractor is failing on more inputs", base.ParseErrors, cur.ParseErrors))
	}
	if curUnresolved > baseUnresolved {
		rc.QualityRegressions = append(rc.QualityRegressions, fmt.Sprintf(
			"unresolved cross-repo edges rose (%d → %d) — linking coverage dropped", baseUnresolved, curUnresolved))
	}
	// A drop in the parsed/seen ratio means enola is parsing a smaller share of the
	// files it enumerated — a coverage regression even if the repo grew.
	if base.FilesSeen > 0 && cur.FilesSeen > 0 {
		if ratio(cur.FilesParsed, cur.FilesSeen) < ratio(base.FilesParsed, base.FilesSeen)-0.02 {
			rc.QualityRegressions = append(rc.QualityRegressions, fmt.Sprintf(
				"parsed/seen ratio dropped (%.0f%% → %.0f%%) — a larger share of files produced no facts",
				ratio(base.FilesParsed, base.FilesSeen)*100, ratio(cur.FilesParsed, cur.FilesSeen)*100))
		}
	}

	return rc
}

// CompareSnapshotReceipts is CompareReceipts for a caller that holds both
// snapshots, and the one the compare_receipts tool uses: its comparability is
// the diff's own, so the pre-check cannot clear a pair diff_snapshot refuses.
func CompareSnapshotReceipts(base, cur *facts.Snapshot) *ReceiptComparison {
	rc := CompareReceipts(base.Meta, cur.Meta)
	rc.Comparability = compareSnapshots(base, cur)
	return rc
}

func delta(name string, before, after int) MetricDelta {
	return MetricDelta{Name: name, Before: before, After: after, Delta: after - before}
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func unresolvedEdges(m facts.SnapshotMeta) int {
	if m.Coverage == nil {
		return 0
	}
	return m.Coverage.UnresolvedEdges
}

func coverageGaps(m facts.SnapshotMeta) int {
	if m.Coverage == nil {
		return 0
	}
	return m.Coverage.CoverageGaps
}

// Render produces a compact markdown summary of the receipt comparison.
func (rc *ReceiptComparison) Render() string {
	var sb strings.Builder
	sb.WriteString("# Receipt comparison\n\n")

	if rc.Identical {
		sb.WriteString("**Identical snapshots** — same `snapshot_id`: the graph was byte-identical over identical inputs. Nothing to compare.\n\n")
		return sb.String()
	}

	if len(rc.Comparability.Warnings) > 0 {
		sb.WriteString("> ⚠️ **Not safely comparable** — differences below may reflect the mismatch, not real change:\n")
		for _, w := range rc.Comparability.Warnings {
			fmt.Fprintf(&sb, ">  - %s\n", w)
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("_Baseline and current were generated over equivalent inputs — the deltas below are real._\n\n")
	}

	sb.WriteString("## Metric deltas\n\n")
	sb.WriteString("| Metric | Baseline | Current | Δ |\n|---|---:|---:|---:|\n")
	for _, d := range rc.Deltas {
		fmt.Fprintf(&sb, "| %s | %d | %d | %+d |\n", d.Name, d.Before, d.After, d.Delta)
	}
	sb.WriteString("\n")

	if len(rc.QualityRegressions) > 0 {
		sb.WriteString("## ⚠️ Extraction-quality regressions\n\n")
		for _, q := range rc.QualityRegressions {
			fmt.Fprintf(&sb, "- %s\n", q)
		}
		sb.WriteString("\n_These indicate enola's own extraction got thinner — candidates to investigate (a missing detection, a bad ignore glob, a broken extractor), not verdicts._\n")
	} else {
		sb.WriteString("_No extraction-quality regressions detected._\n")
	}

	return sb.String()
}
