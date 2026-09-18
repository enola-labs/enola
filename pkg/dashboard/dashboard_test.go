package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/enola-labs/enola/pkg/facts"
	"github.com/enola-labs/enola/pkg/history"
)

func TestSummarizeSnapshotExplainsFreshnessAndDirtyCapture(t *testing.T) {
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	r := &facts.Receipt{
		SnapshotID:  "sha256:b3640f5cdab13ea7",
		GeneratedAt: "2026-08-22T09:55:00Z",
		RepoPath:    "/work/dbt-core",
		Git:         &facts.GitInfo{Commit: "f5a3aa5b1d5b0f7d", Dirty: true},
	}
	got := summarizeSnapshot(r, now)
	if got.RepoName != "dbt-core" || got.Age != "5m 0s ago" || got.ShortID != "b3640f5cdab1" || got.ShortCommit != "f5a3aa5b" {
		t.Fatalf("summary = %+v", got)
	}
	if got.Status != "Captured with local changes" || got.StatusClass != "warn" {
		t.Fatalf("status = %q/%q, want dirty warning", got.Status, got.StatusClass)
	}
}

func TestAssessQualityPrioritizesActionableBlindSpots(t *testing.T) {
	r := &facts.Receipt{Quality: facts.ReceiptQuality{Census: &facts.FileCensus{
		FilesWalked:   1000,
		ExcludedKinds: map[string]int{".sql": 40, ".png": 10, ".toml": 5},
		TopSkipCauses: []facts.CensusCause{
			{Cause: "claimed by cpp, which did not run this snapshot", Count: 2},
			{Cause: "claimed by rust, no facts emitted", Count: 3},
		},
	}}}
	got := assessQuality(r)
	if got.Status != "Coverage worth reviewing" || got.StatusClass != "warn" || got.CoverageLabel != "limited coverage" {
		t.Fatalf("status = %q/%q", got.Status, got.StatusClass)
	}
	if len(got.Potential) != 2 || got.Potential[0].Kind != ".sql" || got.Potential[1].Kind != ".toml" {
		t.Fatalf("potential = %+v, want SQL then TOML", got.Potential)
	}
	if got.ExpectedFiles != 10 || got.ExpectedKinds != 1 || len(got.Inactive) != 1 || len(got.NoFacts) != 1 || got.InactiveShare != "0.2%" {
		t.Fatalf("assessment buckets = %+v", got)
	}
}

func TestAssessQualityEscalatesOnlyMaterialExtractorGaps(t *testing.T) {
	for _, tt := range []struct {
		name, want string
		files      int
		walked     int
	}{
		{name: "small nested population", files: 2, walked: 2987, want: "Coverage worth reviewing"},
		{name: "five percent of corpus", files: 50, walked: 1000, want: "Material extractor coverage gap"},
		{name: "large absolute population", files: 100, walked: 10000, want: "Material extractor coverage gap"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &facts.Receipt{Quality: facts.ReceiptQuality{Census: &facts.FileCensus{
				FilesWalked: tt.walked,
				TopSkipCauses: []facts.CensusCause{{
					Cause: "claimed by cpp, which did not run this snapshot", Count: tt.files,
				}},
			}}}
			if got := assessQuality(r); got.Status != tt.want {
				t.Fatalf("status = %q, want %q (%+v)", got.Status, tt.want, got)
			}
		})
	}
}

func TestReadChangeSummaryMatchesLoadedSnapshotInConfiguredHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	const historyDir = ".enola/history"
	root, err := history.Root(repo, historyDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	entries := []history.Entry{
		{ID: "sha256:loaded", Summary: history.Summary{FactsAdded: 2, EdgesAdded: 7}},
		{ID: "sha256:newer", Summary: history.Summary{FactsRemoved: 99}},
	}
	var lines []byte
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, append(line, '\n')...)
	}
	if err := os.WriteFile(filepath.Join(root, history.LogFileName), lines, 0o644); err != nil {
		t.Fatal(err)
	}

	got := readChangeSummary(repo, "sha256:loaded", historyDir, insightLabels)
	if !got.Available || got.FactsAdded != 2 || got.EdgesAdded != 7 || got.FactsRemoved != 0 {
		t.Fatalf("summary = %+v, want loaded snapshot rather than newest history entry", got)
	}
	if got := readChangeSummary(repo, "sha256:missing", historyDir, insightLabels); got.Available {
		t.Fatalf("missing snapshot unexpectedly returned %+v", got)
	}
}

