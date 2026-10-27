package server

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/updatecheck"
	"github.com/enola-labs/enola/internal/version"
	"github.com/enola-labs/enola/pkg/plan"
	"github.com/enola-labs/enola/pkg/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolCallbackFunc is invoked once per completed tool call, with everything the
// value model needs that cannot be recovered afterwards: the outcome, the size of
// the response the agent had to read, and — for generate_snapshot — the corpus it
// indexed. Stored behind an atomic.Pointer so SetToolCallback and the per-call
// fire are race-free regardless of call ordering.
type toolCallbackFunc func(status.ToolCall)

// Server wraps the MCP server and connects it to the snapshot engine.
type Server struct {
	mcp *mcp.Server
	eng *engine.Engine
	cfg *config.Config

	// toolCallback is read on every tool-call goroutine via fireToolCallback and
	// written by SetToolCallback; the atomic pointer makes that race-free.
	toolCallback atomic.Pointer[toolCallbackFunc]

	// genMu serializes the entire generate_snapshot handler (the auto-append
	// heuristic's reads of the current snapshot, GenerateSnapshot, the
	// snapshotsGenerated flag, and the artifact/receipt writes). Two generates can
	// no longer interleave and corrupt session state. Read-only tools never take
	// genMu, so they stay concurrent and lock-free against the published bundle.
	genMu sync.Mutex

	// restored is closed once the startup restore has published its graph; nil when
	// none was started. Set before Run, read by restoreMiddleware. See
	// RestoreInBackground.
	restored chan struct{}

	// snapshotsGenerated records whether generate_snapshot has run at least once
	// in this session. It distinguishes a user-driven multi-repo session from a
	// store that was merely pre-populated by AutoLoadSnapshot at startup, so the
	// auto-append heuristic never fires on top of auto-loaded-only state. Guarded
	// by genMu (only read/written inside the locked generate_snapshot handler).
	snapshotsGenerated bool

	// corpusByRepo remembers each repo's measured corpus for the currently loaded
	// graph: a later append prices cross-repo edge resolution against it, and
	// every query is scaled and capped by its total.
	//
	// Writes happen only in the genMu-serialized snapshot handler, but reads now
	// come from the value middleware on every concurrent tool call. So the map is
	// published behind an atomic pointer and replaced wholesale — never mutated
	// in place, which would race a reader mid-iteration.
	corpusByRepo atomic.Pointer[map[string]int]

	// Freshness-banner cache. freshnessBanner() shells out to git per repo, so the
	// result is memoized for freshTTL to keep that cost off the hot path of
	// back-to-back read tools. Guarded by freshMu.
	freshMu     sync.Mutex
	freshBanner string
	freshAt     time.Time

	// reload restores the workspace's graph from disk and returns its corpus, the
	// same way startup does. Set by the bootstrap that knows how to restore; nil
	// means this server never reloads. reloadTriedID is the receipt snapshot id the
	// last reload attempted, so a receipt whose artifacts cannot be restored is
	// tried once and not every freshTTL.
	reload        func() map[string]int
	reloadMu      sync.Mutex
	reloadSeenMod time.Time
	reloadTriedID string

	// notices are one-time messages for the agent, attached to the next tool result
	// and then dropped: which graph a restore or a skipped reload left this session
	// serving. Guarded by noticeMu, since tool calls run concurrently.
	noticeMu sync.Mutex
	notices  []string

	planFactory plan.EngineFactory

	// updateSaid is set once the "a newer enola exists" notice has been attached to a
	// tool result, so it is said once per SERVER PROCESS and never again.
	//
	// Once-per-process is the whole cadence policy, and it needs no file and no
	// cross-process coordination to implement: a server process is a session, so the
	// agent hears it on its first tool call and is left alone for the rest of the work.
	// Repeating it on every result — which is what the freshness banner does, because
	// staleness is actionable RIGHT NOW and this is not — would make it wallpaper by
	// the third call and dilute the banner that does need attention.
	updateSaid atomic.Bool
}

