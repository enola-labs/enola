// Package dashboard serves a read-only localhost HTTP dashboard alongside the
// enola MCP server. It binds a free ephemeral port on 127.0.0.1 and renders the
// same activity data as --status plus the
// contents of the current-snapshot and graph-wide receipt.json files, so a user
// can visually inspect what the snapshot captured.
//
// The dashboard is strictly a viewer: every request only reads through existing
// concurrency-safe accessors (the engine's published snapshot, the status usage
// files) and never mutates server state. All logging goes to stderr; stdout is
// reserved for the MCP stdio protocol.
//
// It is a public package so a wrapper binary can add panels of its own without
// forking the page: see Options for the template-overlay and insight-label
// extension points.
package dashboard

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/diff"
	"github.com/enola-labs/enola/pkg/facts"
	"github.com/enola-labs/enola/pkg/history"
	"github.com/enola-labs/enola/pkg/status"
)

//go:embed page.html.tmpl
var pageTemplate string

var baseTmpl = template.Must(template.New("dashboard").Parse(pageTemplate))

// OverlayBlocks names the template blocks an Options.Overlay may redefine, in
// page order. They are the stable half of the overlay contract — renaming one
// silently drops a wrapper's panel — so a wrapper can assert its fragment
// defines only these, and TestOverlayBlocksExistInPage keeps the page honest.
func OverlayBlocks() []string {
	return []string{"extra-styles", "extra-cards", "extra-modals", "extra-scripts"}
}

// Options configures a dashboard, and is the whole extension surface a wrapper
// binary has. A zero Options is the plain engine dashboard.
type Options struct {
	// Overlay is a template fragment that redefines any of the page's extension
	// blocks: "extra-styles" (inside <style>), "extra-cards" (end of the summary
	// card grid), "extra-modals" (after the last modal) and "extra-scripts"
	// (inside the trailing <script>). Each is empty by default.
	//
	// A fragment renders against the same page model as the rest of the template,
	// so it reaches its own data through {{.Extra}} and may reuse the page's CSS
	// classes — .card, .modal, .modal-panel, .count-link and .insight-summary are
	// the stable ones.
	Overlay string

	// Extra computes the data the overlay blocks render, once per request from
	// the live fact store, and is exposed to the template as {{.Extra}}. Leaving
	// it nil — or returning nil, e.g. for an unlicensed feature — renders the
	// blocks with no data, which a fragment guarded by {{if .Extra}} skips.
	Extra func(*facts.Store) any

	// InsightLabels adds explainer-id → display-label entries to the page's own
	// map. That map is also the admission list (see insightDetails), so a wrapper
	// that registers extra explainers MUST list them here or their insights are
	// filtered out of its own dashboard.
	InsightLabels map[string]string

	// Title names the product in the page title, heading and footer. Defaults to
	// defaultTitle.
	Title string

	// Tracker is this process's usage tracker, and the ONLY source the page uses
	// to describe the server it is served by — PID, uptime, dashboard port, the
	// graph loaded, and this process's own tool counts. Without it the page can
	// only render the cross-process aggregate, which with several servers running
	// would attribute a sibling's identity to this one.
	Tracker *status.Tracker

	// StablePort is an optional fixed loopback port this dashboard also listens
	// on, in addition to its own ephemeral one, so there is a URL worth
	// bookmarking. The first server to claim it becomes the "front door"; the
	// others retry in the background and take it over when that server exits.
	// Zero disables it.
	StablePort int

	// Refresh reloads the selected persisted snapshot when it changed. It is used
	// by dashboard-only processes; an MCP-hosted dashboard already sees its
	// engine's live publications. The bool reports whether a new snapshot was
	// published.
	Refresh func() (bool, error)

	// SnapshotPath and GenerateCommand support a useful empty state and keep
	// operational detail out of the primary UI unless the user asks for it.
	SnapshotPath            string
	GenerateCommand         string
	CurrentExtractorVersion string
}

// defaultTitle is the product name shown when Options.Title is empty.
const defaultTitle = "enola"

