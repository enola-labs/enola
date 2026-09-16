package explain

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/metrics"
	"github.com/enola-labs/enola/internal/orphans"
	"github.com/enola-labs/enola/internal/perf"
)

func filledReport() *Report {
	return &Report{
		// Enough of the surrounding report for the neighbouring headings to render,
		// since placement relative to them is what the ordering test checks.
		Hotspots:   []Hotspot{{Module: "internal/facts", FanIn: 251, FanOut: 2, Criticality: "high", BlastRadius: 95}},
		CodeHealth: []FindingGroup{{Label: "god classes (high fan-in)", Count: 25}},
		PackageMetrics: &metrics.Summary{
			Analyzed: 117, Typeless: 32, AvgI: 0.5, AvgD: 0.49,
			OffMain: 9, PainfulDistance: 0.7, MinPainfulTypes: 3,
			MostCoupledPackage: "internal/facts", MostCoupledCa: 83,
		},
		DeadCode: &orphans.Summary{
			Total: 232, Candidates: 6071, High: 58, Medium: 81, Low: 93,
			Isolated: 104, Unreferenced: 128, Exported: 143,
		},
		Performance: &perf.Summary{
			FunctionsAnalyzed: 5198, Total: 1261, High: 249, Medium: 1007, Low: 5,
			ByKind:       []perf.Bucket{{Label: "nested-loop", Count: 362}},
			ByComplexity: []perf.Bucket{{Label: "O(n²)", Count: 696}, {Label: "recursive", Count: 97}},
			Top: []perf.Finding{{
				Symbol: "app.Handler", File: "app/h.go", Line: 5,
				Severity: "high", BigO: "O(n³)", Kind: "nested-loop",
			}},
		},
	}
}

// Placement is the point of the section, not decoration. Package metrics is Ca and
// Ce over the same edges the hotspot table reports as fan-in and fan-out, so it
// renders next to that table; dead code and performance are symbol-level, so they
// render with Code health. A reader who meets them apart treats them as unrelated
// findings, which is the thing this ordering exists to prevent.
func TestSectionsAreOrderedByAltitude(t *testing.T) {
	out := filledReport().Render()

	order := []string{
		"Impact analysis (hotspots)",
		"Package metrics",
		"Code health",
		"Dead code",
		"Performance",
	}
	at := -1
	for _, heading := range order {
		i := strings.Index(out, "\n"+heading+"\n")
		if i < 0 {
			t.Fatalf("report has no %q section:\n%s", heading, out)
		}
		if i < at {
			t.Errorf("%q renders before the section that should precede it", heading)
		}
		at = i
	}
}

func TestSectionsRenderTheirNumbers(t *testing.T) {
	out := filledReport().Render()
	for _, want := range []string{
		"packages analyzed           117 (32 type-less excluded)",
		"most depended-upon       internal/facts (Ca=83)",
		"potential dead code       232  (of 6071 symbols)",
		"performance findings       1261   (high 249 / medium 1007 / low 5)",
		"by kind                  nested-loop 362",
		"by complexity            O(n²) 696 · recursive 97",
		"[high] O(n³) app.Handler (app/h.go:5) — nested-loop",
		"1260 more — analyze_performance filters by package and severity.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n%s", want, out)
		}
	}
}

// A repository with nothing to say about the main sequence says nothing, rather
// than printing a block of zeroes that reads as a measurement.
func TestAbsentSummariesRenderNothing(t *testing.T) {
	out := (&Report{}).Render()
	for _, heading := range []string{"Package metrics", "Dead code", "Performance"} {
		if strings.Contains(out, "\n"+heading+"\n") {
			t.Errorf("empty report still rendered the %q section:\n%s", heading, out)
		}
	}
}

// The thresholds travel with the summary rather than being written again here, so
// a change to painfulDistance moves the sentence too.
func TestOffMainSequenceLineStatesItsThresholds(t *testing.T) {
	out := filledReport().Render()
	if !strings.Contains(out, "(D > 0.7, N >= 3, coupled") {
		t.Errorf("off-main-sequence line does not state its thresholds:\n%s", out)
	}
}