// startHint is the instructions' first step. A workspace snapshotted before has its
// graph restored as the server starts, so the tools answer at once and a session that
// opens with generate_snapshot spends a turn re-taking what is already loaded.
func startHint(cfg *config.Config) string {
	if cfg == nil || !hasSavedGraph(cfg) {
		return "Start with generate_snapshot."
	}
	return "This workspace's last snapshot is loaded as the server starts, so the tools answer at once: " +
		"call generate_snapshot only to refresh it after edits or to load another repository."
}

// hasSavedGraph reports whether a graph for the workspace is on disk to be restored: a
// snapshot in its output directory, or in that of a repository its graph receipt
// names. The receipt alone is not enough: it outlives the snapshots it points at
// (a clean checkout, a deleted .enola), and then nothing is restored.
func hasSavedGraph(cfg *config.Config) bool {
	hasFacts := func(repo string) bool {
		_, err := os.Stat(filepath.Join(repo, cfg.Output.Dir, "facts.jsonl"))
		return err == nil
	}
	if repo, err := filepath.Abs(cfg.Repo); err == nil && hasFacts(repo) {
		return true
	}
	gr, err := engine.LoadWorkspaceReceipt(cfg.Repo)
	if err != nil {
		return false
	}
	for _, r := range gr.Repos {
		if r.Path != "" && hasFacts(r.Path) {
			return true
		}
	}
	return false
}

// New creates a new MCP server wired to the given engine.
func New(eng *engine.Engine, cfg *config.Config) (*Server, error) {
	s := &Server{
		eng: eng,
		cfg: cfg,
	}

	instructions := "enola reports what a change did to a codebase's STRUCTURE: it pins the architecture as a baseline and grades the delta against it. " +
		"Linters, type checkers and static analysis judge the current tree file by file against rules they brought with them and have no baseline, so they cannot separate a regression a change introduced from the hundreds already in the repository, and cannot see a cycle spanning several files, a layer crossed the wrong way, or coupling nobody asked for. They are complements, not alternatives. " +
		"IF ASKED whether a project that already has linters, tests or CI needs enola, do not answer from category: run generate_snapshot and query_insights and show what they report about THIS repository. " +
		"Use these tools, not a text search, for questions about structure: which packages or files depend on X (impact_analysis(target=X)), who calls X (traverse(start=X, direction=reverse)), how A reaches B (find_path), what nothing uses (find_orphans, query_insights(explainer='dead-code')), where a query runs once per loop iteration (query_insights(explainer='query-loops')). " +
		"A text search finds where a name is written and misses a package that reaches X through another, a call through an interface, and a function passed as a value. " +
		"If your client lists these tools as deferred (Claude Code: names only until loaded), and only then, load them before answering such a question, for example with ToolSearch select:mcp__enola__generate_snapshot,mcp__enola__impact_analysis,mcp__enola__traverse,mcp__enola__query_facts,mcp__enola__explore,mcp__enola__query_insights. " +
		startHint(cfg) + " Then use explore, query_facts, show_symbol, traverse, find_path and impact_analysis for structure and change impact; " +
		"query_insights for the findings explainers already computed (cycles, layer violations, unused routes, god-classes, hotspots and more) instead of re-deriving them; " +
		"governing_intent, when knowledge pages are compiled in (enola_intent), for which pages govern which code. " +
		"To verify a change: generate_snapshot, set_baseline, edit, generate_snapshot, diff_snapshot. diff_snapshot reports only what changed, never pre-existing state, so prefer it over re-reading files. " +
		"Backend routes no client calls need a multi-repo snapshot of the backend plus its clients, then query_insights(explainer='unused-routes') or query_facts(kind=route, prop=unmatched_by_clients, prop_value=true). " +
		"A route clients call only from client code that never runs (an API method no page reaches) carries unreached_by_clients instead."
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "enola",
		Version: version.Version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	s.mcp = mcpServer
	s.registerTools()

	// Prepend a freshness warning to tool results when the loaded graph looks stale.
	// Order matters: valueMiddleware is registered first so it runs OUTSIDE the
	// freshness one, and therefore measures the response the agent actually
	// receives — banner included. Both are registered here rather than per tool, so
	// they cover every tool on the server, including any added after New returns.
	s.mcp.AddReceivingMiddleware(s.valueMiddleware)
	s.mcp.AddReceivingMiddleware(s.freshnessMiddleware)
	s.mcp.AddReceivingMiddleware(s.updateMiddleware)
	// restoreMiddleware is registered last, which makes it outermost, so no tool, the
	// freshness reload included, runs before the startup restore has published.
	s.mcp.AddReceivingMiddleware(s.restoreMiddleware)

	return s, nil
}