// engineView is the slice of the engine the dashboard depends on. Narrowing to
// an interface keeps the handler testable without constructing a full engine.
// *bootstrap.Engine satisfies it.
//
//   - GetArtifact fetches the live in-memory receipt ("receipt.json").
//   - ActiveRepo + OutputDir locate the last-written receipt on disk, used as a
//     fallback: AutoLoadSnapshot restores a snapshot's facts with an near-empty
//     Meta, so after a server restart the in-memory receipt is blank while a full
//     one persists at <repo>/.enola/receipt.json.
type engineView interface {
	GetArtifact(name string) ([]byte, error)
	ActiveRepo() string
	OutputDir(repoPath string) string
	// Store exposes the live fact store, from which the dashboard enumerates the
	// service and cross-repo-edge lists behind the graph-receipt counters (the
	// receipt itself stores only the counts). Reads are concurrency-safe.
	Store() *facts.Store
	// GraphReceipt describes the graph THIS engine holds, assembled in memory.
	// It is what the graph panel renders, so that panel and the store-derived
	// panels below it can never describe different repo sets — which reading the
	// machine-wide ~/.enola/receipt.json would allow whenever a second server is
	// running. Nil when no snapshot is loaded.
	GraphReceipt() *facts.GraphReceipt
}

// Server is a running dashboard HTTP server bound to a loopback port.
type Server struct {
	port   int
	eng    engineView
	opts   Options
	tmpl   *template.Template
	labels map[string]string // insight-source allowlist + display labels
	title  string            // product name in the page title/heading/footer
	mux    *http.ServeMux

	changeMu      sync.Mutex
	changeCacheID string
	changeCache   changeSummary

	graphMu       sync.Mutex
	graphCacheKey string
	graphCache    *moduleGraphView

	// frontDoor is set once this server claims the stable port. Read from every
	// request handler and written by the claim goroutine, hence atomic.
	frontDoor atomic.Bool
}

// Start binds a free ephemeral port on 127.0.0.1, serves the dashboard from a
// background goroutine, and returns immediately. A serve error after startup is
// logged to stderr and never propagated — the MCP server must keep running.
//
// An invalid Options.Overlay is reported here rather than at render time, so a
// wrapper's template mistake fails loudly at startup instead of silently
// dropping its panels from every page.
func Start(eng *bootstrap.Engine, opts Options) (*Server, error) {
	tmpl, err := buildTemplate(opts.Overlay)
	if err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("binding dashboard port: %w", err)
	}
	s := &Server{
		port:   ln.Addr().(*net.TCPAddr).Port,
		eng:    eng,
		opts:   opts,
		tmpl:   tmpl,
		labels: mergedLabels(opts.InsightLabels),
		title:  titleOr(opts.Title),
	}

	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/refresh", s.handleRefresh)

	go func() {
		if err := http.Serve(ln, s.mux); err != nil {
			log.Printf("dashboard: serve stopped: %v", err)
		}
	}()

	if opts.StablePort > 0 {
		go s.claimStablePort(opts.StablePort)
	}

	return s, nil
}

// DefaultStablePort is the shared, bookmarkable dashboard port used when neither
// the config nor the environment names one.
const DefaultStablePort = 7171

// StablePortEnv overrides the configured shared-URL port. Set it to "0" or "off"
// to disable the shared URL and keep only this server's ephemeral port.
const StablePortEnv = "ENOLA_DASHBOARD_PORT"

// ResolveStablePort decides which fixed port the dashboard should also listen on:
// ENOLA_DASHBOARD_PORT if set, else the configured port, else DefaultStablePort.
// Zero (from either source, or "off" in the environment) disables it; a negative
// configured value does the same. An unparseable environment value is reported and
// ignored rather than silently dropping the feature.
func ResolveStablePort(configured int) int {
	if v, ok := os.LookupEnv(StablePortEnv); ok {
		v = strings.TrimSpace(v)
		if v == "off" || v == "none" {
			return 0
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Printf("dashboard: ignoring %s=%q: not a port number", StablePortEnv, v)
		} else {
			return max(n, 0)
		}
	}
	switch {
	case configured < 0:
		return 0
	case configured == 0:
		return DefaultStablePort
	default:
		return configured
	}
}

