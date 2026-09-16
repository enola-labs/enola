package dashboard

import (
	"strconv"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/metrics"
)

// sampleMetrics is a single-repo set (empty Repo) covering three zones plus a
// type-less package, which the view excludes from display and averages.
func sampleMetrics() []metrics.PackageMetric {
	return []metrics.PackageMetric{
		{Package: "core.domain", ClassesInterfaces: 8, Ca: 4, Ce: 4, Instability: 0.5, Abstractness: 0.5, Distance: 0.0},         // main sequence
		{Package: "features.reporting", ClassesInterfaces: 10, Ca: 0, Ce: 6, Instability: 1.0, Abstractness: 0.9, Distance: 0.9}, // useless
		{Package: "shared.utils", ClassesInterfaces: 10, Ca: 8, Ce: 0, Instability: 0.0, Abstractness: 0.1, Distance: 0.9},       // pain
		{Package: "typeless.pkg", ClassesInterfaces: 0, Ca: 2, Ce: 3, Instability: 0.6, Abstractness: 0.0, Distance: 0.4},        // excluded (no types)
	}
}

// sampleMultiRepo tags packages across two repos, with a type-less package in one.
func sampleMultiRepo() []metrics.PackageMetric {
	return []metrics.PackageMetric{
		{Repo: "alpha", Package: "alpha/core", ClassesInterfaces: 8, Ca: 4, Ce: 4, Instability: 0.5, Abstractness: 0.5, Distance: 0.0},  // main seq
		{Repo: "alpha", Package: "alpha/util", ClassesInterfaces: 10, Ca: 8, Ce: 0, Instability: 0.0, Abstractness: 0.1, Distance: 0.9}, // pain
		{Repo: "beta", Package: "beta/api", ClassesInterfaces: 10, Ca: 0, Ce: 6, Instability: 1.0, Abstractness: 0.9, Distance: 0.9},    // useless
		{Repo: "beta", Package: "beta/empty", ClassesInterfaces: 0, Ca: 1, Ce: 0, Instability: 0.0, Abstractness: 0.0, Distance: 1.0},   // excluded (no types)
	}
}

func TestPackageMetricsView_Nil(t *testing.T) {
	if got := packageMetricsView(nil); got != nil {
		t.Errorf("packageMetricsView(nil) = %v, want nil", got)
	}
	// A set with only type-less packages has nothing to plot → nil.
	if got := packageMetricsView([]metrics.PackageMetric{{Package: "x", ClassesInterfaces: 0}}); got != nil {
		t.Errorf("packageMetricsView(type-less only) = %v, want nil", got)
	}
}