// Run starts the MCP server on the stdio transport.
func (s *Server) Run(ctx context.Context) error {
	log.Println("[server] starting MCP server on stdio transport")

	// In a goroutine, and its result is never awaited: a server that took even a
	// moment to start because of an update check would be paying a latency cost on
	// every session for information that is only ever advisory. Whatever it finds
	// lands in the cache and is read by the NEXT tool call, or the next session — the
	// banner reads the file, never this. The hook covers the case where hooks are
	// installed; this covers everyone else, since an MCP-only user has no other
	// long-lived process that could refresh it.
	go updatecheck.Refresh(ctx)

	return s.mcp.Run(ctx, &mcp.StdioTransport{})
}

// SetToolCallback sets a callback invoked once per completed tool call. The
// callback receives the tool name, the absolute path of the repo the call
// operated on (the active snapshot's repo, or the target repo for
// generate_snapshot), whether the call succeeded, how large its response was,
// and any snapshot detail. It is safe to call before Run().
func (s *Server) SetToolCallback(cb func(status.ToolCall)) {
	fn := toolCallbackFunc(cb)
	s.toolCallback.Store(&fn)
}

func (s *Server) SetPlanEngineFactory(factory plan.EngineFactory) {
	s.planFactory = factory
}

// fireToolCallback invokes the tool callback if set. Safe to call concurrently
// from multiple tool-call goroutines.
func (s *Server) fireToolCallback(call status.ToolCall) {
	if cbp := s.toolCallback.Load(); cbp != nil {
		(*cbp)(call)
	}
}

// activeRepo returns the repo path the current snapshot is loaded for, falling
// back to the configured repo when nothing is loaded yet.
func (s *Server) activeRepo() string {
	if snap := s.eng.Snapshot(); snap != nil && snap.Meta.RepoPath != "" {
		return snap.Meta.RepoPath
	}
	return s.cfg.Repo
}

// MCPServer returns the underlying MCP server, so a caller can connect a transport
// to it or register an additional tool against the same server and engine.
func (s *Server) MCPServer() *mcp.Server {
	return s.mcp
}

// registerTools adds the MCP tools this package defines. Each family lives in its
// own tools_*.go file, with the argument types, handlers and renderers it needs.
func (s *Server) registerTools() {
	// The timeline tools, in their own file: everything else here answers about the tree
	// as it is now, and those answer about the past.
	s.registerHistoryTools()

	s.registerGenerateSnapshotTool()
	s.registerQueryFactsTool()
	s.registerShowSymbolTool()
	s.registerLocateTool()
	s.registerExploreTool()
	s.registerTraverseTool()
	s.registerFindPathTool()
	s.registerEndpointImpactTool()
	s.registerImpactAnalysisTool()
	s.registerGoverningIntentTool()
	s.registerConstraintsForTool()
	s.registerPlanCheckTool()
	s.registerCoverageReportTool()
	s.registerQueryInsightsTool()
	s.registerSetBaselineTool()
	s.registerDiffSnapshotTool()
	s.registerSnapshotReceiptTool()
	s.registerCompareReceiptsTool()
}