// stableRetryInterval is how often a server that lost the race for the stable
// port re-tries it, so the bookmarkable URL is picked up again within seconds of
// the owning server exiting.
const stableRetryInterval = 5 * time.Second

// claimStablePort keeps trying to bind the stable loopback port and serves the
// same page from it once it succeeds. Exactly one server can hold the port at a
// time — the OS decides the race — and every server keeps retrying until it wins,
// so the URL survives the front-door terminal closing.
//
// It runs for the life of the process: if the listener ever fails, the loop
// resumes trying. Failures are logged at most once per state change; a port held
// by another server is the normal case, not an error.
func (s *Server) claimStablePort(port int) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			time.Sleep(stableRetryInterval)
			continue
		}

		s.frontDoor.Store(true)
		if s.opts.Tracker != nil {
			s.opts.Tracker.SetFrontDoor(true)
		}
		log.Printf("dashboard: serving the shared URL http://%s", addr)

		// Serve blocks until the listener breaks; then this server is no longer
		// the front door and the loop goes back to competing for the port.
		err = http.Serve(ln, s.mux)
		s.frontDoor.Store(false)
		if s.opts.Tracker != nil {
			s.opts.Tracker.SetFrontDoor(false)
		}
		log.Printf("dashboard: shared URL listener stopped: %v", err)
		time.Sleep(stableRetryInterval)
	}
}

// Port returns the loopback port the dashboard is listening on.
func (s *Server) Port() int { return s.port }

// URL returns the dashboard's localhost URL.
func (s *Server) URL() string { return fmt.Sprintf("http://127.0.0.1:%d", s.port) }

// handleIndex gathers live data on each request (so the periodic reload shows
// fresh numbers) and renders the page. Only the root path is served.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	if s.opts.Refresh != nil {
		if _, err := s.opts.Refresh(); err != nil {
			log.Printf("dashboard: refreshing snapshot: %v", err)
		}
	}
	data := s.buildPageForModule(r.URL.Query().Get("module"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		log.Printf("dashboard: render failed: %v", err)
	}
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/refresh" {
		http.NotFound(w, r)
		return
	}
	changed := false
	var err error
	if s.opts.Refresh != nil {
		changed, err = s.opts.Refresh()
	}
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"changed": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"changed": changed})
}

// titleOr returns the configured product name, or the default when unset.
func titleOr(title string) string {
	if title == "" {
		return defaultTitle
	}
	return title
}

// buildTemplate returns the page template for one server, with an optional
// overlay fragment applied over it. Parsing a fragment onto a clone redefines
// whichever extension blocks it declares and leaves the rest of the page
// untouched.
//
// Every server gets its own clone, even without an overlay: html/template
// refuses to Clone a template that has already executed, so serving a page
// straight from baseTmpl would make the NEXT server's Clone fail. Keeping
// baseTmpl un-executed is what lets a wrapper start a dashboard after a plain
// one in the same process.
func buildTemplate(overlay string) (*template.Template, error) {
	t, err := baseTmpl.Clone()
	if err != nil {
		return nil, fmt.Errorf("cloning dashboard template: %w", err)
	}
	if overlay == "" {
		return t, nil
	}
	if t, err = t.Parse(overlay); err != nil {
		return nil, fmt.Errorf("parsing dashboard overlay: %w", err)
	}
	return t, nil
}

// toolRow is one row of the tool-usage table (this server vs lifetime total).
type toolRow struct {
	Name    string
	Session int
	Total   int
}

// instanceRow is one row of the instances table: another enola server running
// right now, with the URL of its dashboard. Self marks the one serving this page.
type instanceRow struct {
	PID       int
	Binary    string
	Repos     string
	Uptime    string
	Calls     int
	URL       string
	Self      bool
	FrontDoor bool
}

// instanceRows projects the live-instance registry into table rows. selfPID is
// the process serving this page, which is highlighted rather than hidden — a
// user comparing two dashboards needs to see which row is the page they are on.
func instanceRows(instances []status.Instance, selfPID int, now time.Time) []instanceRow {
	rows := make([]instanceRow, 0, len(instances))
	for _, inst := range instances {
		rows = append(rows, instanceRow{
			PID:       inst.PID,
			Binary:    inst.Binary,
			Repos:     inst.RepoLabels(),
			Uptime:    formatDuration(now.Sub(inst.StartTime)),
			Calls:     inst.SessionCalls(),
			URL:       inst.URL(),
			Self:      inst.PID == selfPID,
			FrontDoor: inst.FrontDoor,
		})
	}
	return rows
}

