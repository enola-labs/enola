package dashboard

import (
	"encoding/json"
	"fmt"
	"html/template"
	"sort"

	"github.com/enola-labs/enola/internal/metrics"
)

// Scatter geometry. The viewBox is fixed; the SVG scales responsively in CSS.
// A square plot area holds the (Instability, Abstractness) unit square, with a
// left/bottom margin for the axis tick labels. Mirrors edgediagram.go's
// "pure function → view struct → inline SVG" approach so the layout is
// deterministic and unit-testable.
const (
	scatterPlot      = 360.0 // side of the square plot area (the I×A unit square)
	scatterMarginL   = 40.0  // room for the abstractness (Y) tick labels
	scatterMarginB   = 32.0  // room for the instability (X) tick labels
	scatterMarginT   = 12.0
	scatterMarginR   = 12.0
	scatterPointR    = 5.0
	scatterWidth     = scatterMarginL + scatterPlot + scatterMarginR
	scatterHeight    = scatterMarginT + scatterPlot + scatterMarginB
	scatterTickLabel = 14.0 // gap from the plot edge to a tick label
)

// metricRow is one per-package row of the metrics table, pre-formatted for display.
// Repo is empty in single-repo mode; in multi-repo mode it groups the row under the
// repo selector and Hidden marks rows for repos other than the default selection.
type metricRow struct {
	Repo    string
	Package string
	N       int
	Ca, Ce  int
	I, A, D string // %.2f
	Zone    metrics.Zone
	Status  string // human label matching the zone
	Hidden  bool   // initial visibility: true for repos other than the default (multi-repo)
}

// metricAgg holds the aggregate cards + guide counts for one population. Averages
// are over typed packages (N ≥ 1) — the same set package_metrics reports I/D over —
// so the numbers agree with the tool.
type metricAgg struct {
	Analyzed     int
	AvgCa, AvgCe string // %.1f
	AvgI, AvgA   string // %.2f
	AvgD         string // %.2f
	HealthyCount int
	PainCount    int
	UselessCount int
}

// aggJS is the client-side per-repo aggregate, injected as a JSON map so the repo
// selector can swap the cards + legend without a page reload.
type aggJS struct {
	Analyzed int    `json:"analyzed"`
	Ca       string `json:"ca"`
	Ce       string `json:"ce"`
	I        string `json:"i"`
	A        string `json:"a"`
	D        string `json:"d"`
	Healthy  int    `json:"healthy"`
	Pain     int    `json:"pain"`
	Useless  int    `json:"useless"`
}

// axisTick is one gridline + its tick label, pre-positioned so the template stays
// free of coordinate math.
type axisTick struct {
	LineX1, LineY1 float64
	LineX2, LineY2 float64
	LabelX, LabelY float64
	Label          string
}

// metricPoint is one package plotted at (I, A) — Y inverted so A=1 sits at the top.
type metricPoint struct {
	CX, CY float64
	Zone   metrics.Zone
	Repo   string
	Label  string // hover tooltip, e.g. "core.domain — I=0.12 A=0.81 D=0.31"
	Hidden bool   // initial visibility (repos other than the default, multi-repo)
}

// metricScatter is the full main-sequence layout the template renders as inline SVG.
type metricScatter struct {
	Width, Height              float64
	PlotX, PlotY, PlotW, PlotH float64
	// Main-sequence line A+I=1: from (I=0,A=1) top-left to (I=1,A=0) bottom-right.
	LineX1, LineY1, LineX2, LineY2 float64
	// Axis-title anchors. The abstractness title is rotated -90° about (YAxisX,YAxisY).
	XAxisX, XAxisY float64
	YAxisX, YAxisY float64
	XTicks         []axisTick // instability axis (bottom)
	YTicks         []axisTick // abstractness axis (left)
	Points         []metricPoint
}

// metricsView is the template model for the Package Metrics modal. In multi-repo
// mode (a multi-repo append snapshot) Multi is true, Repos lists the selectable
// labels, Default is the initially-shown repo, Agg reflects Default, and AggMapJS
// carries every repo's aggregate for the client-side selector. In single-repo mode
// Multi is false, Repos/AggMapJS are empty, and Agg covers the whole set.
type metricsView struct {
	Multi         bool
	Repos         []string
	Default       string
	TotalAnalyzed int // typed packages across all repos, for the trigger card
	Agg           metricAgg
	AggMapJS      template.JS
	Rows          []metricRow
	Scatter       metricScatter
}