func TestNewFindingsCardLinksToDrillDown(t *testing.T) {
	tmpl, err := buildTemplate()
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	err = tmpl.Execute(&body, pageData{
		Title: "enola",
		Changes: changeSummary{
			Available:   true,
			FindingsNew: 1,
			NewFindingDetails: []changeFinding{{
				Label: "Dependency cycles", Title: "Cyclic dependency detected",
				Confidence: 100, Evidence: "src/core",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`onclick="openModal('new-findings-modal')"`, `id="new-findings-modal"`,
		"Cyclic dependency detected", "src/core",
	} {
		if !strings.Contains(body.String(), want) {
			t.Errorf("page missing new-finding drill-down content %q", want)
		}
	}
}

// fakeArtifacts is a stub engineView for handler tests. activeRepo/outputDir
// drive the on-disk receipt fallback (empty by default → no fallback). store is
// nil by default, so graphDetails yields empty lists and the counters render as
// plain (non-clickable) numbers.
type fakeArtifacts struct {
	receipt    []byte
	insights   []byte
	err        error
	activeRepo string
	outputDir  string
	store      *facts.Store
	snapshot   *facts.Snapshot
	graph      *facts.GraphReceipt
}

func (f fakeArtifacts) GetArtifact(name string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if name == "insights.json" {
		return f.insights, nil
	}
	return f.receipt, nil
}

func (f fakeArtifacts) ActiveRepo() string { return f.activeRepo }

func (f fakeArtifacts) OutputDir(repoPath string) string { return f.outputDir }

func (f fakeArtifacts) DashboardState() (*facts.Store, *facts.Snapshot, *facts.GraphReceipt) {
	return f.store, f.snapshot, f.graph
}

// newTestServer builds a Server exactly as Start does — parsed template and
// merged insight labels included — but without binding a port. Constructing the
// struct literally instead would leave those nil, so a handler test would panic
// rather than exercise what the real server renders.
func newTestServer(port int, eng engineView, opts ...Options) *Server {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	tmpl, err := buildTemplate()
	if err != nil {
		panic(err)
	}
	return &Server{port: port, eng: eng, opts: o, tmpl: tmpl}
}

// isolateHome points HOME at an empty temp dir so status.ServerSnapshot (which
// reads ~/.enola/usage) and the graph-receipt read (~/.enola/receipt.json) are
// deterministic and start empty.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestHandlerDegradesGracefully(t *testing.T) {
	isolateHome(t)
	s := newTestServer(54321, fakeArtifacts{err: errors.New("no snapshot generated")})

	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	wantContains := []string{
		"Refresh",                        // explicit disk refresh remains available
		"Ready to map this repository.",  // product-facing empty state
		"No snapshot loaded yet",         // degraded current receipt
		"No graph loaded in this server", // degraded graph receipt
	}
	for _, want := range wantContains {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Contains(body, "setInterval") || strings.Contains(body, "refreshSnapshot") {
		t.Error("dashboard must refresh only when the user requests it")
	}
}

func TestRefreshFailureIsVisibleAndKeepsServingThePage(t *testing.T) {
	isolateHome(t)
	s := newTestServer(54321, fakeArtifacts{}, Options{Refresh: func() (bool, error) {
		return false, errors.New("restore failed")
	}})

	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Could not load the latest snapshot") {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerRendersReceipts(t *testing.T) {
	home := isolateHome(t)

	receipt := facts.Receipt{
		SnapshotID:   "snap-abc123",
		EnolaVersion: "9.9.9",
		Extractors:   []string{"goextractor", "tsextractor"},
		FactCount:    4242,
	}
	rb, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}

	// The graph panel must come from THIS engine. To prove it does, plant a
	// different graph in the machine-wide ~/.enola/receipt.json — the file another
	// running server would have overwritten — and assert none of it reaches the page.
	graph := &facts.GraphReceipt{
		SnapshotID:   "graph-xyz",
		ServiceCount: 7,
		Repos:        []facts.GraphRepoEntry{{Label: "backend", Path: "/x/backend", FactCount: 100}},
	}
	foreign := facts.GraphReceipt{
		SnapshotID: "graph-from-another-server",
		Repos:      []facts.GraphRepoEntry{{Label: "someone-elses-repo", Path: "/x/elsewhere"}},
	}
	fb, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".enola")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "receipt.json"), fb, 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(8080, fakeArtifacts{receipt: rb, graph: graph})
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	for _, want := range []string{
		"4242",                         // fact summary
		"Your architecture is mapped.", // product-facing populated state
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	for _, unwanted := range []string{"graph-from-another-server", "someone-elses-repo"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("body contains %q — the graph panel must render THIS engine's graph, "+
				"not the machine-wide receipt another server may have written", unwanted)
		}
	}
}

// TestReceiptDiskFallback covers the server-restart case: the in-memory receipt
// is blank (auto-loaded facts only), so the dashboard must fall back to the
// last-written <repo>/.enola/receipt.json on disk.
func TestReceiptDiskFallback(t *testing.T) {
	isolateHome(t)
	repo := t.TempDir()
	outDir := filepath.Join(repo, ".enola")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	diskReceipt := facts.Receipt{SnapshotID: "sha256:ondisk", FactCount: 77}
	db, err := json.Marshal(diskReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "receipt.json"), db, 0o644); err != nil {
		t.Fatal(err)
	}

	// In-memory receipt is blank (only RepoPath set, SnapshotID empty), mimicking
	// AutoLoadSnapshot; ActiveRepo/OutputDir point at the on-disk receipt.
	blank, err := json.Marshal(facts.Receipt{RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(9, fakeArtifacts{receipt: blank, activeRepo: repo, outputDir: outDir})

	rv := s.currentReceipt(nil)
	if rv == nil || rv.SnapshotID != "sha256:ondisk" || rv.FactCount != 77 {
		t.Fatalf("currentReceipt = %+v, want the on-disk receipt", rv)
	}
}

func TestPublishedEmptyInsightsDoNotFallBackToStaleArtifact(t *testing.T) {
	stale, err := json.Marshal([]facts.Insight{{Source: "cycles", Title: "resolved finding", Confidence: 1}})
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(9, fakeArtifacts{insights: stale})
	current := &facts.Snapshot{Insights: []facts.Insight{}}
	if got := s.currentInsights(current); len(got) != 0 {
		t.Fatalf("currentInsights = %+v, want authoritative empty publication", got)
	}
}

func TestPageUsesOnePublishedDashboardState(t *testing.T) {
	isolateHome(t)
	st := facts.NewStore()
	st.Add(facts.Fact{Kind: facts.KindService, Name: "fresh-service"})
	fresh := &facts.Snapshot{Meta: facts.SnapshotMeta{SnapshotID: "fresh", FactCount: 1, RepoPath: "/fresh"}}
	graph := &facts.GraphReceipt{SnapshotID: "fresh", ServiceCount: 1}
	staleReceipt, err := json.Marshal(facts.Receipt{SnapshotID: "stale", FactCount: 99})
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(9, fakeArtifacts{
		receipt: staleReceipt, store: st, snapshot: fresh, graph: graph,
	})
	data := s.buildPageForModule("")
	if data.Receipt == nil || data.Receipt.SnapshotID != "fresh" || data.Graph != graph {
		t.Fatalf("page state = receipt %+v graph %+v, want one fresh publication", data.Receipt, data.Graph)
	}
	if len(data.Services) != 1 || data.Services[0].Name != "fresh-service" {
		t.Fatalf("services = %+v, want store captured with fresh publication", data.Services)
	}
}

func TestHandlerNotFound(t *testing.T) {
	isolateHome(t)
	s := newTestServer(1, fakeArtifacts{err: errors.New("no snapshot generated")})
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/other", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestStartBindsLoopbackPort(t *testing.T) {
	s, err := Start(nil, Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.Port() <= 0 {
		t.Errorf("port = %d, want > 0", s.Port())
	}
	if want := "http://127.0.0.1:"; !strings.HasPrefix(s.URL(), want) {
		t.Errorf("URL = %q, want prefix %q", s.URL(), want)
	}
}

func TestToolRows(t *testing.T) {
	total := map[string]int{"explore": 10, "query_facts": 5}
	session := map[string]int{"explore": 2}
	rows := toolRows(total, session)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Sorted by name: explore, query_facts.
	if rows[0].Name != "explore" || rows[0].Session != 2 || rows[0].Total != 10 {
		t.Errorf("row0 = %+v", rows[0])
	}
	if rows[1].Name != "query_facts" || rows[1].Session != 0 || rows[1].Total != 5 {
		t.Errorf("row1 = %+v", rows[1])
	}
}

// TestGraphDetails covers the store-backed enumeration directly: services sorted
// by name with their depends_on counts, one edge per depends_on relation sorted
// by consumer→provider, and empty lists for a nil store.
func TestGraphDetails(t *testing.T) {
	if s, e := graphDetails(nil); s != nil || e != nil {
		t.Fatalf("graphDetails(nil) = (%v, %v), want (nil, nil)", s, e)
	}

	st := facts.NewStore()
	st.Add(facts.Fact{Kind: facts.KindService, Name: "frontend", Relations: []facts.Relation{
		{Kind: facts.RelDependsOn, Target: "backend"},
		{Kind: facts.RelImports, Target: "ignored"}, // non-edge relation is skipped
	}})
	st.Add(facts.Fact{Kind: facts.KindService, Name: "backend"})

	services, edges := graphDetails(st)
	if len(services) != 2 || services[0].Name != "backend" || services[1].Name != "frontend" {
		t.Fatalf("services = %+v, want [backend, frontend]", services)
	}
	if services[0].DependsOn != 0 || services[1].DependsOn != 1 {
		t.Errorf("depends-on counts = %d/%d, want 0/1", services[0].DependsOn, services[1].DependsOn)
	}
	if len(edges) != 1 || edges[0].Consumer != "frontend" || edges[0].Provider != "backend" {
		t.Fatalf("edges = %+v, want [frontend→backend]", edges)
	}
}

// TestHandlerRendersModals verifies the clickable counters and modal contents are
// rendered when the store holds service facts. The clickable count buttons live in
// the graph-receipt cards, so the engine must report a graph for them to render.
func TestHandlerRendersModals(t *testing.T) {
	isolateHome(t)

	graph := &facts.GraphReceipt{SnapshotID: "graph-xyz", ServiceCount: 2, CrossRepoEdgeCount: 1}

	st := facts.NewStore()
	st.Add(facts.Fact{Kind: facts.KindService, Name: "frontend", Relations: []facts.Relation{
		{Kind: facts.RelDependsOn, Target: "backend"},
	}})
	st.Add(facts.Fact{Kind: facts.KindService, Name: "backend"})

	s := newTestServer(8080, fakeArtifacts{err: errors.New("no receipt"), store: st, graph: graph})
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`id="services-modal"`,                   // services modal container
		`id="edges-modal"`,                      // edges modal container
		`onclick="openModal('services-modal')"`, // clickable services count
		`onclick="openModal('edges-modal')"`,    // clickable edges count
		"frontend", "backend",                   // service names / edge endpoints
		// Diagram view + header toggle for the edges modal.
		`id="edges-diagram"`,          // diagram view container
		`<svg class="edge-graph"`,     // inline SVG node-link diagram
		`id="edges-view-toggle"`,      // header toggle control
		`onclick="toggleEdgesView()"`, // toggle handler
		`id="edges-table"`,            // table view container (now a sibling)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestBuildEdgeDiagram covers the node-link layout: one node per service placed on
// the ring, one edge per drawable relation, all coordinates inside the viewBox, a
// deterministic result, and graceful handling of empty and unknown-endpoint inputs.
func TestBuildEdgeDiagram(t *testing.T) {
	if buildEdgeDiagram(nil, nil) != nil {
		t.Fatal("empty input should yield a nil diagram")
	}
	svcs := []serviceRow{{Name: "backend"}, {Name: "frontend"}, {Name: "mobile"}}
	edges := []edgeRow{
		{Consumer: "frontend", Provider: "backend"},
		{Consumer: "mobile", Provider: "backend"},
		{Consumer: "frontend", Provider: "ghost"},  // unknown provider → skipped
		{Consumer: "backend", Provider: "backend"}, // self-loop → skipped
	}
	d := buildEdgeDiagram(svcs, edges)
	if d == nil {
		t.Fatal("expected a diagram")
	}
	if len(d.Nodes) != 3 {
		t.Errorf("nodes = %d, want 3", len(d.Nodes))
	}
	if len(d.Edges) != 2 {
		t.Errorf("edges = %d, want 2 (unknown endpoint + self-loop dropped)", len(d.Edges))
	}
	for _, n := range d.Nodes {
		if n.X < 0 || n.X > d.Width || n.Y < 0 || n.Y > d.Height {
			t.Errorf("node %q at (%.1f,%.1f) outside viewBox %gx%g", n.Name, n.X, n.Y, d.Width, d.Height)
		}
		if n.Anchor != "start" && n.Anchor != "end" {
			t.Errorf("node %q anchor = %q, want start|end", n.Name, n.Anchor)
		}
	}
	// Deterministic: same inputs → identical layout.
	if d2 := buildEdgeDiagram(svcs, edges); !reflect.DeepEqual(d, d2) {
		t.Error("buildEdgeDiagram is not deterministic")
	}
}

// TestInsightDetails covers the store-independent grouping: groups with a proven
// (structural) finding rank first regardless of size, then by count desc; items
// within a group are sorted by confidence desc; the largest group's bar is at
// 100%; the structural/candidate split and evidence extraction are correct; and
// nil input yields empty output.
func TestInsightDetails(t *testing.T) {
	labels := insightLabels

	if g, s, c := insightDetails(nil, labels); g != nil || s != 0 || c != 0 {
		t.Fatalf("insightDetails(nil) = (%v, %d, %d), want empty", g, s, c)
	}

	ins := []facts.Insight{
		{Source: "hotspots", Title: "Hot symbol", Confidence: 0.65, Evidence: []facts.Evidence{{File: "a.go"}}},
		{Source: "hotspots", Title: "Alloc in loop", Confidence: 0.85},
		{Source: "hotspots", Title: "Boxing", Confidence: 0.65},
		{Source: "cycles", Title: "Import cycle", Confidence: 1.0, Evidence: []facts.Evidence{{Symbol: "foo"}}},
	}
	groups, structural, candidate := insightDetails(ins, labels)

	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	// cycles (1, but proven/structural) ranks before hotspots (3, heuristic) — a
	// real problem outranks a bigger bucket of candidates.
	if groups[0].Source != "cycles" || groups[0].BarPct != 33 || !groups[0].HasStructural {
		t.Errorf("group0 = %+v, want cycles/33%%/structural", groups[0])
	}
	if !groups[0].Items[0].Structural || groups[0].Items[0].Evidence != "foo" {
		t.Errorf("cycles item = %+v, want structural + evidence foo", groups[0].Items[0])
	}
	if groups[0].Items[0].Band != "structural" {
		t.Errorf("cycles band = %q, want structural", groups[0].Items[0].Band)
	}
	if groups[1].Source != "hotspots" || groups[1].Count != 3 || groups[1].BarPct != 100 || groups[1].HasStructural {
		t.Errorf("group1 = %+v, want hotspots/3/100%%/not structural", groups[1])
	}
	if groups[1].Label != "Hotspots" {
		t.Errorf("group1 label = %q, want Hotspots", groups[1].Label)
	}
	// Within hotspots, highest confidence first (85, 65, 65 by title).
	if groups[1].Items[0].Title != "Alloc in loop" || groups[1].Items[0].Confidence != 85 {
		t.Errorf("hotspots item0 = %+v, want Alloc in loop/85", groups[1].Items[0])
	}
	if groups[1].Items[0].Band != "high" || groups[1].Items[1].Band != "medium" {
		t.Errorf("candidate bands = %q/%q, want high/medium", groups[1].Items[0].Band, groups[1].Items[1].Band)
	}
	if structural != 1 || candidate != 3 {
		t.Errorf("split = %d structural / %d candidate, want 1/3", structural, candidate)
	}
}

// TestInsightDetailsExcludesInformationalFromBothCounts covers the fix for a real
// leak: an Informational finding (e.g. "Architecture pattern: declared") describes
// the graph rather than flagging a problem, and pkg/check never grades it — so it
// must not inflate the "structural" count just because it happens to sit at 1.0,
// nor count as a "candidate" needing a fix. It still renders in its group's Items.
func TestInsightDetailsExcludesInformationalFromBothCounts(t *testing.T) {
	ins := []facts.Insight{
		{Source: "domain", Title: "Architecture pattern: declared", Confidence: 1.0, Informational: true},
		{Source: "domain", Title: "Real boundary violation", Confidence: 1.0},
	}
	groups, structural, candidate := insightDetails(ins, insightLabels)
	if structural != 1 || candidate != 0 {
		t.Fatalf("split = %d/%d, want 1 structural / 0 candidate — the informational note must be counted in neither", structural, candidate)
	}
	if len(groups) != 1 || len(groups[0].Items) != 2 {
		t.Fatalf("groups = %+v, want one group with both items", groups)
	}
	var sawInformational bool
	for _, item := range groups[0].Items {
		if item.Title == "Architecture pattern: declared" {
			sawInformational = true
			if !item.Informational || item.Structural || item.Band != "informational" {
				t.Errorf("informational item = %+v, want Informational=true, Structural=false", item)
			}
		}
	}
	if !sawInformational {
		t.Fatalf("groups = %+v, want the informational item still present", groups)
	}
}

func TestInsightBandUsesFindingSemanticsBeforeRoundedDisplay(t *testing.T) {
	ins := []facts.Insight{
		{Source: "hotspots", Title: "Near certain candidate", Confidence: 0.999},
		{Source: "domain", Title: "Architecture context", Confidence: 1, Informational: true},
	}
	groups, _, _ := insightDetails(ins, insightLabels)
	bands := map[string]string{}
	for _, group := range groups {
		for _, item := range group.Items {
			bands[item.Title] = item.Band
		}
	}
	if bands["Near certain candidate"] != "high" || bands["Architecture context"] != "informational" {
		t.Fatalf("bands = %+v, want rounded 100%% candidate kept high and context kept informational", bands)
	}
}

// TestInsightDetailsCapsPreviewAndCarriesTopAction covers the overview's per-group
// preview cap (Items stays complete for the modal; Shown/Hidden bound what the
// overview renders inline) and that each row carries only the first suggested
// action, since explainers already order their own Actions most-direct-fix first.
func TestInsightDetailsCapsPreviewAndCarriesTopAction(t *testing.T) {
	ins := make([]facts.Insight, 0, 7)
	for i := 0; i < 7; i++ {
		in := facts.Insight{Source: "hotspots", Title: fmt.Sprintf("Hot symbol %d", i), Confidence: 0.7}
		if i == 0 {
			in.Actions = []string{"do X", "do Y"}
		}
		ins = append(ins, in)
	}
	groups, _, _ := insightDetails(ins, insightLabels)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	g := groups[0]
	if len(g.Items) != 7 || len(g.Shown) != 5 || g.Hidden != 2 {
		t.Fatalf("g = %+v, want 7 items / 5 shown / 2 hidden", g)
	}
	var found bool
	for _, item := range g.Items {
		if item.Title == "Hot symbol 0" {
			found = true
			if item.Action != "do X" {
				t.Errorf("action = %q, want only the first suggested action (do X)", item.Action)
			}
		}
	}
	if !found {
		t.Fatalf("items = %+v, want Hot symbol 0 present", g.Items)
	}
}

// insightLabels is an admission list, not decoration: a finding whose source has no
// label is dropped, and must not reach the groups OR the structural/candidate counts.
// A repo's .enola/insights.json can be written by a newer build, or by a third-party
// binary registering explainers of its own, and neither may put an unlabelled source
// on this page.
func TestInsightDetailsFiltersUnknownSources(t *testing.T) {
	ins := []facts.Insight{
		{Source: "cycles", Title: "Import cycle", Confidence: 1.0},
		{Source: "not-an-explainer", Title: "Slow loop", Confidence: 0.65},
		{Source: "also-unknown", Title: "Unused func", Confidence: 1.0},
		{Source: "", Title: "Unstamped", Confidence: 0.5},
	}

	groups, structural, candidate := insightDetails(ins, insightLabels)
	if len(groups) != 1 || groups[0].Source != "cycles" {
		t.Fatalf("groups = %+v, want only cycles", groups)
	}
	if structural != 1 || candidate != 0 {
		t.Errorf("split = %d/%d, want 1 structural / 0 candidate — dropped insights must not be counted", structural, candidate)
	}
}

func TestInsightDetailsAdmitsEveryCurrentBuiltInSource(t *testing.T) {
	sources := []string{
		"hotspots", "god-class", "exported-surface", "complexity-outliers",
		"dependency-depth", "cycles", "layers", "crossrepo", "coverage",
		"unused-routes", "domain", "query-loops", "entry-points",
		"messaging-coverage", "intent", "constraints",
	}
	ins := make([]facts.Insight, 0, len(sources))
	for _, source := range sources {
		ins = append(ins, facts.Insight{Source: source, Title: source, Confidence: 1})
	}

	groups, structural, candidate := insightDetails(ins, insightLabels)
	if len(groups) != len(sources) || structural != len(sources) || candidate != 0 {
		t.Fatalf("built-in insights = %d groups, %d/%d split; want %d groups, %d/0 split",
			len(groups), structural, candidate, len(sources), len(sources))
	}
}

// TestHandlerRendersInsightsModal verifies the clickable Insights counter and the
// modal's grouped bars render when insights.json is available via GetArtifact. The
// engine reports a graph so the graph card (which hosts one clickable counter)
// renders.
func TestHandlerRendersInsightsModal(t *testing.T) {
	isolateHome(t)

	graph := &facts.GraphReceipt{SnapshotID: "graph-xyz", InsightCount: 2}

	insights := []facts.Insight{
		{Source: "god-class", Title: "UserService is a god class", Confidence: 0.9},
		{Source: "cycles", Title: "Import cycle a → b → a", Confidence: 1.0},
	}
	ib, err := json.Marshal(insights)
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServer(8080, fakeArtifacts{insights: ib, graph: graph})
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`id="insights-modal"`,                   // modal container
		`onclick="openModal('insights-modal')"`, // clickable counter
		"God classes", "Dependency cycles",      // friendly labels
		"UserService is a god class", // an insight title
		"90%", "100%",                // confidence rendering
		"structural",   // structural chip on the 100% insight
		"1 <span",      // header split count
		">structural<", // header split label
		// confidence-band filter wiring
		`id="insight-band"`,
		"Structural (100%)",
		"Context",
		`onchange="filterInsights(this.value)"`,
		`data-band="high"`,
		`id="insight-shown"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestCoverageDetails covers the store-backed coverage enumeration: per-service
// classification, the unmatched-route list, sort order, and nil → empty.
func TestCoverageDetails(t *testing.T) {
	if s, u := coverageDetails(nil); s != nil || u != nil {
		t.Fatalf("coverageDetails(nil) = (%v, %v), want (nil, nil)", s, u)
	}

	st := facts.NewStore()
	// A connected service (one resolved depends_on edge).
	st.Add(facts.Fact{Kind: facts.KindService, Name: "frontend",
		Props: map[string]any{"edge_coverage": []map[string]any{
			{"detected": 139, "resolved": 131, "external": 0, "unresolved": 8},
		}},
		Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "backend"}},
	})
	// A coverage-gap service: outbound detected but nothing resolved.
	st.Add(facts.Fact{Kind: facts.KindService, Name: "app",
		Props: map[string]any{"edge_coverage": []map[string]any{
			{"detected": 5, "resolved": 0, "external": 0, "unresolved": 5},
		}},
	})
	st.Add(facts.Fact{Kind: facts.KindRoute, Name: "/v2/hide.json", Repo: "frontend",
		File: "a.jsx", Line: 18,
		Props: map[string]any{"method": "POST", "unmatched_by_server": true, "unmatched_reason": "path_unknown"}})
	st.Add(facts.Fact{Kind: facts.KindRoute, Name: "/matched", Repo: "frontend",
		Props: map[string]any{"method": "GET"}}) // not unmatched → excluded

	services, unresolved := coverageDetails(st)
	if len(services) != 2 || services[0].Service != "app" || services[1].Service != "frontend" {
		t.Fatalf("services = %+v, want [app, frontend]", services)
	}
	if services[0].Status != svcCoverageGap {
		t.Errorf("app status = %q, want %q", services[0].Status, svcCoverageGap)
	}
	if services[1].Status != svcConnected || services[1].Resolved != 1 || services[1].Unresolved != 8 {
		t.Errorf("frontend row = %+v, want connected/resolved 1/unresolved 8", services[1])
	}
	if len(unresolved) != 1 || unresolved[0].Path != "/v2/hide.json" || unresolved[0].Method != "POST" || unresolved[0].Reason != "path_unknown" {
		t.Fatalf("unresolved = %+v, want one POST /v2/hide.json path_unknown", unresolved)
	}
}

// TestHandlerRendersQualityModals verifies the clickable Extraction-quality cards
// and their modals render from the receipt samples and the live store.
func TestHandlerRendersQualityModals(t *testing.T) {
	isolateHome(t)

	receipt := facts.Receipt{
		SnapshotID: "snap-q",
		Quality: facts.ReceiptQuality{
			FilesSeen:        80,
			FilesParsed:      60,
			FilesSkipped:     0,
			DirsSkipped:      1,
			SkippedSample:    []string{".git/ (glob: .git/**)"},
			ParseErrors:      1,
			ParseErrorSample: []facts.ParseError{{Extractor: "goextractor", File: "bad.go", Msg: "syntax error near EOF"}},
			Coverage:         &facts.CoverageSummary{ServicesTotal: 1, UnresolvedEdges: 1},
			Census: &facts.FileCensus{
				FilesWalked:      100,
				Parsed:           60,
				ExcludedByIgnore: 10,
				ExcludedByKind:   25,
				ExcludedKinds:    map[string]int{".sql": 25},
				SkippedWithCause: 5,
				TopSkipCauses:    []facts.CensusCause{{Cause: "claimed by go, no facts emitted", Count: 5}},
			},
		},
	}
	rb, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}

	st := facts.NewStore()
	st.Add(facts.Fact{Kind: facts.KindService, Name: "frontend",
		Props:     map[string]any{"edge_coverage": []map[string]any{{"detected": 9, "resolved": 8, "external": 0, "unresolved": 1}}},
		Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "backend"}}})
	st.Add(facts.Fact{Kind: facts.KindRoute, Name: "/v2/hide.json", Repo: "frontend",
		Props: map[string]any{"method": "POST", "unmatched_by_server": true, "unmatched_reason": "path_unknown"}})

	s := newTestServer(8080, fakeArtifacts{receipt: rb, store: st})
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "Report extraction issue") || !strings.Contains(body, "github.com/enola-labs/enola/issues/new") {
		t.Error("parse errors should offer a prefilled extraction issue")
	}
	for _, want := range []string{
		`id="coverage-modal"`, `id="coverage-unresolved-modal"`, `id="skipped-modal"`, `id="parse-errors-modal"`,
		`Files walked`, `Intentionally ignored`, `Non-source / unsupported`, `No facts emitted`,
		`Ignored and non-source files aren't parse failures`, `.sql`, `claimed by go, no facts emitted`,
		`onclick="openModal('coverage-modal')"`,
		`onclick="openModal('coverage-unresolved-modal')"`,
		`onclick="openModal('skipped-modal')"`,
		`onclick="openModal('parse-errors-modal')"`,
		".git/ (glob: .git/**)", // skipped sample entry
		"syntax error near EOF", // parse-error message
		"/v2/hide.json",         // unmatched route path
		"path_unknown",          // unmatched reason
		"connected",             // service status
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestValueRowsHasTotal(t *testing.T) {
	rows, total := valueRows(map[string]int{"explore": 3}, map[string]int{"explore": 24_000})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Label != "explore" || rows[0].Calls != 3 {
		t.Errorf("row = %+v", rows[0])
	}
	if total.Label != "TOTAL" || total.Calls != 3 {
		t.Errorf("total = %+v", total)
	}
	// The page must render the credit that was recorded, so the dashboard and
	// --status can never disagree about the same usage file.
	if rows[0].TokensSaved != "24.0K" {
		t.Errorf("TokensSaved = %q, want 24.0K", rows[0].TokensSaved)
	}
}

// html/template refuses to Clone a template that has already executed, so a
// server must never render straight from the package-level base. Starting a
// second dashboard in a process where the first has already served a page is
// exactly the order that regressed.
func TestSecondServerCanStartAfterFirstHasRendered(t *testing.T) {
	isolateHome(t)

	first := newTestServer(1, fakeArtifacts{err: errors.New("no snapshot")})
	first.handleIndex(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if _, err := buildTemplate(); err != nil {
		t.Fatalf("template build after a render: %v", err)
	}
}

// The page is branded in the title, the heading and the footer, all from one constant.
func TestTitleIsTheProductName(t *testing.T) {
	isolateHome(t)

	rec := httptest.NewRecorder()
	newTestServer(1, fakeArtifacts{err: errors.New("none")}).handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	for _, want := range []string{"<title>enola — dashboard</title>", `<h1>enola<span class="brand-dot">.</span></h1>`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}