// valueRow is one row of the value-estimate table (pre-formatted for display).
type valueRow struct {
	Label       string
	Calls       int
	TimeSaved   string
	TokensSaved string
}

// snapshotSummary turns receipt provenance into the user-facing state shown at
// the top of the Snapshots tab. The full receipt remains available below it.
type snapshotSummary struct {
	RepoName, Age, Generated, ShortID, ShortCommit, Status, StatusClass string
}

type qualityBlindSpot struct {
	Kind, Reason string
	Count        int
}

type qualityAssessment struct {
	Status, StatusClass, Summary string
	CoverageLabel                string
	Potential                    []qualityBlindSpot
	Expected                     []qualityBlindSpot
	Inactive                     []facts.CensusCause
	NoFacts                      []facts.CensusCause
	ExpectedFiles, ExpectedKinds int
	InactiveFiles, FilesWalked   int
	InactiveShare                string
}

type changeSummary struct {
	Available, Incomparable, Initial bool
	Headline, ComparedTo             string
	FactsAdded, FactsRemoved         int
	EdgesAdded, EdgesRemoved         int
	FindingsNew, FindingsResolved    int
	NewFindingDetails                []changeFinding
}

type changeFinding struct {
	Source, Label, Title, Evidence string
	Confidence                     int
}

// pageData is the full template model.
type pageData struct {
	Title           string
	SnapshotPath    string
	GenerateCommand string
	VersionNotice   string

	// This server's own identity. Never sourced from the cross-process aggregate:
	// with several agent terminals open, that would name a sibling process.
	Running       bool
	PID           int
	Port          int
	Uptime        string
	StartedAt     string
	TrackingSince string
	ReposTracked  int
	Binary        string
	ConfigPath    string
	WorkDir       string
	ReposLoaded   string
	SessionCalls  int

	// FrontDoor is true when this server currently owns the stable URL, and
	// StableURL names it (empty when the feature is off).
	FrontDoor bool
	StableURL string

	// Instances is every enola server running right now, this one included, so a
	// user with several terminals open can see them all and switch between them.
	Instances []instanceRow

	Tools      []toolRow
	Values     []valueRow
	ValueTotal valueRow
	// BeyondContext marks that some indexed corpus exceeded what an agent can
	// hold at once, where the token figure understates what enola did.
	BeyondContext bool

	HasReceipt  bool
	Receipt     *facts.Receipt
	Snapshot    snapshotSummary
	ReceiptNote string

	HasGraph  bool
	Graph     *facts.GraphReceipt
	GraphNote string

	// Services and CrossRepoEdges are enumerated from the live fact store (not the
	// receipt, which holds only counts) to back the clickable graph-receipt cards.
	Services       []serviceRow
	CrossRepoEdges []edgeRow
	// EdgeDiagram is the node-link layout of CrossRepoEdges for the diagram view of
	// the edges modal; nil when there are no edges (the modal shows the table only).
	EdgeDiagram *diagramView
	// ModuleGraph is the bounded, interactive module-level architecture map.
	ModuleGraph *moduleGraphView

	// Insights (grouped by explainer) back the clickable Insights card; the
	// structural/candidate split is shown in the modal header.
	Insights          []insightGroup
	InsightStructural int
	InsightCandidate  int
	InsightTotal      int // structural + candidate; initial "shown" count for the modal filter

	// Extraction-quality proof data: per-service coverage and the unmatched-route
	// list (from the live store), plus the capped skip/parse-error samples (from the
	// receipt) that back the clickable Extraction-quality cards.
	Coverage         []coverageRow
	UnresolvedRoutes []routeRow
	SkippedSample    []string
	ParseErrors      []facts.ParseError
	Quality          qualityAssessment
	Changes          changeSummary

	// Extra is whatever Options.Extra returned for this request — the data the
	// overlay blocks render. Nil in a plain engine dashboard, and nil whenever a
	// wrapper declines to supply it (e.g. an unlicensed feature), which is what a
	// fragment guarded by {{if .Extra}} keys off.
	Extra any
}