// packageMetricsView projects the computed per-package metrics into the dashboard
// model: aggregate cards, a distance-ranked table, and the main-sequence scatter,
// with a per-repo selector when the store holds more than one repo. Only typed
// packages (N ≥ 1) are shown — abstractness/distance are undefined for type-less
// packages, and package_metrics likewise excludes them. Zone classification comes
// from metrics.Classify — the same source the explainer and package_metrics use —
// so the dashboard can never disagree with the tool. Returns nil when there are no
// typed packages, so the card and modal auto-hide. Pure and deterministic.
func packageMetricsView(pms []metrics.PackageMetric) *metricsView {
	typed := make([]metrics.PackageMetric, 0, len(pms))
	for _, m := range pms {
		if m.ClassesInterfaces > 0 {
			typed = append(typed, m)
		}
	}
	if len(typed) == 0 {
		return nil
	}

	// Distinct repo labels (non-empty). Multi-repo mode kicks in at 2+.
	repoSet := map[string]struct{}{}
	for _, m := range typed {
		if m.Repo != "" {
			repoSet[m.Repo] = struct{}{}
		}
	}
	repos := make([]string, 0, len(repoSet))
	for r := range repoSet {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	v := &metricsView{TotalAnalyzed: len(typed)}
	v.Multi = len(repos) > 1
	if v.Multi {
		v.Repos = repos
		v.Default = repos[0]
	}

	// Aggregates: per-repo (multi) for the client-side swap, plus the headline Agg
	// (the default repo in multi mode, or the whole set in single mode).
	if v.Multi {
		aggMap := make(map[string]aggJS, len(repos))
		for _, r := range repos {
			agg, js := computeAgg(filterByRepo(typed, r))
			aggMap[r] = js
			if r == v.Default {
				v.Agg = agg
			}
		}
		if b, err := json.Marshal(aggMap); err == nil {
			v.AggMapJS = template.JS(b) //nolint:gosec // keys are repo labels, values are our formatted numbers
		}
	} else {
		v.Agg, _ = computeAgg(typed)
	}

	// Rows: grouped by repo, worst (farthest from the main sequence) first within a
	// repo, package name as the final tiebreak. Non-default repos start hidden.
	rows := make([]metricRow, 0, len(typed))
	for _, m := range typed {
		z := metrics.Classify(m)
		rows = append(rows, metricRow{
			Repo:    m.Repo,
			Package: m.Package,
			N:       m.ClassesInterfaces,
			Ca:      m.Ca,
			Ce:      m.Ce,
			I:       fmt.Sprintf("%.2f", m.Instability),
			A:       fmt.Sprintf("%.2f", m.Abstractness),
			D:       fmt.Sprintf("%.2f", m.Distance),
			Zone:    z,
			Status:  zoneLabel(z),
			Hidden:  v.Multi && m.Repo != v.Default,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Repo != rows[j].Repo {
			return rows[i].Repo < rows[j].Repo
		}
		if rows[i].D != rows[j].D {
			return rows[i].D > rows[j].D // fixed %.2f width in [0,1] → lexical == numeric
		}
		return rows[i].Package < rows[j].Package
	})
	v.Rows = rows

	v.Scatter = buildScatter(typed, v.Multi, v.Default)

	return v
}

// filterByRepo returns the metrics belonging to one repo label.
func filterByRepo(pms []metrics.PackageMetric, repo string) []metrics.PackageMetric {
	out := make([]metrics.PackageMetric, 0, len(pms))
	for _, m := range pms {
		if m.Repo == repo {
			out = append(out, m)
		}
	}
	return out
}

// computeAgg averages Ca/Ce/I/A/D and tallies zones over a typed population, in both
// the display shape (metricAgg) and the client-swap shape (aggJS).
func computeAgg(typed []metrics.PackageMetric) (metricAgg, aggJS) {
	var sumCa, sumCe, sumI, sumA, sumD float64
	var healthy, pain, useless int
	for _, m := range typed {
		switch metrics.Classify(m) {
		case metrics.ZoneMainSequence:
			healthy++
		case metrics.ZonePain:
			pain++
		case metrics.ZoneUseless:
			useless++
		}
		sumCa += float64(m.Ca)
		sumCe += float64(m.Ce)
		sumI += m.Instability
		sumA += m.Abstractness
		sumD += m.Distance
	}
	n := float64(len(typed))
	ca := fmt.Sprintf("%.1f", sumCa/n)
	ce := fmt.Sprintf("%.1f", sumCe/n)
	i := fmt.Sprintf("%.2f", sumI/n)
	a := fmt.Sprintf("%.2f", sumA/n)
	d := fmt.Sprintf("%.2f", sumD/n)
	agg := metricAgg{
		Analyzed: len(typed),
		AvgCa:    ca, AvgCe: ce, AvgI: i, AvgA: a, AvgD: d,
		HealthyCount: healthy, PainCount: pain, UselessCount: useless,
	}
	js := aggJS{
		Analyzed: len(typed),
		Ca:       ca, Ce: ce, I: i, A: a, D: d,
		Healthy: healthy, Pain: pain, Useless: useless,
	}
	return agg, js
}

// zoneLabel is the human-readable status shown in the table's Status column.
func zoneLabel(z metrics.Zone) string {
	switch z {
	case metrics.ZoneMainSequence:
		return "Main sequence"
	case metrics.ZonePain:
		return "Zone of Pain"
	case metrics.ZoneUseless:
		return "Zone of Uselessness"
	default:
		return "—"
	}
}

// buildScatter lays out the (Instability, Abstractness) plot: axes at 0/0.5/1, the
// A+I=1 main-sequence diagonal, and one point per typed package coloured by zone.
// In multi-repo mode each point is tagged with its repo and starts hidden unless it
// belongs to the default repo. Points are emitted in (repo, package) order for
// deterministic, testable output.
func buildScatter(typed []metrics.PackageMetric, multi bool, def string) metricScatter {
	s := metricScatter{
		Width: scatterWidth, Height: scatterHeight,
		PlotX: scatterMarginL, PlotY: scatterMarginT,
		PlotW: scatterPlot, PlotH: scatterPlot,
	}

	// x = left + I*plot ; y = top + (1-A)*plot  (A=1 at the top).
	px := func(i float64) float64 { return scatterMarginL + i*scatterPlot }
	py := func(a float64) float64 { return scatterMarginT + (1-a)*scatterPlot }

	s.LineX1, s.LineY1 = px(0), py(1) // (I=0, A=1)
	s.LineX2, s.LineY2 = px(1), py(0) // (I=1, A=0)

	s.XAxisX, s.XAxisY = s.PlotX+s.PlotW/2, s.Height-1
	s.YAxisX, s.YAxisY = 11, s.PlotY+s.PlotH/2

	for _, t := range []struct {
		v   float64
		lbl string
	}{{0, "0"}, {0.5, "0.5"}, {1, "1"}} {
		x := px(t.v)
		s.XTicks = append(s.XTicks, axisTick{
			LineX1: x, LineY1: s.PlotY, LineX2: x, LineY2: s.PlotY + s.PlotH,
			LabelX: x, LabelY: s.PlotY + s.PlotH + scatterTickLabel, Label: t.lbl,
		})
		y := py(t.v)
		s.YTicks = append(s.YTicks, axisTick{
			LineX1: s.PlotX, LineY1: y, LineX2: s.PlotX + s.PlotW, LineY2: y,
			LabelX: s.PlotX - 8, LabelY: y + 4, Label: t.lbl,
		})
	}

	pts := append([]metrics.PackageMetric(nil), typed...)
	sort.SliceStable(pts, func(i, j int) bool {
		if pts[i].Repo != pts[j].Repo {
			return pts[i].Repo < pts[j].Repo
		}
		return pts[i].Package < pts[j].Package
	})
	for _, m := range pts {
		s.Points = append(s.Points, metricPoint{
			CX:   px(m.Instability),
			CY:   py(m.Abstractness),
			Zone: metrics.Classify(m),
			Repo: m.Repo,
			Label: fmt.Sprintf("%s — I=%.2f A=%.2f D=%.2f",
				m.Package, m.Instability, m.Abstractness, m.Distance),
			Hidden: multi && m.Repo != def,
		})
	}

	return s
}