func TestPackageMetricsView_SingleRepoAggregate(t *testing.T) {
	v := packageMetricsView(sampleMetrics())
	if v == nil {
		t.Fatal("packageMetricsView returned nil for non-empty input")
	}
	if v.Multi {
		t.Error("Multi = true, want false for an untagged (single-repo) set")
	}
	if v.TotalAnalyzed != 3 {
		t.Errorf("TotalAnalyzed = %d, want 3 (type-less excluded)", v.TotalAnalyzed)
	}
	// Averages over the 3 typed packages, matching package_metrics.
	agg := v.Agg
	for _, c := range []struct{ name, got, want string }{
		{"Analyzed", strconv.Itoa(agg.Analyzed), "3"},
		{"AvgCa", agg.AvgCa, "4.0"},
		{"AvgCe", agg.AvgCe, "3.3"},
		{"AvgI", agg.AvgI, "0.50"},
		{"AvgA", agg.AvgA, "0.50"},
		{"AvgD", agg.AvgD, "0.60"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if agg.HealthyCount != 1 || agg.PainCount != 1 || agg.UselessCount != 1 {
		t.Errorf("zone counts = healthy %d pain %d useless %d, want 1/1/1",
			agg.HealthyCount, agg.PainCount, agg.UselessCount)
	}
}

func TestPackageMetricsView_RowsTypedOnlyRankedByDistance(t *testing.T) {
	v := packageMetricsView(sampleMetrics())
	if len(v.Rows) != 3 {
		t.Fatalf("rows = %d, want 3 (type-less excluded)", len(v.Rows))
	}
	// Worst (highest D) first; the two D=0.90 packages tie-break by name.
	wantOrder := []string{"features.reporting", "shared.utils", "core.domain"}
	for i, want := range wantOrder {
		if v.Rows[i].Package != want {
			t.Errorf("row[%d] = %q, want %q", i, v.Rows[i].Package, want)
		}
	}
	if v.Rows[0].Status != "Zone of Uselessness" || v.Rows[0].Zone != metrics.ZoneUseless {
		t.Errorf("row[0] status = %q/%q, want Zone of Uselessness/useless", v.Rows[0].Status, v.Rows[0].Zone)
	}
	if v.Rows[1].Status != "Zone of Pain" || v.Rows[1].Zone != metrics.ZonePain {
		t.Errorf("row[1] status = %q/%q, want Zone of Pain/pain", v.Rows[1].Status, v.Rows[1].Zone)
	}
	for _, r := range v.Rows {
		if r.Hidden {
			t.Errorf("row %q Hidden = true, want false in single-repo mode", r.Package)
		}
	}
}

func TestPackageMetricsView_MultiRepo(t *testing.T) {
	v := packageMetricsView(sampleMultiRepo())
	if v == nil {
		t.Fatal("nil view")
	}
	if !v.Multi {
		t.Fatal("Multi = false, want true for a repo-tagged set")
	}
	if got := strings.Join(v.Repos, ","); got != "alpha,beta" {
		t.Errorf("Repos = %q, want alpha,beta (sorted)", got)
	}
	if v.Default != "alpha" {
		t.Errorf("Default = %q, want alpha", v.Default)
	}
	if v.TotalAnalyzed != 3 {
		t.Errorf("TotalAnalyzed = %d, want 3", v.TotalAnalyzed)
	}
	// Headline Agg reflects the default repo (alpha: 1 main-seq + 1 pain, avg D 0.45).
	if v.Agg.Analyzed != 2 || v.Agg.HealthyCount != 1 || v.Agg.PainCount != 1 || v.Agg.AvgD != "0.45" {
		t.Errorf("default Agg = %+v, want analyzed 2 / healthy 1 / pain 1 / D 0.45", v.Agg)
	}
	// Per-repo aggregate map carries both repos.
	js := string(v.AggMapJS)
	if !strings.Contains(js, `"alpha"`) || !strings.Contains(js, `"beta"`) {
		t.Errorf("AggMapJS missing a repo: %s", js)
	}
	// Non-default repo rows/points start hidden; default repo visible.
	hiddenByRepo := map[string]bool{}
	for _, r := range v.Rows {
		hiddenByRepo[r.Repo] = r.Hidden
	}
	if hiddenByRepo["alpha"] || !hiddenByRepo["beta"] {
		t.Errorf("row Hidden by repo = %v, want alpha visible / beta hidden", hiddenByRepo)
	}
	// Type-less beta/empty is excluded from rows and points.
	if len(v.Rows) != 3 || len(v.Scatter.Points) != 3 {
		t.Errorf("rows/points = %d/%d, want 3/3 (type-less excluded)", len(v.Rows), len(v.Scatter.Points))
	}
}

func TestBuildScatter_Geometry(t *testing.T) {
	typed := []metrics.PackageMetric{
		{Package: "core.domain", ClassesInterfaces: 8, Ca: 4, Ce: 4, Instability: 0.5, Abstractness: 0.5, Distance: 0.0},
		{Package: "shared.utils", ClassesInterfaces: 10, Ca: 8, Ce: 0, Instability: 0.0, Abstractness: 0.1, Distance: 0.9},
	}
	s := buildScatter(typed, false, "")

	if s.Width != 412 || s.Height != 404 {
		t.Errorf("size = %vx%v, want 412x404", s.Width, s.Height)
	}
	// Main-sequence line: (I=0,A=1) top-left → (I=1,A=0) bottom-right.
	if s.LineX1 != 40 || s.LineY1 != 12 || s.LineX2 != 400 || s.LineY2 != 372 {
		t.Errorf("main-seq line = (%v,%v)-(%v,%v), want (40,12)-(400,372)",
			s.LineX1, s.LineY1, s.LineX2, s.LineY2)
	}
	if len(s.Points) != 2 {
		t.Fatalf("points = %d, want 2", len(s.Points))
	}
	// Points are emitted in package-name order; core.domain (I=0.5,A=0.5) sits dead
	// centre and on the line.
	p := s.Points[0]
	if p.CX != 220 || p.CY != 192 || p.Zone != metrics.ZoneMainSequence {
		t.Errorf("core.domain point = (%v,%v) zone %q, want (220,192) main-sequence", p.CX, p.CY, p.Zone)
	}
}

// The panel renders through the page itself now rather than an overlay, so the
// test renders the real template. That is the half a fragment harness could not
// check: a field the page root does not carry fails only at render time.
//
// Cloning rather than executing baseTmpl directly, for the reason the server has:
// an executed template can no longer be cloned.
func renderPage(t *testing.T, view *metricsView) string {
	t.Helper()
	tmpl, err := baseTmpl.Clone()
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, pageData{PackageMetrics: view}); err != nil {
		t.Fatalf("page execute: %v", err)
	}
	return b.String()
}

func TestRenderPackageMetricsPanel(t *testing.T) {
	// Single-repo: trigger card, modal, scatter and table all render; no selector.
	body := renderPage(t, packageMetricsView(sampleMetrics()))
	for _, want := range []string{
		`onclick="openModal('package-metrics-modal')"`, // clickable card in the receipt grid
		`id="package-metrics-modal"`,                   // modal container
		`<svg class="metric-scatter"`,                  // inline SVG scatter
		`class="main-seq"`,                             // main-sequence diagonal
		"Instability (I)", "Abstractness (A)",          // axis titles
		"Zone of Uselessness", "Zone of Pain", "Main sequence", // status labels
		"Avg distance (D)",                  // aggregate card
		"features.reporting", "core.domain", // table rows
	} {
		if !strings.Contains(body, want) {
			t.Errorf("single-repo page missing %q", want)
		}
	}
	if strings.Contains(body, `id="metric-repo"`) {
		t.Error("single-repo page has a repo selector, want none")
	}

	// Multi-repo: selector, Repo column, per-repo agg script, hidden non-default rows.
	mbody := renderPage(t, packageMetricsView(sampleMultiRepo()))
	for _, want := range []string{
		`id="metric-repo"`,                        // repo <select>
		`onchange="selectMetricRepo(this.value)"`, // selector handler
		`<th>Repo</th>`,                           // repo column header
		`window.METRIC_AGG =`,                     // per-repo aggregate map
		`data-repo="beta"`,                        // repo-tagged rows/points
		"alpha/util", "beta/api",                  // rows from both repos
		`· 2 repos`, // trigger card annotation
	} {
		if !strings.Contains(mbody, want) {
			t.Errorf("multi-repo page missing %q", want)
		}
	}
	if !strings.Contains(mbody, `data-repo="beta"`) || !strings.Contains(mbody, `style="display:none"`) {
		t.Error("multi-repo page should hide non-default (beta) rows/points")
	}

	// No typed package: neither the card nor the modal container appears, rather
	// than an empty panel. selectMetricRepo names the id unconditionally, so match
	// the container itself.
	off := renderPage(t, nil)
	if strings.Contains(off, `id="package-metrics-modal"`) {
		t.Error("modal rendered with no view")
	}
	if strings.Contains(off, `openModal('package-metrics-modal')`) {
		t.Error("trigger card rendered with no view")
	}
}