// buildPageForModule collects the status, current-snapshot receipt and
// graph-wide receipt into the template model, focusing the architecture map on
// module if non-empty. Every source degrades gracefully to a note on error.
func (s *Server) buildPageForModule(module string) pageData {
	data := pageData{
		Title:           s.title,
		Port:            s.port,
		SnapshotPath:    s.opts.SnapshotPath,
		GenerateCommand: s.opts.GenerateCommand,
	}

	now := time.Now()

	// Identity comes from THIS process's tracker. A user running one server per
	// agent terminal must be able to tell, from the page alone, which one they are
	// looking at — so nothing here may come from the cross-process aggregate.
	var self status.Instance
	if s.opts.Tracker != nil {
		self = s.opts.Tracker.Self()
		data.Running = true
		data.PID = self.PID
		data.Binary = self.Binary
		if self.Version != "" {
			data.Binary += " " + self.Version
		}
		data.ConfigPath = self.ConfigPath
		data.WorkDir = self.WorkDir
		data.ReposLoaded = self.RepoLabels()
		data.SessionCalls = self.SessionCalls()
		if !self.StartTime.IsZero() {
			data.StartedAt = self.StartTime.Format("2006-01-02 15:04:05")
			data.Uptime = formatDuration(now.Sub(self.StartTime))
		}
		data.Tools = toolRows(nil, self.SessionCounts) // totals filled in below
	}

	data.FrontDoor = s.frontDoor.Load()
	if s.opts.StablePort > 0 {
		data.StableURL = fmt.Sprintf("http://127.0.0.1:%d", s.opts.StablePort)
	}

	// Lifetime totals and the per-repo tracking window are genuinely cross-process
	// figures, and are labelled as such on the page.
	ss := status.ServerSnapshot()
	if ss.Found {
		data.ReposTracked = ss.Repos
		if !ss.TrackingSince.IsZero() {
			data.TrackingSince = ss.TrackingSince.Format("2006-01-02 15:04:05")
		}
		data.Tools = toolRows(ss.GrandTotal, self.SessionCounts)
		data.Values, data.ValueTotal = valueRows(ss.GrandTotal, ss.GrandSaved)
		data.BeyondContext = ss.BeyondContext
		data.Instances = instanceRows(ss.Instances, self.PID, now)
	}
	if len(data.Instances) == 0 {
		data.Instances = instanceRows(status.LiveInstances(), self.PID, now)
	}

	// Current-snapshot receipt: prefer the live in-memory receipt, falling back
	// to the last-written one on disk (see currentReceipt).
	if rv := s.currentReceipt(); rv != nil {
		data.HasReceipt = true
		data.Receipt = rv
		data.Snapshot = summarizeSnapshot(rv, now)
		// Capped skip/parse-error samples back the clickable Extraction-quality cards.
		data.SkippedSample = rv.Quality.SkippedSample
		data.ParseErrors = rv.Quality.ParseErrorSample
		data.Quality = assessQuality(rv)
		if s.opts.CurrentExtractorVersion != "" && rv.ExtractorVersion != "" && rv.ExtractorVersion != s.opts.CurrentExtractorVersion {
			data.VersionNotice = "This snapshot uses a different extraction schema. Regenerate it before relying on comparisons or completeness."
		}
	} else {
		data.ReceiptNote = "No snapshot loaded yet — run generate_snapshot to populate this."
	}

	// Graph receipt: assembled from THIS engine's live graph, so the repo list
	// always describes the same store as the services/insights/coverage panels
	// below it. Reading the shared ~/.enola/receipt.json here would show whichever
	// repos another running server snapshotted last.
	if gv := s.eng.GraphReceipt(); gv != nil {
		data.HasGraph = true
		data.Graph = gv
	} else {
		data.GraphNote = "No graph loaded in this server yet — run generate_snapshot to populate this."
	}

	// Service and cross-repo-edge lists from the live store, backing the clickable
	// counters. Empty (store not loaded) → the cards render as plain numbers.
	data.Services, data.CrossRepoEdges = graphDetails(s.eng.Store())
	data.EdgeDiagram = buildEdgeDiagram(data.Services, data.CrossRepoEdges)
	currentSnapshotID := ""
	if data.Receipt != nil {
		currentSnapshotID = data.Receipt.SnapshotID
	}
	data.ModuleGraph = s.readModuleGraph(currentSnapshotID, module)
	data.Changes = s.readChangeSummary(s.eng.ActiveRepo(), currentSnapshotID)

	// Insight list (grouped by explainer) backing the clickable Insights counter.
	// Empty → the counter renders as a plain number.
	data.Insights, data.InsightStructural, data.InsightCandidate = insightDetails(s.currentInsights(), s.labels)
	data.InsightTotal = data.InsightStructural + data.InsightCandidate

	// Cross-repo coverage + unmatched routes from the live store, backing the
	// clickable Extraction-quality coverage cards.
	data.Coverage, data.UnresolvedRoutes = coverageDetails(s.eng.Store())

	// Whatever a wrapper's overlay blocks render, recomputed per request from the
	// same live store as everything above.
	if s.opts.Extra != nil {
		data.Extra = s.opts.Extra(s.eng.Store())
	}

	return data
}

func (s *Server) readModuleGraph(snapshotID, focus string) *moduleGraphView {
	key := snapshotID + "\x00" + focus
	s.graphMu.Lock()
	defer s.graphMu.Unlock()
	if snapshotID != "" && key == s.graphCacheKey {
		return s.graphCache
	}
	result := buildModuleGraphFocused(s.eng.Store(), focus)
	s.graphCacheKey, s.graphCache = key, result
	return result
}

func (s *Server) readChangeSummary(repoPath, snapshotID string) changeSummary {
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	if snapshotID != "" && snapshotID == s.changeCacheID {
		return s.changeCache
	}
	result := readChangeSummary(repoPath, snapshotID, s.labels)
	s.changeCacheID, s.changeCache = snapshotID, result
	return result
}

func readChangeSummary(repoPath, snapshotID string, labels map[string]string) changeSummary {
	if repoPath == "" || snapshotID == "" {
		return changeSummary{}
	}
	root, err := history.Root(repoPath, "")
	if err != nil {
		return changeSummary{}
	}
	entries, err := history.Read(root)
	if err != nil || len(entries) == 0 {
		return changeSummary{}
	}
	var entry history.Entry
	entryIndex := -1
	found := false
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].ID == snapshotID {
			entry = entries[i]
			entryIndex = i
			found = true
			break
		}
	}
	if !found {
		return changeSummary{}
	}
	s := entry.Summary
	result := changeSummary{
		Available: true, Incomparable: s.Incomparable, Initial: s.Initial,
		Headline: s.Headline(), ComparedTo: "previous recorded snapshot",
		FactsAdded: s.FactsAdded, FactsRemoved: s.FactsRemoved,
		EdgesAdded: s.EdgesAdded, EdgesRemoved: s.EdgesRemoved,
		FindingsNew: s.FindingsNew, FindingsResolved: s.FindingsResolved,
	}
	if !s.Incomparable && !s.Initial && entryIndex > 0 && entry.Blob != nil && entries[entryIndex-1].Blob != nil {
		previous, previousErr := history.Load(root, entries[entryIndex-1])
		current, currentErr := history.Load(root, entry)
		if previousErr == nil && currentErr == nil {
			delta := diff.Compute(previous, current)
			for _, finding := range delta.FindingsNew {
				label := labels[finding.Source]
				if label == "" {
					label = finding.Source
				}
				result.NewFindingDetails = append(result.NewFindingDetails, changeFinding{
					Source: finding.Source, Label: label, Title: finding.Title,
					Confidence: int(finding.Confidence*100 + 0.5), Evidence: firstEvidence(finding.Evidence),
				})
			}
		}
	}
	return result
}

var reviewableFileKinds = map[string]string{
	".sql":   "Queries and transformations may encode data architecture.",
	".toml":  "Project and dependency configuration may affect architecture.",
	".html":  "Templates may contain routes, calls, or application structure.",
	".j2":    "Templates may contain generated configuration or queries.",
	".jinja": "Templates may contain generated configuration or queries.",
	".pyi":   "Python type declarations may describe public interfaces.",
	".sh":    "Scripts may describe build and operational dependencies.",
	".ps1":   "Scripts may describe build and operational dependencies.",
}

func assessQuality(r *facts.Receipt) qualityAssessment {
	q := qualityAssessment{Status: "Analysis complete", StatusClass: "on", Summary: "No parse failures or obvious extractor gaps were recorded."}
	if census := r.Quality.Census; census != nil {
		for kind, count := range census.ExcludedKinds {
			if reason, review := reviewableFileKinds[kind]; review {
				q.Potential = append(q.Potential, qualityBlindSpot{Kind: kind, Count: count, Reason: reason})
			} else {
				q.ExpectedFiles += count
				q.ExpectedKinds++
				q.Expected = append(q.Expected, qualityBlindSpot{Kind: kind, Count: count})
			}
		}
		sort.Slice(q.Potential, func(i, j int) bool {
			if q.Potential[i].Count != q.Potential[j].Count {
				return q.Potential[i].Count > q.Potential[j].Count
			}
			return q.Potential[i].Kind < q.Potential[j].Kind
		})
		sort.Slice(q.Expected, func(i, j int) bool {
			if q.Expected[i].Count != q.Expected[j].Count {
				return q.Expected[i].Count > q.Expected[j].Count
			}
			return q.Expected[i].Kind < q.Expected[j].Kind
		})
		for _, cause := range census.TopSkipCauses {
			if strings.Contains(cause.Cause, "did not run") {
				q.Inactive = append(q.Inactive, cause)
				q.InactiveFiles += cause.Count
			} else {
				q.NoFacts = append(q.NoFacts, cause)
			}
		}
		q.FilesWalked = census.FilesWalked
		q.InactiveShare = formatShare(q.InactiveFiles, census.FilesWalked)
	}
	if r.Quality.ParseErrors > 0 {
		q.Status = "Analysis needs attention"
		q.StatusClass = "off"
		q.Summary = fmt.Sprintf("%d parse error(s) may leave gaps in the architecture map.", r.Quality.ParseErrors)
	} else if q.InactiveFiles > 0 && materialCoverageGap(q.InactiveFiles, q.FilesWalked) {
		q.Status = "Material extractor coverage gap"
		q.StatusClass = "off"
		q.CoverageLabel = "material gap"
		q.Summary = fmt.Sprintf("%d of %d walked files (%s) belonged to an extractor that did not run.", q.InactiveFiles, q.FilesWalked, q.InactiveShare)
	} else if q.InactiveFiles > 0 {
		q.Status = "Coverage worth reviewing"
		q.StatusClass = "warn"
		q.CoverageLabel = "limited coverage"
		q.Summary = fmt.Sprintf("Analysis completed without parse errors. %d of %d walked files (%s) belonged to an extractor that did not activate.", q.InactiveFiles, q.FilesWalked, q.InactiveShare)
	} else if len(q.Potential) > 0 {
		q.Status = "Coverage worth reviewing"
		q.StatusClass = "warn"
		q.Summary = "Analysis succeeded, but potentially architectural file types are outside the current graph."
	}
	return q
}

// materialCoverageGap deliberately requires meaningful corpus impact before the
// dashboard uses red/error language. A handful of nested examples in a large
// monorepo is transparent but not alarming; a missing language population is.
func materialCoverageGap(files, walked int) bool {
	if files <= 0 {
		return false
	}
	return files >= 100 || walked > 0 && float64(files)/float64(walked) >= 0.05
}

func formatShare(part, total int) string {
	if part <= 0 || total <= 0 {
		return "unknown share"
	}
	pct := float64(part) / float64(total) * 100
	if pct < 0.1 {
		return "<0.1%"
	}
	return fmt.Sprintf("%.1f%%", pct)
}

func summarizeSnapshot(r *facts.Receipt, now time.Time) snapshotSummary {
	s := snapshotSummary{
		RepoName:    filepath.Base(r.RepoPath),
		ShortID:     shortDigest(r.SnapshotID, 12),
		Status:      "Snapshot ready",
		StatusClass: "on",
	}
	if generated, err := time.Parse(time.RFC3339, r.GeneratedAt); err == nil {
		s.Age = formatDuration(now.Sub(generated)) + " ago"
		s.Generated = generated.Local().Format("2006-01-02 15:04:05 MST")
	} else {
		s.Generated = r.GeneratedAt
	}
	if r.Git != nil {
		s.ShortCommit = shortDigest(r.Git.Commit, 8)
		if r.Git.Dirty {
			s.Status = "Captured with local changes"
			s.StatusClass = "warn"
		}
	}
	if r.Quality.ParseErrors > 0 {
		s.Status = "Analysis needs attention"
		s.StatusClass = "off"
	}
	return s
}

func shortDigest(value string, n int) string {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) > n {
		return value[:n]
	}
	return value
}

// currentReceipt returns the receipt to display for the current snapshot, or nil
// if none is available. It prefers the live in-memory receipt (fresh after a
// generate_snapshot), but falls back to the last-written receipt on disk when the
// in-memory one is missing or blank — the common case after a server restart,
// where AutoLoadSnapshot restores facts without full receipt metadata.
func (s *Server) currentReceipt() *facts.Receipt {
	if b, err := s.eng.GetArtifact("receipt.json"); err == nil {
		var rv facts.Receipt
		if err := json.Unmarshal(b, &rv); err == nil && rv.SnapshotID != "" {
			return &rv
		}
	}

	repo := s.eng.ActiveRepo()
	if repo == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(s.eng.OutputDir(repo), "receipt.json"))
	if err != nil {
		return nil
	}
	var rv facts.Receipt
	if err := json.Unmarshal(b, &rv); err != nil {
		return nil
	}
	return &rv
}

// currentInsights returns the insight list for the current snapshot, or nil. Like
// currentReceipt it prefers the live in-memory artifact (fresh after generate) but
// falls back to the last-written insights.json on disk — AutoLoadSnapshot restores
// facts without the snapshot's insights, so after a server restart the in-memory
// list is empty while a full one persists at <repo>/.enola/insights.json.
func (s *Server) currentInsights() []facts.Insight {
	if b, err := s.eng.GetArtifact("insights.json"); err == nil {
		var ins []facts.Insight
		if err := json.Unmarshal(b, &ins); err == nil && len(ins) > 0 {
			return ins
		}
	}

	repo := s.eng.ActiveRepo()
	if repo == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(s.eng.OutputDir(repo), "insights.json"))
	if err != nil {
		return nil
	}
	var ins []facts.Insight
	if err := json.Unmarshal(b, &ins); err != nil {
		return nil
	}
	return ins
}

// toolRows builds the sorted union of tool-usage rows (this server and lifetime).
func toolRows(total, session map[string]int) []toolRow {
	set := make(map[string]struct{}, len(total)+len(session))
	for k := range total {
		set[k] = struct{}{}
	}
	for k := range session {
		set[k] = struct{}{}
	}
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)

	rows := make([]toolRow, 0, len(names))
	for _, n := range names {
		rows = append(rows, toolRow{Name: n, Session: session[n], Total: total[n]})
	}
	return rows
}

// valueRows builds the per-tool value-estimate rows plus the total row, reusing
// the shared status value model so the numbers match --status exactly.
func valueRows(total, saved map[string]int) ([]valueRow, valueRow) {
	rep := status.ComputeValue(total, saved)
	rows := make([]valueRow, 0, len(rep.Tools))
	for _, tv := range rep.Tools {
		rows = append(rows, valueRow{
			Label:       tv.Tool,
			Calls:       tv.Calls,
			TimeSaved:   formatDuration(tv.TimeSaved),
			TokensSaved: humanCount(tv.TokensSaved),
		})
	}
	totalRow := valueRow{
		Label:       "TOTAL",
		Calls:       rep.TotalCalls,
		TimeSaved:   formatDuration(rep.TotalTimeSaved),
		TokensSaved: humanCount(rep.TotalTokensSaved),
	}
	return rows, totalRow
}
