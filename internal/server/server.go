package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/conformance"
	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/drift"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/explainers/constraints"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	httpsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/http"
	"github.com/enola-labs/enola/internal/metrics"
	"github.com/enola-labs/enola/internal/orphans"
	"github.com/enola-labs/enola/internal/perf"
	"github.com/enola-labs/enola/internal/updatecheck"
	"github.com/enola-labs/enola/internal/version"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/enola-labs/enola/pkg/coverage"
	"github.com/enola-labs/enola/pkg/mcputil"
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

// callRecord is the per-call scratchpad a handler uses to tell the value
// middleware what it did. The middleware puts one in the context before invoking
// the handler and reads it afterwards, so the common case — a read tool that only
// needs its name, repo and response size — costs the handler nothing.
//
// Only generate_snapshot enriches it, because only a snapshot's value depends on
// facts the middleware cannot see from the outside.
type callRecord struct {
	mu       sync.Mutex
	repo     string
	snapshot *status.SnapshotValue
}

type callRecordKey struct{}

// recordSnapshot attaches snapshot-value detail to the in-flight call, if the
// call is being observed. Safe to call when it is not.
func recordSnapshot(ctx context.Context, repo string, sv status.SnapshotValue) {
	rec, ok := ctx.Value(callRecordKey{}).(*callRecord)
	if !ok || rec == nil {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.repo = repo
	rec.snapshot = &sv
}

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

// New creates a new MCP server wired to the given engine.
func New(eng *engine.Engine, cfg *config.Config) (*Server, error) {
	s := &Server{
		eng: eng,
		cfg: cfg,
	}

	instructions := "enola reports what a change did to a codebase's STRUCTURE. That needs a \"before\", so it pins the architecture as a value and grades the delta against it. This is a different question from the one linters, type checkers and static analysis answer — whether the current tree breaks rules they brought with them, file by file. Those have no baseline, so they cannot separate a regression a change introduced from the hundreds already in the repository, and a dependency cycle spanning four files, a layer crossed the wrong way, or coupling nobody asked for is invisible to every one of them. They are complements, not alternatives. IF ASKED whether a project that already has linters, tests or CI needs enola, do not answer from category: run generate_snapshot and query_insights and show what they actually report about THIS repository. Use this server to explore a repository's architecture as queryable facts. Run generate_snapshot first to index a codebase, then use explore, query_facts, show_symbol, traverse, find_path, and impact_analysis to understand code structure, dependencies, and change impact. When knowledge pages compile into the snapshot (enola_intent), governing_intent answers the reverse query directly — which pages govern a file or fact, and which code a page governs. Explainers run automatically during generate_snapshot and compute findings (dependency cycles, layer violations, unused/dead routes, god-classes, hotspots, and more) — fetch them with query_insights rather than re-deriving them by hand. To find backend HTTP routes that no loaded client calls, take a multi-repo (append-mode) snapshot of the backend plus its clients, then call query_insights(explainer='unused-routes') or query_facts(kind=route, prop=unmatched_by_clients, prop_value=true). To verify what a change did to the architecture, pin a baseline before editing and diff after: generate_snapshot → set_baseline → make changes → generate_snapshot → diff_snapshot. diff_snapshot is delta-only (it reports just what changed — new/resolved findings, new coupling, added/removed symbols — never pre-existing state), so prefer it over re-reading files to confirm a change. Supports Go, TypeScript/JavaScript (incl. Vue, Svelte, Ember), Python, Java, Kotlin, Scala, Dart/Flutter, Ruby, PHP, Swift, Rust, C/C++, .NET (C#/VB.NET/F#/Razor/XAML), Terraform/HCL, Ansible, gRPC/Protobuf, OpenAPI, AsyncAPI, and GraphQL."
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

// valueMiddleware records every tool call AFTER its handler has run.
//
// Recording centrally, and after the handler, is what makes three things
// possible. The outcome is known, so a validation failure or a "run
// generate_snapshot first" is counted as a call but credited nothing. The
// response is in hand, so its size can be subtracted — which is what makes
// output_mode visible in the estimate. And because it is registered once on the
// shared MCP server, it covers every tool registered on it, including any added
// after New returns, which cannot then drift by forgetting to report themselves.
func (s *Server) valueMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
		if !ok {
			return next(ctx, method, req)
		}

		rec := &callRecord{}
		result, err := next(context.WithValue(ctx, callRecordKey{}, rec), method, req)

		call := status.ToolCall{
			Tool:         params.Name,
			Repo:         s.activeRepo(),
			OK:           err == nil,
			CorpusTokens: s.loadedCorpusTokens(),
		}
		if ctr, ok := result.(*mcp.CallToolResult); ok && ctr != nil {
			call.OK = call.OK && !ctr.IsError
			call.ResponseBytes = resultBytes(ctr)
		}
		rec.mu.Lock()
		if rec.repo != "" {
			call.Repo = rec.repo
		}
		call.Snapshot = rec.snapshot
		rec.mu.Unlock()

		s.fireToolCallback(call)
		return result, err
	}
}

// charsPerToken is the rough characters-per-token ratio used to convert source
// bytes into tokens, matching the engine's own heuristic elsewhere.
const charsPerToken = 4

// rememberCorpus records a repo's measured corpus in the published map, so that
// snapshotting a sibling can price cross-repo edge resolution against what is
// already loaded, and queries can be scaled by the whole graph. Copy-on-write:
// the map is rebuilt and swapped rather than updated in place. Caller holds genMu.
func (s *Server) rememberCorpus(repo string, tokens int) {
	next := make(map[string]int)
	if cur := s.corpusByRepo.Load(); cur != nil {
		for k, v := range *cur {
			next[k] = v
		}
	}
	next[repo] = tokens
	s.corpusByRepo.Store(&next)
}

// resetCorpus clears the pool, for a non-append snapshot that resets the store.
// Caller holds genMu.
func (s *Server) resetCorpus() {
	s.corpusByRepo.Store(nil)
}

// SeedCorpus publishes the corpus of a graph restored from disk, so queries are
// priced against it before this process has taken any snapshot of its own.
//
// Without it the pool is empty until the first generate_snapshot, and a restart
// silently un-scales every query — which is the normal path, since
// AutoLoadSnapshot exists precisely so queries work after a restart WITHOUT
// re-snapshotting. Call it before Run; a nil or empty map is ignored, and a
// later snapshot overwrites what it seeded.
func (s *Server) SeedCorpus(byRepo map[string]int) {
	if len(byRepo) == 0 {
		return
	}
	seeded := make(map[string]int, len(byRepo))
	for repo, tokens := range byRepo {
		if tokens > 0 {
			seeded[repo] = tokens
		}
	}
	if len(seeded) == 0 {
		return
	}
	s.corpusByRepo.Store(&seeded)
}

// corpusTokensExcept sums the published corpus, optionally skipping one repo.
// Pass "" to total the whole loaded graph.
func (s *Server) corpusTokensExcept(exclude string) int {
	cur := s.corpusByRepo.Load()
	if cur == nil {
		return 0
	}
	total := 0
	for repo, tokens := range *cur {
		if repo != exclude {
			total += tokens
		}
	}
	return total
}

// priorCorpusTokens is the combined corpus loaded before this call, excluding the
// repo about to be indexed. It is deliberately session-scoped: after a restart it
// starts empty, so the first snapshot of a session claims no cross-repo premium.
// Under-crediting a restart is the right way to be wrong here.
func (s *Server) priorCorpusTokens(exclude string) int {
	return s.corpusTokensExcept(exclude)
}

// loadedCorpusTokens is the size of the graph a query actually searched — every
// repo resident, since a query is not scoped to one of them.
func (s *Server) loadedCorpusTokens() int {
	return s.corpusTokensExcept("")
}

// previousSnapshot reads a repo's last snapshot id and file hashes from its own
// .enola directory. Returns zero values when there is no readable previous
// snapshot, which the caller treats as a first build.
func previousSnapshot(absRepo string) (string, map[string]string) {
	data, err := os.ReadFile(filepath.Join(absRepo, ".enola", "snapshot.meta.json"))
	if err != nil {
		return "", nil
	}
	var meta facts.SnapshotMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", nil
	}
	hashes := make(map[string]string, len(meta.FileHashes))
	for _, fh := range meta.FileHashes {
		hashes[fh.Path] = fh.Hash
	}
	return meta.SnapshotID, hashes
}

// changedFraction is the share of the current files whose content differs from
// the previous snapshot (added, removed or modified), in [0,1]. It returns -1
// when there is no previous snapshot to compare against — a first build, which
// the value model prices as re-deriving everything.
//
// Returning -1 rather than 1 for that case matters: a genuine zero (the files
// are identical even though the snapshot id moved, e.g. after a version bump)
// must earn confirmation credit, not a full rebuild's worth.
func changedFraction(prev map[string]string, current []facts.FileHash) float64 {
	if len(prev) == 0 || len(current) == 0 {
		return -1
	}
	changed := 0
	for _, fh := range current {
		if prev[fh.Path] != fh.Hash {
			changed++
		}
	}
	// Files that disappeared are work too: their edges have to be retired.
	for path := range prev {
		found := false
		for _, fh := range current {
			if fh.Path == path {
				found = true
				break
			}
		}
		if !found {
			changed++
		}
	}
	f := float64(changed) / float64(len(current))
	if f > 1 {
		return 1
	}
	return f
}

// resultBytes measures the text an agent has to read from a tool result. Non-text
// content is ignored rather than guessed at.
func resultBytes(ctr *mcp.CallToolResult) int {
	n := 0
	for _, c := range ctr.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			n += len(tc.Text)
		}
	}
	return n
}

// SetReloader installs the function that restores this workspace's graph from disk
// (bootstrap.AutoLoadSnapshot behind the import boundary). With it set, the server
// notices when a sibling process rewrote the artifacts under it, which is what a CLI
// `--generate` or `--generate --refresh` does while a server is running: the receipt
// on disk carries a snapshot id the served graph does not, and the served facts and
// insights are the ones from before. Checked at the freshness cadence, so a rewrite
// reaches the tools within freshTTL.
func (s *Server) SetReloader(reload func() map[string]int) {
	s.reload = reload
}

// reloadIfRewritten swaps in the on-disk graph when the workspace receipt names a
// snapshot other than the one being served. It runs before every read tool, so a
// rewrite is answered from on the very next call; the receipt is a small file and is
// only re-read when its modification time moved. Skipped while a generate holds
// genMu: that generate is about to publish a newer bundle and write the receipt
// itself.
func (s *Server) reloadIfRewritten() bool {
	if s.reload == nil || s.cfg == nil {
		return false
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	path, err := engine.WorkspaceReceiptPath(s.cfg.Repo)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info.ModTime().Equal(s.reloadSeenMod) {
		return false
	}
	s.reloadSeenMod = info.ModTime()
	gr, err := engine.LoadWorkspaceReceipt(s.cfg.Repo)
	if err != nil || gr.SnapshotID == "" || gr.SnapshotID == s.reloadTriedID {
		return false
	}
	if snap := s.eng.Snapshot(); snap != nil && snap.Meta.SnapshotID == gr.SnapshotID {
		return false
	}
	if !s.genMu.TryLock() {
		s.reloadSeenMod = time.Time{}
		return false
	}
	defer s.genMu.Unlock()
	s.reloadTriedID = gr.SnapshotID
	log.Printf("[server] artifacts on disk are snapshot %s, serving %s: reloading", shortID(gr.SnapshotID), shortID(s.loadedSnapshotID()))
	corpus := s.reload()
	if corpus != nil {
		s.SeedCorpus(corpus)
	}
	s.freshMu.Lock()
	s.freshAt = time.Time{}
	s.freshMu.Unlock()
	return true
}

func (s *Server) loadedSnapshotID() string {
	if snap := s.eng.Snapshot(); snap != nil {
		return snap.Meta.SnapshotID
	}
	return ""
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	if id == "" {
		return "(none)"
	}
	return id
}

// freshTTL bounds how often the freshness banner recomputes. The banner shells out
// to git per repo, so caching it for a short window keeps that cost off the hot
// path of back-to-back read tools without letting the warning go badly stale.
const freshTTL = 30 * time.Second

// bannerSuppressed lists the tools that must NOT get the freshness banner
// prepended: generate_snapshot (which just refreshed the graph) and set_baseline (a
// mutation, not a query). Every other tool result is fair game for the nudge.
func bannerSuppressed(tool string) bool {
	return tool == "generate_snapshot" || tool == "set_baseline"
}

// freshnessMiddleware prepends a staleness warning to successful tool-call results
// when the loaded graph looks out of date (older than 24h, or a repo's code moved
// since its snapshot). It is warn-only: it never blocks or regenerates. Registered
// once, it also covers the analyzers that share this MCP server.
func (s *Server) freshnessMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" {
			if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && !bannerSuppressed(params.Name) {
				s.reloadIfRewritten()
			}
		}
		result, err := next(ctx, method, req)
		if err != nil || method != "tools/call" {
			return result, err
		}
		params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
		if !ok || bannerSuppressed(params.Name) {
			return result, err
		}
		ctr, ok := result.(*mcp.CallToolResult)
		if !ok || ctr == nil || ctr.IsError {
			return result, err
		}
		banner := s.freshnessBanner()
		if banner == "" {
			return result, err
		}
		ctr.Content = append([]mcp.Content{&mcp.TextContent{Text: banner + "\n\n"}}, ctr.Content...)
		return result, err
	}
}

// freshnessBanner returns a one-line staleness warning for the loaded graph, or ""
// when it is fresh. Results are memoized for freshTTL because the underlying check
// shells out to git per repo.
func (s *Server) freshnessBanner() string {
	s.freshMu.Lock()
	defer s.freshMu.Unlock()
	if !s.freshAt.IsZero() && time.Since(s.freshAt) < freshTTL {
		return s.freshBanner
	}

	st := s.eng.Staleness(24*time.Hour, time.Now())
	banner := ""
	if st.Stale() {
		var parts []string
		if st.TooOld {
			parts = append(parts, fmt.Sprintf("graph is %s old", humanizeDuration(st.Age)))
		}
		for _, c := range st.Changed {
			parts = append(parts, fmt.Sprintf("%s (%s)", c.Label, c.Reason))
		}
		banner = "⚠️ Snapshot may be stale: " + strings.Join(parts, "; ") + ". Run generate_snapshot to refresh."
	}

	s.freshBanner = banner
	s.freshAt = time.Now()
	return banner
}

// updateMiddleware attaches a one-time notice that a newer enola exists.
//
// Kept separate from freshnessMiddleware rather than folded into it, because the two
// answer different questions on different clocks: freshness is about THIS graph being
// behind THIS tree and is worth repeating until fixed; this is about the binary being
// behind the release stream, is not actionable inside the session, and is said once.
// Sharing a code path would force one cadence onto both.
//
// It appends rather than prepends: the freshness warning is the one the agent may need
// to act on before trusting the result, so it stays at the top.
//
// Everything it reads is local — updatecheck.AgentLine reads a cached file — so this
// adds no latency and no network call to a tool call. See internal/updatecheck.
func (s *Server) updateMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" {
			if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && !bannerSuppressed(params.Name) {
				s.reloadIfRewritten()
			}
		}
		result, err := next(ctx, method, req)
		if err != nil || method != "tools/call" {
			return result, err
		}
		params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
		if !ok || bannerSuppressed(params.Name) {
			return result, err
		}
		ctr, ok := result.(*mcp.CallToolResult)
		if !ok || ctr == nil || ctr.IsError {
			return result, err
		}
		notice := updatecheck.AgentLine(engine.ExtractorVersion())
		if notice == "" {
			return result, err
		}
		// Claimed with CompareAndSwap, not a check-then-set: tool calls are dispatched
		// concurrently by the MCP SDK, and two arriving together would otherwise both
		// see false and both append.
		if !s.updateSaid.CompareAndSwap(false, true) {
			return result, err
		}
		ctr.Content = append(ctr.Content, &mcp.TextContent{Text: "\n\n" + notice})
		return result, err
	}
}

// humanizeDuration renders a coarse, human-friendly age (minutes/hours/days).
func humanizeDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
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

// generateSnapshotArgs are the arguments for the generate_snapshot tool.
type generateSnapshotArgs struct {
	RepoPath  string `json:"repo_path" jsonschema:"Path to the repository to analyze. Defaults to the configured repo path."`
	Append    bool   `json:"append,omitempty" jsonschema:"If true, keep existing facts and add new ones with repo-prefixed file paths (for multi-repo analysis). Default false."`
	Fresh     bool   `json:"fresh,omitempty" jsonschema:"Force a clean SINGLE-repo snapshot: reset the store (discard any previously loaded repos) and index only repo_path, bypassing the auto-append heuristic. Use when you've moved to a different project and do NOT want it merged into an existing multi-repo store. Mutually exclusive with append."`
	NoCluster bool   `json:"no_cluster,omitempty" jsonschema:"When repo_path is a folder holding several git repositories, index it as ONE repository instead of as a cluster. Default false."`
}

// queryFactsArgs are the arguments for the query_facts tool.
type queryFactsArgs struct {
	Kind      string `json:"kind,omitempty" jsonschema:"Filter by fact kind: module, symbol, route, storage, dependency, or service (service = a whole repo, used as a node in the cross-repo graph)"`
	File      string `json:"file,omitempty" jsonschema:"Filter by file path"`
	Name      string `json:"name,omitempty" jsonschema:"Filter by name using substring match"`
	Relation  string `json:"relation,omitempty" jsonschema:"Filter by relation kind: declares, imports, calls, implements, or depends_on"`
	Prop      string `json:"prop,omitempty" jsonschema:"Filter by property name (e.g. source, symbol_kind, exported, framework, storage_kind, role, method, unmatched_by_clients). output_mode=summary surfaces notable boolean flags (like unmatched_by_clients) present in the result set."`
	PropValue string `json:"prop_value,omitempty" jsonschema:"Filter by property value (requires prop to be set)"`

	// Batch filters — OR within dimension, AND across dimensions
	Names      []string `json:"names,omitempty" jsonschema:"Filter by multiple exact names (OR). Use instead of name for batch lookups."`
	Files      []string `json:"files,omitempty" jsonschema:"Filter by multiple file paths (OR). Use instead of file for batch lookups."`
	Kinds      []string `json:"kinds,omitempty" jsonschema:"Filter by multiple kinds (OR). Use instead of kind for batch lookups."`
	FilePrefix string   `json:"file_prefix,omitempty" jsonschema:"Filter by file path prefix (e.g. internal/server to match all files in that directory)"`
	Repo       string   `json:"repo,omitempty" jsonschema:"Filter by repository label (set in multi-repo/append mode, e.g. 'go-service')"`

	// Pagination
	Offset int `json:"offset,omitempty" jsonschema:"Number of results to skip for pagination. Default 0."`
	Limit  int `json:"limit,omitempty" jsonschema:"Maximum number of results to return (1-500). Default 100."`

	// Relation expansion
	IncludeRelated bool `json:"include_related,omitempty" jsonschema:"If true, inline the full fact data for each relation target instead of just the target name"`

	// Output format
	OutputMode string `json:"output_mode,omitempty" jsonschema:"Output format: 'full' (DEFAULT, JSON facts), 'compact' (markdown table), 'names' (just names+files), or 'summary' (counts only: total + breakdown by kind and top files — cheapest, use to size a result set before fetching it)."`
	MaxTokens  int    `json:"max_tokens,omitempty" jsonschema:"Optional hard cap on output size (approx tokens). Output is truncated with a notice. Default: no cap."`
}

// enrichedFact wraps a Fact with resolved relation targets.
type enrichedFact struct {
	facts.Fact
	RelatedFacts []facts.Fact `json:"related_facts,omitempty"`
}

// queryResponse is the structured response for query_facts when advanced features are used.
type queryResponse struct {
	Facts   any  `json:"facts"`
	Total   int  `json:"total"`
	Offset  int  `json:"offset"`
	Limit   int  `json:"limit"`
	HasMore bool `json:"has_more"`
}

// renderCompact formats facts as a markdown table for minimal token usage.
func renderCompact(results []facts.Fact, total int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d results (showing %d):\n\n", total, len(results)))
	sb.WriteString("| Kind | Name | File | Line |\n")
	sb.WriteString("|------|------|------|------|\n")
	for _, f := range results {
		sb.WriteString(fmt.Sprintf("| %s | %s | %s | %d |\n", f.Kind, f.Name, f.File, f.Line))
	}
	return sb.String()
}

// renderNamesOnly returns just names and files, one per line.
func renderNamesOnly(results []facts.Fact, total int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d results (showing %d):\n\n", total, len(results)))
	for _, f := range results {
		sb.WriteString(fmt.Sprintf("%s  %s:%d\n", f.Name, f.File, f.Line))
	}
	return sb.String()
}

// renderQuerySummary returns counts only — total plus a breakdown by kind and the
// top files — so the caller can size a result set before fetching the facts
// themselves. The breakdown is computed over the returned sample (results); when
// total exceeds the sample it is annotated as approximate.
// notableBoolProps are high-signal boolean fact properties surfaced in the
// query_facts summary so a caller sizing a result set discovers actionable
// flags (e.g. dead routes) without already knowing the prop name exists.
var notableBoolProps = []string{"unmatched_by_clients"}

func renderQuerySummary(results []facts.Fact, total int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found **%d** matching facts.\n\n", total)

	byKind := map[string]int{}
	byFile := map[string]int{}
	flagCounts := map[string]int{}
	for _, f := range results {
		byKind[f.Kind]++
		if f.File != "" {
			byFile[f.File]++
		}
		for _, p := range notableBoolProps {
			if f.Props != nil && f.Props[p] == true {
				flagCounts[p]++
			}
		}
	}

	if len(byKind) > 0 {
		sb.WriteString("## By kind\n\n")
		for _, k := range topCounts(byKind, len(byKind)) {
			fmt.Fprintf(&sb, "- %s: %d\n", k, byKind[k])
		}
		sb.WriteString("\n")
	}
	if len(flagCounts) > 0 {
		sb.WriteString("## Flags\n\n")
		for _, p := range topCounts(flagCounts, len(flagCounts)) {
			fmt.Fprintf(&sb, "- %s=true: %d — list with query_facts(prop=%q, prop_value=true); see the summarized finding via query_insights\n", p, flagCounts[p], p)
		}
		sb.WriteString("\n")
	}
	if len(byFile) > 0 {
		sb.WriteString("## Top files\n\n")
		for _, f := range topCounts(byFile, 10) {
			fmt.Fprintf(&sb, "- %s: %d\n", f, byFile[f])
		}
		sb.WriteString("\n")
	}
	if total > len(results) {
		fmt.Fprintf(&sb, "_Breakdown computed over a sample of %d of %d matches; counts are approximate. Re-run with filters to narrow, or output_mode=compact/names to list facts._\n", len(results), total)
	}
	return sb.String()
}

// filterInsights returns the insights matching all of the supplied filters.
// explainer is matched case-insensitively against Insight.Source; repo matches
// the repo-prefix path segment of each insight's evidence files (insights have
// no structured repo field) — see insightBelongsToRepo; minConfidence keeps
// insights at or above the bar. multiRepo reports whether the snapshot spans
// more than one repo, which selects strict vs. legacy repo matching.
func filterInsights(insights []facts.Insight, explainer, repo string, minConfidence float64, multiRepo bool) []facts.Insight {
	repoLC := strings.ToLower(strings.TrimSpace(repo))
	var out []facts.Insight
	for _, in := range insights {
		if explainer != "" && !strings.EqualFold(in.Source, strings.TrimSpace(explainer)) {
			continue
		}
		if in.Confidence < minConfidence {
			continue
		}
		if repoLC != "" && !insightBelongsToRepo(in, repoLC, multiRepo) {
			continue
		}
		out = append(out, in)
	}
	return out
}

// crossRepoExplainer reports whether name identifies an explainer whose findings
// depend on KindService nodes — the cross-repo "graph of graphs" the linker builds
// only for a multi-repo (append-mode) snapshot. On a single-repo snapshot these
// explainers return nothing not because the code is clean but because they could
// not run; the response must say which. Keep in sync with crossrepo.ComputeLinks,
// which returns nil below two repos.
func crossRepoExplainer(name string) bool {
	switch name {
	case "unused-routes", "coverage", "crossrepo":
		return true
	}
	return false
}

// producingExplainers returns the sorted, de-duplicated set of explainer names
// that actually produced at least one insight in snap, read from the Insight.Source
// the engine stamps. Unlike SnapshotMeta.Explainers — the ran-without-error set,
// which includes explainers gated out to zero insights — this lists only the
// genuine sources, so it never implies an insight came from an explainer that
// produced none.
func producingExplainers(snap *facts.Snapshot) []string {
	seen := map[string]struct{}{}
	for _, in := range snap.Insights {
		if in.Source != "" {
			seen[in.Source] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// noMatchInsightsMessage builds the query_insights response for the case where no
// insight matched the filter. It separates three situations the old single string
// conflated: nothing was produced at all; a cross-repo explainer could not run
// because the snapshot is single-repo (no KindService nodes); and a genuine
// no-match. The middle wording mirrors coverage_report's, so the two surfaces read
// the same for the same underlying cause.
func noMatchInsightsMessage(explainer, repo string, minConfidence float64, snap *facts.Snapshot, store *facts.Store) string {
	if len(snap.Insights) == 0 {
		return "No insights were produced for this snapshot."
	}
	if crossRepoExplainer(explainer) && len(store.ByKind(facts.KindService)) == 0 {
		return fmt.Sprintf(
			"Explainer %q did not run: it needs a multi-repo (append-mode) snapshot. "+
				"Snapshot the backend, then each client with append=true.", explainer)
	}
	return fmt.Sprintf(
		"No insights matched (explainer=%q, repo=%q, min_confidence=%.2f). The snapshot has %d insight(s) produced by: %v. "+
			"Call query_insights without filters to list them.",
		explainer, repo, minConfidence, len(snap.Insights), producingExplainers(snap))
}

// singleRepoServiceHint returns an append-mode hint (and true) when a
// query_facts(kind=service) call returned nothing because the snapshot is
// single-repo — service nodes exist only in multi-repo (append-mode) snapshots, so
// a bare marshalled null here reads like an absence of edges rather than an absence
// of the whole cross-repo graph. Mirrors coverage_report's wording.
func singleRepoServiceHint(kind string, resultCount int, store *facts.Store) (string, bool) {
	if kind == facts.KindService && resultCount == 0 && len(store.RepoLabels()) <= 1 {
		return "No service nodes found: query_facts(kind=\"service\") needs a multi-repo (append-mode) snapshot. " +
			"Snapshot the backend, then each client with append=true.", true
	}
	return "", false
}

// pathInRepo reports whether a repo-prefixed evidence path (e.g.
// "golf/internal/x.go") belongs to repo (given lowercased). Matching is on the
// first path segment, so "golf" does not match "golf-ui/..." or
// "my-golf-journal-*".
func pathInRepo(path, repoLC string) bool {
	p := strings.ToLower(strings.TrimSpace(path))
	return p == repoLC || strings.HasPrefix(p, repoLC+"/")
}

// insightBelongsToRepo reports whether an insight is about repo (given
// lowercased). In multi-repo snapshots evidence paths are repo-prefixed, so we
// match the path-segment of each evidence File/Fact exactly. The title
// substring match is dropped there because titles aren't reliably repo-qualified
// and over-match shared tokens (e.g. "golf" in "golf-ui"). Single-repo snapshots
// don't prefix evidence paths, so there we keep the legacy substring heuristic —
// it can't leak across repos because there are no siblings.
func insightBelongsToRepo(in facts.Insight, repoLC string, multiRepo bool) bool {
	for _, ev := range in.Evidence {
		if pathInRepo(ev.File, repoLC) || pathInRepo(ev.Fact, repoLC) {
			return true
		}
	}
	if multiRepo {
		return false
	}
	// Single-repo legacy fallback (unchanged behavior).
	if strings.Contains(strings.ToLower(in.Title), repoLC) {
		return true
	}
	for _, ev := range in.Evidence {
		if strings.Contains(strings.ToLower(ev.File), repoLC) ||
			strings.Contains(strings.ToLower(ev.Fact), repoLC) {
			return true
		}
	}
	return false
}

// renderInsightsSummary lists one row per insight (explainer, confidence, title)
// with a by-explainer tally — the cheapest way to size and triage findings.
func renderInsightsSummary(insights []facts.Insight) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found **%d** insight(s).\n\n", len(insights))

	bySource := map[string]int{}
	for _, in := range insights {
		bySource[insightSource(in)]++
	}
	if len(bySource) > 0 {
		sb.WriteString("## By explainer\n\n")
		for _, s := range topCounts(bySource, len(bySource)) {
			fmt.Fprintf(&sb, "- %s: %d\n", s, bySource[s])
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Insights\n\n")
	sb.WriteString("| Explainer | Confidence | Title |\n|---|---|---|\n")
	for _, in := range insights {
		fmt.Fprintf(&sb, "| %s | %.2f | %s |\n", insightSource(in), in.Confidence, oneLine(in.Title))
	}
	sb.WriteString("\n_Use output_mode='compact' for descriptions, evidence, and suggested actions, or 'full' for complete JSON._\n")
	return sb.String()
}

// renderInsightsCompact renders each insight with its description, an evidence
// sample (capped), and suggested actions.
func renderInsightsCompact(insights []facts.Insight) string {
	const evidenceSample = 10
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found **%d** insight(s).\n\n", len(insights))
	for i, in := range insights {
		fmt.Fprintf(&sb, "### %d. %s\n", i+1, in.Title)
		fmt.Fprintf(&sb, "- explainer: %s · confidence: %.2f\n", insightSource(in), in.Confidence)
		if in.Description != "" {
			fmt.Fprintf(&sb, "- %s\n", in.Description)
		}
		if len(in.Evidence) > 0 {
			fmt.Fprintf(&sb, "- evidence (%d):\n", len(in.Evidence))
			shown := len(in.Evidence)
			if shown > evidenceSample {
				shown = evidenceSample
			}
			for _, ev := range in.Evidence[:shown] {
				fmt.Fprintf(&sb, "    - %s\n", formatEvidence(ev))
			}
			if len(in.Evidence) > shown {
				fmt.Fprintf(&sb, "    - … and %d more (output_mode='full' for all)\n", len(in.Evidence)-shown)
			}
		}
		if len(in.Actions) > 0 {
			sb.WriteString("- suggested actions:\n")
			for _, a := range in.Actions {
				fmt.Fprintf(&sb, "    - %s\n", a)
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// insightSource returns the producing explainer name, or a placeholder when unset.
func insightSource(in facts.Insight) string {
	if in.Source == "" {
		return "—"
	}
	return in.Source
}

// formatEvidence joins an evidence record's non-empty fields into one line.
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

// oneLine collapses newlines and escapes pipes so a string is safe inside a
// single markdown table cell.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", "\\|")
}

// registerTools adds MCP tools for snapshot generation and fact querying.
// explainerHints annotates the explainer names whose purpose is not obvious from the
// name alone. Names without an entry are rendered bare.
var explainerHints = map[string]string{
	"unused-routes":      "dead/uncalled HTTP routes",
	"messaging-coverage": "undeclared messaging calls and AsyncAPI operations with no detected implementation",
	"intent":             "declared architecture verdicted against the measured graph",
	"constraints":        "declared component rules, and the edges that breach them",
	"domain":             "what a framework's declarations say about its data and API",
	"query-loops":        "a database query issued once per loop iteration",
	"entry-points":       "the symbols a framework invokes directly",
}

// explainerFilterList renders the explainer= vocabulary for the query_insights tool
// description, in the order the engine runs them.
//
// Derived from config.KnownExplainers rather than written out, because the written-out
// version had gone stale: it named eleven explainers for as long as sixteen had shipped,
// so five of them — intent, constraints, domain, query-loops and entry-points — were
// invisible to any agent reading the tool description to decide what it could ask for.
// The findings were there; nothing told the caller the filter would accept the name.
func explainerFilterList() string {
	parts := make([]string, 0, len(config.KnownExplainers))
	for _, name := range config.KnownExplainers {
		if hint := explainerHints[name]; hint != "" {
			parts = append(parts, name+" ("+hint+")")
			continue
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, ", ")
}

func (s *Server) registerTools() {
	// The timeline tools, in their own file: everything else here answers about the tree
	// as it is now, and those answer about the past.
	s.registerHistoryTools()

	// The three analyzers that own a tool as well as an explainer. They register on
	// THIS server, so internal/server's value middleware observes their calls like
	// any other tool's and prices them once; a plugin reporting its own calls could
	// forget to, or remember twice. Each takes the accessor rather than a store,
	// because a handler must read whatever the latest snapshot published.
	metrics.Register(s.mcp, s.eng.Store)
	orphans.Register(s.mcp, s.eng.Store)
	perf.Register(s.mcp, s.eng.Store)

	// Tool: generate_snapshot
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "generate_snapshot",
		Description: "Index a repository and extract its architecture as queryable facts. " +
			"Supports Go, TypeScript/JavaScript (incl. Vue, Svelte, Ember), Python, Java, Kotlin, Scala, Dart/Flutter, Ruby, PHP, Swift, Rust, C/C++, .NET (C#/VB.NET/F#/Razor/XAML), Terraform/HCL, Ansible, gRPC/Protobuf, OpenAPI, AsyncAPI, and GraphQL. " +
			"Produces facts of kind: module, symbol, route, storage, dependency, service. " +
			"Run this first before any other tool. Re-run after code changes. " +
			"To VERIFY a change you are about to make, call set_baseline right after this first snapshot (BEFORE editing); " +
			"then after editing, re-run generate_snapshot and call diff_snapshot to see exactly what the change did to the architecture. " +
			"A repo_path that is a folder holding several git repositories is indexed as a cluster in one call (each repository a service, the calls between them linked); pass no_cluster=true to index it as one repository. " +
			"In multi-repo mode, call with append=true for each additional repo after the first; " +
			"enola auto-enables append when it detects you have switched to a different repo. " +
			"If you have instead moved to a DIFFERENT project and want a clean single-repo snapshot (not merged into the current store), pass fresh=true to reset.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args generateSnapshotArgs) (*mcp.CallToolResult, any, error) {
		repoPath := args.RepoPath
		if repoPath == "" {
			repoPath = s.cfg.Repo
		}

		absRepo, err := filepath.Abs(repoPath)
		if err != nil {
			return errorResult(fmt.Sprintf("invalid repo path: %v", err)), nil, nil
		}

		if args.Fresh && args.Append {
			return errorResult("fresh and append are mutually exclusive: fresh forces a single-repo reset; append adds to the existing store. Pick one."), nil, nil
		}

		// Serialize the whole write side against any other generate_snapshot: the
		// auto-append heuristic reads the current snapshot/store, then GenerateSnapshot
		// rebuilds and publishes, then snapshotsGenerated/WriteArtifacts/WriteGlobalReceipt
		// run — all of which must be atomic w.r.t. a second concurrent generate. Held
		// for the rest of the handler. Read-only tools do not take genMu.
		s.genMu.Lock()
		defer s.genMu.Unlock()

		// A folder of git repositories is indexed as a cluster, as --generate does. An
		// explicit append names one repository to add, so it is left alone.
		if !args.Append && !args.NoCluster {
			if repos := workspace.Clusterable(absRepo, ""); len(repos) > 0 {
				return s.generateCluster(ctx, absRepo, repos)
			}
		}

		// Auto-enable append mode when switching to a different repo while facts
		// from another repo are already loaded — but only once this session has
		// explicitly generated a snapshot. A store pre-populated solely by
		// AutoLoadSnapshot must not trigger append: an explicit/default
		// append=false resets and discards the auto-loaded state.
		//
		// fresh=true is the explicit escape hatch: it suppresses this heuristic so
		// the engine's non-append path clears the store and indexes only repo_path —
		// the way to force a single-repo snapshot when you've switched projects and
		// do NOT want the auto-append merge.
		appendMode := args.Append
		autoAppended := false
		autoAppendedFrom := ""
		if !args.Fresh && !appendMode && s.snapshotsGenerated && s.eng.Store().Count() > 0 && s.eng.Snapshot() != nil {
			prevRepo := s.eng.Snapshot().Meta.RepoPath
			if prevRepo != "" && prevRepo != absRepo {
				appendMode = true
				autoAppended = true
				autoAppendedFrom = prevRepo
				log.Printf("[server] auto-enabled append mode: switching from %s to %s", prevRepo, absRepo)
			}
		}

		// A fresh (non-append) snapshot that discards an auto-loaded store is
		// silent otherwise; log it so the reset is visible.
		if !appendMode && !s.snapshotsGenerated && s.eng.Store().Count() > 0 && s.eng.Snapshot() != nil {
			if prevRepo := s.eng.Snapshot().Meta.RepoPath; prevRepo != "" && prevRepo != absRepo {
				log.Printf("[server] discarding auto-loaded snapshot from %s; generating fresh single-repo snapshot for %s", prevRepo, absRepo)
			}
		}

		// A non-append snapshot resets the store, so the cross-repo corpus pool must
		// reset with it. Without this, the premium for joining a graph is charged
		// against repos that were discarded when the store was cleared — a repo
		// appended to a small cluster would be credited for edges against every
		// unrelated repo snapshotted earlier in the session.
		if !appendMode {
			s.resetCorpus()
		}

		// Read this repo's previous snapshot from disk before overwriting it, so the
		// value model can tell a first build from a refresh, and an unchanged
		// refresh from one that has real work to re-derive. Disk rather than the
		// in-memory snapshot because in append mode the loaded snapshot describes
		// whichever repo was indexed last, not this one.
		snapshot, err := s.indexRepo(ctx, absRepo, appendMode)
		if err != nil {
			return errorResult(fmt.Sprintf("snapshot generation failed: %v", err)), nil, nil
		}

		// Write artifacts to disk
		if err := s.eng.WriteArtifacts(absRepo); err != nil {
			log.Printf("[server] warning: failed to write artifacts: %v", err)
		}

		// Refresh the graph-wide receipt at ~/.enola/receipt.json (non-fatal).
		if err := s.eng.WriteGlobalReceipt(); err != nil {
			log.Printf("[server] warning: failed to write global receipt: %v", err)
		}

		summary := s.snapshotSummary(snapshot)

		if appendMode {
			autoNote := ""
			if autoAppended {
				autoNote = " (auto-enabled: different repo detected)"
			}
			summary += s.multiRepoSummary(snapshot, autoNote)
		} else {
			// Single-repo edit-verify loop guidance, tailored to whether a baseline
			// is already pinned: nudge set_baseline before the agent edits, then
			// diff_snapshot once a baseline exists. Skipped in multi-repo mode where
			// the baseline concept is per-first-repo and would only add noise.
			summary += s.loopHint(absRepo)
		}

		// De-silence the auto-append: lead with a prominent, self-correcting warning
		// so an agent that only meant a single-repo snapshot sees the merge and the
		// exact remedy, rather than silently ending up with a polluted store.
		if autoAppended {
			summary = fmt.Sprintf(
				"⚠️ **Auto-appended to the existing multi-repo store.** You snapshotted %q while %q was loaded, without append=true, so enola merged them into one store. "+
					"If you meant a FRESH single-repo snapshot of %q, re-run with fresh=true.\n\n---\n\n",
				filepath.Base(absRepo), filepath.Base(autoAppendedFrom), filepath.Base(absRepo),
			) + summary
		}

		// A folder holding several git repositories was just indexed as one repository.
		// Lead with that and with how to snapshot them as a cluster, so an agent does not
		// read the missing cross-repo graph as a fact about the code.
		if !appendMode {
			if folded := workspace.Folded(absRepo, ""); len(folded) > 0 {
				summary = fmt.Sprintf(
					"⚠️ **%s holds %d git repositories (%s), indexed here as ONE repository.** It has no service nodes and no edges between them. "+
						"To analyse them as a cluster, call generate_snapshot once per repository (repo_path=%s), passing append=true for every one after the first.\n\n---\n\n",
					absRepo, len(folded), workspace.Names(folded), filepath.Join(absRepo, "<repo>"),
				) + summary
			}
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: summary},
			},
		}, nil, nil
	})

	// Tool: query_facts
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "query_facts",
		Description: "Precision filter over extracted facts. Use after explore when you need specific subsets — " +
			"e.g. all symbols in a file, all external dependencies, all routes. " +
			"Fact kinds: module, symbol, route, storage, dependency, service. " +
			"name= is a substring match; names= is exact (batch). files= and kinds= are OR filters; combined with other fields they are AND. " +
			"output_mode: 'full' (default JSON) → 'compact' (markdown table) → 'names' (names+files) → 'summary' (counts only). " +
			"Use output_mode='summary' first to size an unfamiliar result set, then 'compact'/'names' to save tokens on large sets, and pass max_tokens to hard-cap output. " +
			"For dependencies, set prop='source' prop_value='internal'|'external'|'stdlib'|'framework' to filter noise. " +
			"Supports pagination via offset/limit (default 100, max 500).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args queryFactsArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		// Normalize absolute filesystem paths to store-relative paths.
		normFile := s.normalizeToRelative(args.File)
		normPrefix := s.normalizeToRelative(args.FilePrefix)
		var normFiles []string
		for _, f := range args.Files {
			normFiles = append(normFiles, s.normalizeToRelative(f))
		}

		// In multi-repo mode, expand the file prefix to include repo labels
		// if the user provided a bare relative path (e.g. "src/" instead of "golf-ui/src/").
		prefixes := s.expandFilePrefix(normPrefix)

		mode := resolveOutputMode(args.OutputMode, modeFull)

		// Summary mode aggregates over as many matches as the store allows (cap 500)
		// so the by-kind/by-file breakdown reflects the widest available sample.
		limit := args.Limit
		if mode == modeSummary {
			limit = 500
		}

		// Query with the first (or only) prefix.
		opts := facts.QueryOpts{
			Kind:       args.Kind,
			Kinds:      args.Kinds,
			File:       normFile,
			Files:      normFiles,
			FilePrefix: prefixes[0],
			Name:       args.Name,
			Names:      args.Names,
			Repo:       args.Repo,
			RelKind:    args.Relation,
			Prop:       args.Prop,
			PropValue:  args.PropValue,
			Offset:     args.Offset,
			Limit:      limit,
		}

		results, total := store.QueryAdvanced(opts)

		// If multiple repo labels matched, merge results from additional prefixes.
		for _, p := range prefixes[1:] {
			opts.FilePrefix = p
			extra, extraTotal := store.QueryAdvanced(opts)
			results = append(results, extra...)
			total += extraTotal
		}

		// A query_facts(kind=service) that comes back empty on a single-repo snapshot
		// means the cross-repo graph was never built, not that this service is a leaf.
		// Say so, mirroring coverage_report, instead of returning a bare null.
		if hint, ok := singleRepoServiceHint(args.Kind, len(results), store); ok {
			return textResult(hint), nil, nil
		}

		// Non-JSON output modes: return text instead of JSON.
		switch mode {
		case modeSummary:
			return textResult(capTokens(renderQuerySummary(results, total), args.MaxTokens, false)), nil, nil
		case modeCompact:
			return textResult(capTokens(renderCompact(results, total), args.MaxTokens, false)), nil, nil
		case modeNames:
			return textResult(capTokens(renderNamesOnly(results, total), args.MaxTokens, false)), nil, nil
		}

		// Determine if advanced features are in use (triggers structured response)
		useAdvanced := args.IncludeRelated || args.Offset > 0 || args.Limit > 0 ||
			len(args.Names) > 0 || len(args.Files) > 0 || len(args.Kinds) > 0 ||
			args.FilePrefix != "" || args.Repo != ""

		// Enrich with related facts if requested
		var output any
		if args.IncludeRelated {
			enriched := make([]enrichedFact, len(results))
			seen := make(map[string]struct{}) // deduplicate related facts
			for i, f := range results {
				enriched[i] = enrichedFact{Fact: f}
				for _, rel := range f.Relations {
					if _, dup := seen[rel.Target]; dup {
						continue
					}
					seen[rel.Target] = struct{}{}
					related := store.LookupByExactName(rel.Target)
					enriched[i].RelatedFacts = append(enriched[i].RelatedFacts, related...)
				}
			}
			output = enriched
		} else {
			output = results
		}

		if useAdvanced {
			limit := args.Limit
			if limit <= 0 {
				limit = 100
			}
			if limit > 500 {
				limit = 500
			}
			resp := queryResponse{
				Facts:   output,
				Total:   total,
				Offset:  args.Offset,
				Limit:   limit,
				HasMore: total > args.Offset+len(results),
			}
			return jsonResultCapped(resp, args.MaxTokens)
		}

		// Legacy format: raw JSON array (backwards compatible)
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return errorResult(fmt.Sprintf("failed to marshal results: %v", err)), nil, nil
		}

		text := string(data)
		if total > len(results) {
			text += fmt.Sprintf("\n\n... (showing %d of %d results, refine your query or use offset/limit for pagination)", len(results), total)
		}

		return textResult(capTokens(text, args.MaxTokens, true)), nil, nil
	})

	// Tool: show_symbol
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "show_symbol",
		Description: "Return the source code implementation of a named symbol. " +
			"Prefers exact name match; falls back to substring match and returns up to 5 results. " +
			"Default context: 60 lines (asymmetric: ~15 before declaration, ~45 after). " +
			"Use context_lines to widen or narrow the window. " +
			"Works in both single-repo and multi-repo (append) mode.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args showSymbolArgs) (*mcp.CallToolResult, any, error) {
		snapshot := s.eng.Snapshot()
		if snapshot == nil {
			return errorResult("No snapshot available. Run generate_snapshot first."), nil, nil
		}

		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		if args.Name == "" {
			return errorResult("name is required"), nil, nil
		}

		// Prefer exact match to avoid substring noise (e.g. "Transaction" matching "AutoTransactionsTogglePatch").
		results := store.LookupByExactName(args.Name)
		// Filter to symbols only
		symbolResults := results[:0]
		for _, r := range results {
			if r.Kind == facts.KindSymbol {
				symbolResults = append(symbolResults, r)
			}
		}
		results = symbolResults
		if len(results) == 0 {
			results = store.Query("symbol", "", args.Name, "")
		}
		if len(results) == 0 {
			return errorResult(fmt.Sprintf("No symbols matching %q", args.Name)), nil, nil
		}

		contextLines := args.ContextLines
		if contextLines <= 0 {
			contextLines = 60
		}

		// Limit to 5 results
		if len(results) > 5 {
			results = results[:5]
		}

		var sb strings.Builder

		for i, fact := range results {
			if i > 0 {
				sb.WriteString("\n---\n\n")
			}

			// Header
			sb.WriteString(fmt.Sprintf("### %s\n", fact.Name))
			sb.WriteString(fmt.Sprintf("File: %s  Line: %d\n", fact.File, fact.Line))

			// Show props summary
			if sig, ok := fact.Props["signature"].(string); ok {
				sb.WriteString(fmt.Sprintf("Signature:\n```\n%s\n```\n", sig))
			}
			if comp, ok := fact.Props["ios_component"].(string); ok {
				sb.WriteString(fmt.Sprintf("iOS Component: %s\n", comp))
			}
			if g := store.Graph(); g != nil {
				if pages := g.GoverningIntentForFile(fact.Repo, fact.File); len(pages) > 0 {
					var refs []string
					for _, p := range pages {
						refs = append(refs, p.Page+pageMeta(p.Type, p.Status))
					}
					sb.WriteString("Governed by: " + strings.Join(refs, "; ") + "\n")
				}
			}

			sb.WriteString("\n")

			// Read source file (handles both single-repo and multi-repo paths)
			absFile := s.eng.ResolveFactFile(&fact)
			source, err := readSourceWindow(absFile, fact.Line, contextLines)
			if err != nil {
				sb.WriteString(fmt.Sprintf("_Could not read source: %v_\n", err))
				continue
			}

			lang := "go"
			if l, ok := fact.Props["language"].(string); ok && l != "" {
				lang = l
			}
			sb.WriteString(fmt.Sprintf("```%s\n%s\n```\n", lang, source))
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: sb.String()},
			},
		}, nil, nil
	})

	// Tool: explore
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "explore",
		Description: "Primary exploration tool — use this first after generate_snapshot. " +
			"Given a module name, file path, symbol name, or directory prefix, returns a structured markdown summary: " +
			"symbols (with kinds and line numbers), direct dependencies, and reverse dependents. " +
			"At depth=2 the default output_mode='summary' returns an aggregated Insights section (dependency hotspots, cycle/layer warnings, size metrics) — \"what is architecturally significant\" — instead of a raw symbol-relations dump; set output_mode='compact'/'full' to get the per-symbol relations list instead. " +
			"'Module' means a package-level grouping (e.g. a Go package or TypeScript file group), not a repo. " +
			"Accepts absolute filesystem paths — they are normalised automatically. Pass max_tokens to hard-cap large directory/module output. " +
			"Use query_facts for precise filtering, traverse for multi-hop graph walks.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args exploreArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		if args.Focus == "" {
			return errorResult("focus is required"), nil, nil
		}

		depth := args.Depth
		if depth <= 0 {
			depth = 1
		}
		if depth > 2 {
			depth = 2
		}

		var sb strings.Builder

		// Normalize absolute filesystem paths to store-relative paths.
		focus := s.normalizeToRelative(args.Focus)

		// Try to determine focus type by matching against store indexes.
		// Priority: exact module name > exact file > symbol name substring > file prefix (directory)
		// Special case: "." means the repo root (from normalizing an absolute path that
		// equals the snapshot RepoPath). Route directly to directory exploration to avoid
		// "." accidentally substring-matching dotted symbol names.
		// At depth=2 the default 'summary' mode replaces the raw per-symbol relations
		// dump with an aggregated Insights section. compact/full keep the dump.
		mode := resolveOutputMode(args.OutputMode, modeSummary)

		switch {
		case focus == "." && s.exploreDirectory(store, focus, &sb):
		case focus != "." && s.exploreModule(store, focus, depth, mode, &sb):
		case focus != "." && s.exploreModuleSubstring(store, focus, depth, mode, &sb):
		case focus != "." && s.exploreFile(store, focus, depth, &sb):
		case focus != "." && s.exploreSymbol(store, focus, depth, &sb):
		case s.exploreDirectory(store, focus, &sb):
		default:
			return errorResult(fmt.Sprintf("No facts matching focus %q. Try a module name, file path, symbol name, or directory prefix.", focus)), nil, nil
		}

		return textResult(capTokens(sb.String(), args.MaxTokens, false)), nil, nil
	})

	// Tool: traverse
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "traverse",
		Description: "Walk the dependency/call graph from a starting node. " +
			"direction='forward' answers \"what does X depend on?\"; direction='reverse' answers \"what depends on X?\". " +
			"start= accepts substring match plus scoped prefixes (repo:, kind:, file:) and package-qualified names (e.g. 'domain/cart.CartService') to disambiguate; returns ranked candidates with confidence when ambiguous. " +
			"relation_kinds filter: imports, calls, declares, implements, depends_on, has_method. " +
			"Forward traversal from a struct/interface follows has_method edges to its methods (and then their calls). " +
			"Reverse traversal from a struct/interface automatically includes its methods and constructor as origins, so it surfaces callers (including cross-repo) that reference the type only through a method — matching impact_analysis. " +
			"Note: interface method calls cannot be statically bound to a concrete implementation, so such call edges may be absent or appear as unresolved nodes. " +
			"node_kinds filters output (not traversal itself): module, symbol, dependency, route, storage. " +
			"TOKEN COST — output_mode ladder: 'summary' (DEFAULT) aggregates counts by node/relation kind, internal/external split, and hottest modules (small, no node list); 'compact' lists nodes grouped by depth; 'full' returns the raw JSON node/edge graph and can be VERY large. " +
			"Start with summary; escalate to compact/full only when you need specific nodes. Always keep max_depth/max_nodes bounded, and pass max_tokens to hard-cap the response. " +
			"Defaults: max_depth=5, max_nodes=100. Use instead of repeated explore calls for transitive relationships.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args traverseArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		graph := store.Graph()
		if graph == nil {
			return errorResult("No graph available. Run generate_snapshot first."), nil, nil
		}

		if args.Start == "" {
			return errorResult("start is required"), nil, nil
		}

		// Resolve start name: try exact match first, then substring
		startName, res, err := s.resolveNodeName(store, args.Start)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		mode := resolveOutputMode(args.OutputMode, modeSummary)

		// Over threshold: refuse to guess; return resolution with empty results.
		if res != nil && res.Matched == "" {
			resp := traverseResponse{
				Resolution: res,
				TraversalResult: facts.TraversalResult{
					Nodes: []facts.TraversalNode{},
					Edges: []facts.TraversalEdge{},
				},
			}
			if wantsFullOutput(mode) {
				return jsonResultCapped(resp, args.MaxTokens)
			}
			if wantsSummary(mode) {
				return textResult(capTokens(s.renderTraverseSummary(store, resp, args.Start, ""), args.MaxTokens, false)), nil, nil
			}
			return textResult(capTokens(renderTraverseCompact(resp, args.Start, ""), args.MaxTokens, false)), nil, nil
		}

		direction := args.Direction
		if direction == "" {
			direction = "forward"
		}
		if direction != "forward" && direction != "reverse" {
			return errorResult("direction must be 'forward' or 'reverse'"), nil, nil
		}

		// Reverse traversal of a type must seed its methods + constructor (callers
		// reference those, not the bare type), matching impact_analysis — otherwise
		// cross-repo and same-repo dependents are missed. Forward already follows
		// has_method edges from the type, so it needs no rollup.
		var result facts.TraversalResult
		if direction == "reverse" {
			result = graph.TraverseFrom(graph.RollupSeeds(startName), direction, args.RelationKinds, args.NodeKinds, args.MaxDepth, args.MaxNodes)
		} else {
			result = graph.Traverse(startName, direction, args.RelationKinds, args.NodeKinds, args.MaxDepth, args.MaxNodes)
		}

		resp := traverseResponse{Resolution: res, TraversalResult: result}
		if wantsFullOutput(mode) {
			return jsonResultCapped(resp, args.MaxTokens)
		}
		if wantsSummary(mode) {
			return textResult(capTokens(s.renderTraverseSummary(store, resp, startName, direction), args.MaxTokens, false)), nil, nil
		}
		return textResult(capTokens(renderTraverseCompact(resp, startName, direction), args.MaxTokens, false)), nil, nil
	})

	// Tool: find_path
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "find_path",
		Description: "Find the shortest path (BFS, by hop count) between two nodes in the architectural graph. " +
			"Answers \"how does X reach Y?\" or \"what is the call chain from A to B?\". " +
			"from= and to= use substring match with smart disambiguation, and accept scoped prefixes " +
			"(repo:, kind:, file:) plus PACKAGE-QUALIFIED names to pin down a common short name — e.g. " +
			"to=\"ticket.Repository\" or to=\"repo:golf domain/cart.CartService\" resolves where bare " +
			"\"Repository\"/\"CartService\" would be ambiguous. " +
			"When an endpoint is ambiguous, find_path TRIES the top candidates (and, for a type, its methods/constructor) " +
			"and returns the first path it finds; the response carries resolution objects with the ranked candidates. " +
			"If no path connects any candidate pair, found=false and a 'note' explains whether the endpoints were " +
			"ambiguous (with the candidates tried) or resolved uniquely but unreachable within max_depth hops.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args findPathArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		graph := store.Graph()
		if graph == nil {
			return errorResult("No graph available. Run generate_snapshot first."), nil, nil
		}

		if args.From == "" || args.To == "" {
			return errorResult("both 'from' and 'to' are required"), nil, nil
		}

		fromName, fromRes, err := s.resolveNodeName(store, args.From)
		if err != nil {
			return errorResult(fmt.Sprintf("from: %v", err)), nil, nil
		}
		toName, toRes, err := s.resolveNodeName(store, args.To)
		if err != nil {
			return errorResult(fmt.Sprintf("to: %v", err)), nil, nil
		}

		// Build ranked candidate lists for each endpoint (most-likely first) and try
		// a path across the combinations rather than silently giving up when a name
		// is ambiguous. This delivers the "give me vague names and I'll find the
		// connection" behavior.
		fromCands := s.pathCandidates(store, args.From, fromName, fromRes)
		toCands := s.pathCandidates(store, args.To, toName, toRes)
		if len(fromCands) == 0 || len(toCands) == 0 {
			return jsonResult(findPathResponse{
				FromResolution: fromRes,
				ToResolution:   toRes,
				PathResult:     facts.PathResult{From: fromName, To: toName, Found: false},
				Note:           "could not resolve both endpoints to a graph node",
				FromTried:      fromCands,
				ToTried:        toCands,
			})
		}

		result := s.bestPath(graph, fromCands, toCands, args.RelationKinds, args.MaxDepth)

		resp := findPathResponse{
			FromResolution: fromRes,
			ToResolution:   toRes,
			PathResult:     result,
			FromTried:      fromCands,
			ToTried:        toCands,
		}
		if !result.Found {
			ambiguous := len(fromCands) > 1 || len(toCands) > 1
			if ambiguous {
				resp.Note = fmt.Sprintf("no path within %d hops between any candidate pair "+
					"(from: %d candidate(s), to: %d candidate(s)). Narrow with a package-qualified "+
					"name (e.g. \"repo:<label> pkg.Type\") — see from_tried/to_tried.",
					effectiveMaxDepth(args.MaxDepth), len(fromCands), len(toCands))
			} else {
				resp.Note = fmt.Sprintf("both endpoints resolved uniquely, but no path connects them within %d hops",
					effectiveMaxDepth(args.MaxDepth))
			}
		}
		return jsonResult(resp)
	})

	// Tool: endpoint_impact
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "endpoint_impact",
		Description: "Answer what changing an HTTP endpoint reaches: the route, the controller serving it, the models that controller touches, the models associated with those, and the physical tables behind them. " +
			"Use impact_analysis when you have a symbol; use this when what you have is a URL. " +
			"Client call sites and mock-server routes are excluded — this answers about what the application serves. " +
			"Each hop can run out, and the result names the one that did: a route whose controller does not resolve is reported as reaching an UNKNOWN set, not an empty one.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args endpointImpactArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		if args.Endpoint == "" {
			return errorResult("endpoint is required"), nil, nil
		}
		// Callers come from the cross-repo linker's own matching, under the server's
		// linking vocabulary, so a call it linked is a caller here too.
		linkVocab, err := s.eng.Config().LinkingVocab()
		if err != nil {
			return errorResult(fmt.Sprintf("invalid linking vocabulary: %v", err)), nil, nil
		}
		return jsonResult(store.AnalyzeEndpoint(args.Endpoint, args.MaxRoutes,
			httpsignal.NewCallerFinder(routeindex.New(linkVocab), store.All())))
	})

	// Tool: impact_analysis
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "impact_analysis",
		Description: "Compute the blast radius of changing a target node: all nodes that transitively depend on it, grouped by hop depth. " +
			"Use for refactoring planning and change risk assessment. " +
			"When knowledge pages declare anchors (enola_intent), the result also lists the pages anchored to the target's file — the decisions governing the code you are about to change. " +
			"target= uses substring match with smart disambiguation. " +
			"Default: reverse direction only (what breaks if target changes). " +
			"Set include_forward=true to also see what the target itself depends on (useful for understanding what could break the target). " +
			"TOKEN COST — output_mode ladder: 'summary' (DEFAULT) gives the accurate total dependent count plus breakdowns by kind/depth, hotspot modules, cross-repo reach, and any cycle/layer insights touching the target (small, no node list); 'compact' lists dependents grouped by hop depth; 'full' returns the raw JSON by_depth/edges graph and can be VERY large. " +
			"Start with summary; escalate only when you need the specific nodes. Keep max_depth/max_nodes bounded and pass max_tokens to hard-cap the response. " +
			"Defaults: max_depth=3, max_nodes=200.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args impactAnalysisArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		graph := store.Graph()
		if graph == nil {
			return errorResult("No graph available. Run generate_snapshot first."), nil, nil
		}

		if args.Target == "" {
			return errorResult("target is required"), nil, nil
		}

		targetName, res, err := s.resolveNodeName(store, args.Target)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		if targetName != "" {
			canonical, normalization := canonicalImpactTarget(store, targetName)
			if canonical != targetName {
				targetName = canonical
				// An exact file_ref normally has no resolution note. Surface this
				// normalization because it materially changes what was traversed.
				if res == nil {
					res = normalization
				}
			}
		}
		mode := resolveOutputMode(args.OutputMode, modeSummary)

		// Over threshold: refuse to guess; return resolution with empty results.
		if res != nil && res.Matched == "" {
			resp := impactResponse{
				Resolution: res,
				ImpactResult: facts.ImpactResult{
					Target:  args.Target,
					ByDepth: map[int][]facts.TraversalNode{},
					Edges:   []facts.TraversalEdge{},
				},
			}
			if wantsFullOutput(mode) {
				return jsonResultCapped(resp, args.MaxTokens)
			}
			if wantsSummary(mode) {
				return textResult(capTokens(s.renderImpactSummary(resp), args.MaxTokens, false)), nil, nil
			}
			return textResult(capTokens(renderImpactCompact(resp), args.MaxTokens, false)), nil, nil
		}

		result := graph.ImpactSet(targetName, args.MaxDepth, args.MaxNodes, args.IncludeForward)

		resp := impactResponse{Resolution: res, ImpactResult: result}
		if wantsFullOutput(mode) {
			return jsonResultCapped(resp, args.MaxTokens)
		}
		if wantsSummary(mode) {
			return textResult(capTokens(s.renderImpactSummary(resp), args.MaxTokens, false)), nil, nil
		}
		return textResult(capTokens(renderImpactCompact(resp), args.MaxTokens, false)), nil, nil
	})

	// Tool: governing_intent
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "governing_intent",
		Description: "Answer the reverse query between knowledge and code directly, in either direction. " +
			"For a code target — a fact name (exact) or a file path (label-prefixed or repo-relative) — list the knowledge pages whose declared anchors cover its file, each with type, status, and its outgoing relations, so the trail continues past the first hop. " +
			"For a compiled page path, list the page's declared anchors with measured coverage (files and facts under each). " +
			"Cheaper than impact_analysis when the question is governance, not blast radius. " +
			"An empty result distinguishes 'no knowledge pages compiled in this snapshot' (the graph was not asked) from 'nothing governs this target'.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args governingIntentArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		graph := store.Graph()
		if graph == nil {
			return errorResult("No graph available. Run generate_snapshot first."), nil, nil
		}
		if args.Target == "" {
			return errorResult("target is required"), nil, nil
		}
		if !graph.HasCompiledPages() {
			return textResult("No knowledge pages are compiled into this snapshot — the reverse query was not asked. " +
				"Pages opt in with an enola_intent page declaration and compile at snapshot time; see docs/INTENT.md."), nil, nil
		}

		if cov, found := graph.GovernedByPage(args.Target); found {
			return textResult(capTokens(renderGovernedCode(args.Target, cov), args.MaxTokens, false)), nil, nil
		}

		type site struct{ repo, file string }
		var sites []site
		seen := map[site]bool{}
		add := func(repo, file string) {
			st := site{repo, file}
			if repo != "" && file != "" && !seen[st] {
				seen[st] = true
				sites = append(sites, st)
			}
		}
		for _, m := range store.LookupByExactName(args.Target) {
			if m.Kind == facts.KindIntent || (args.Repo != "" && m.Repo != args.Repo) {
				continue
			}
			add(m.Repo, m.File)
		}
		if len(sites) == 0 {
			for _, f := range store.All() {
				if f.Kind == facts.KindIntent || f.File == "" || f.Repo == "" ||
					(args.Repo != "" && f.Repo != args.Repo) {
					continue
				}
				if f.File == args.Target || strings.TrimPrefix(f.File, f.Repo+"/") == args.Target {
					add(f.Repo, f.File)
				}
			}
		}
		if len(sites) == 0 {
			return textResult(fmt.Sprintf("Nothing measured matches %q — not a compiled page, an exact fact name, or a measured file path. "+
				"File paths join in the label-prefixed and repo-relative forms; pass repo= to scope one.", args.Target)), nil, nil
		}
		if len(sites) > 5 {
			sites = sites[:5]
		}

		var sb strings.Builder
		for i, st := range sites {
			if i > 0 {
				sb.WriteString("\n---\n\n")
			}
			fmt.Fprintf(&sb, "### %s (%s)\n\n", st.file, st.repo)
			pages := graph.GoverningIntentForFile(st.repo, st.file)
			if len(pages) == 0 {
				sb.WriteString("No knowledge page anchors this file — asked, none governs.\n")
				continue
			}
			writeGoverningIntent(&sb, pages)
		}
		return textResult(capTokens(sb.String(), args.MaxTokens, false)), nil, nil
	})

	// Tool: constraints_for
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "constraints_for",
		Description: "The pre-edit contract for a file or fact: which declared constraint components contain it, what every rule binding those components says (in words, with the rationale it was declared with and its enforcement mode), and the current constraint violations whose evidence names the target. " +
			"Guidance rules ride each component's own guidance list — steering, not law — carrying the advice message, its mode, and each exemplar of prior art annotated present, absent, or unmeasured against this snapshot. " +
			"Ask BEFORE editing — the target may be a file path that does not exist yet, matched against component patterns, so the answer covers code about to be written. " +
			"target= is a file path (repo-relative) or an exact fact name; matching is exact and fail-closed, like the constraints explainer's own. " +
			"Returns JSON. An empty result distinguishes 'no constraint components compiled in this snapshot' (the contract was not asked) from 'nothing binds this target'.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args constraintsForArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		if args.Target == "" {
			return errorResult("target is required"), nil, nil
		}
		target := s.normalizeToRelative(args.Target)
		bindings, declared := constraints.ContractFor(store, target)
		if !declared {
			return textResult("No constraint components are compiled into this snapshot — the contract was not asked. " +
				"Components and rules ride enola_intent page declarations and compile at snapshot time; see docs/INTENT.md."), nil, nil
		}
		resp := constraintsForResponse{Target: target, Components: bindings}
		if snap := s.eng.Snapshot(); snap != nil {
			resp.Violations = constraints.ViolationsReferencing(snap.Insights, target)
		}
		return jsonResultCapped(resp, args.MaxTokens)
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "plan_check",
		Description: "Plan an intended change BEFORE editing: for each target — paths (which may not exist yet) or exact fact names — the declared constraint components and rules governing it, with mode and the because: each was declared with, plus its blast radius (fan-in/fan-out over the current snapshot, capped samples with exact counts). " +
			"Pass patch= (a unified diff) for the counterfactual: the patch is applied to a scratch copy of the repo — the working tree and its .enola are never touched — facts are regenerated there, and the constraint verdicts that WOULD appear are reported in new/resolved/unchanged buckets, each naming the rule, the would-be witness, and its because:. " +
			"Governance answers from the working tree's declarations (enola-intent.yaml plus enola/constraints/), so an edit to the law is visible without regenerating. " +
			"A patch that does not apply, or that touches files outside the snapshot's scope, is a named error, never a guess. When no rule governs a target the report says so explicitly, and the snapshot's staleness relative to the tree is stated rather than silently answered around. " +
			"A report, never a gate: like enola check, the verdict is for the caller to weigh. Returns JSON.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args planCheckArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		repoPath := s.currentRepoPath()
		if repoPath == "" {
			return errorResult("No snapshot available. Run generate_snapshot first."), nil, nil
		}
		if len(args.Paths) == 0 && len(args.Symbols) == 0 && args.Patch == "" {
			return errorResult("nothing to plan: give paths, symbols, or patch"), nil, nil
		}
		label := filepath.Base(repoPath)
		contractStore, err := plan.ContractStore(repoPath, store.All(), s.eng.Config().Intent[label])
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		info := plan.SnapshotInfo{}
		if snap := s.eng.Snapshot(); snap != nil {
			info.GeneratedAt = snap.Meta.GeneratedAt
		}
		if d, driftErr := s.eng.Drift(repoPath); driftErr == nil && (d.Unknown || d.Any()) {
			info.Staleness = d.Summary(5)
		}
		paths := make([]string, 0, len(args.Paths))
		for _, p := range args.Paths {
			paths = append(paths, s.normalizeToRelative(p))
		}
		deps := plan.Deps{
			RepoPath:      repoPath,
			RepoLabel:     label,
			Store:         contractStore,
			Snapshot:      info,
			OutputDirName: s.eng.Config().Output.Dir,
			NewEngine:     s.planFactory,
		}
		report, err := plan.Compute(ctx, plan.Request{Paths: paths, Symbols: args.Symbols, Patch: []byte(args.Patch)}, deps)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		return jsonResultCapped(report, args.MaxTokens)
	})

	// Tool: coverage_report
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "coverage_report",
		Description: "Report per-service edge coverage so you can tell a genuinely isolated service from one whose edges enola could not resolve. " +
			"For each service (repo) node it shows resolved outbound dependencies plus, per edge type (currently http_client), how many outbound call sites were detected, resolved to a loaded service, and left unresolved. " +
			"Each service is classified: 'connected' (has resolved outbound edges), 'coverage_gap' (no resolved outbound edges but unresolved call sites were detected — likely NOT isolated, verify against source), or 'isolated' (no outbound edges and no detected call sites — genuinely a leaf). " +
			"Use this before concluding a service is isolated. Only meaningful for multi-repo (append-mode) snapshots; single-repo snapshots have no service nodes. " +
			"output_mode='summary' (DEFAULT) returns a markdown table; 'full' returns JSON. Optional repo= filters to one service.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args coverageReportArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		report := coverage.Build(store, args.Repo)
		if len(report) == 0 {
			if args.Repo != "" {
				return errorResult(fmt.Sprintf("No service node named %q. coverage_report needs a multi-repo (append-mode) snapshot.", args.Repo)), nil, nil
			}
			return textResult("No service nodes found. coverage_report needs a multi-repo (append-mode) snapshot." +
				coverage.RenderClientsMarkdown(coverage.BuildClients(store))), nil, nil
		}

		if resolveOutputMode(args.OutputMode, modeSummary) == modeFull {
			return jsonResult(report)
		}
		return textResult(report.RenderMarkdown() + coverage.RenderClientsMarkdown(coverage.BuildClients(store))), nil, nil
	})

	// Tool: query_insights
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "query_insights",
		Description: "Return the architectural findings (insights) that explainers computed during generate_snapshot — the first-class answer to questions like \"which routes are unused?\", \"where are the dependency cycles?\", or \"which modules are god-classes?\". " +
			"Each insight carries a title, the explainer that produced it, a description, a confidence (0-1; lower = candidate to verify, not a verdict), evidence (files/symbols/routes), and suggested actions. " +
			"Filter by explainer= — one of: " + explainerFilterList() + "; repo= (in multi-repo snapshots, matches the repo-prefix path segment of each insight's evidence — e.g. \"golf\" matches golf/... but not golf-ui/...; single-repo snapshots fall back to a substring match); and min_confidence=. " +
			"output_mode ladder: 'summary' (DEFAULT — one row per insight: explainer, confidence, title) → 'compact' (adds description, an evidence sample, and suggested actions) → 'full' (complete JSON incl. all evidence and actions). Pass max_tokens to hard-cap output. " +
			"All explainers populate insights, but route/cross-repo findings (unused-routes, crossrepo, coverage) only appear for multi-repo (append-mode) snapshots of a backend plus its clients. " +
			"Prefer this over hand-diffing query_facts results: e.g. query_insights(explainer=\"unused-routes\") returns the per-repo dead-route candidates directly.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args queryInsightsArgs) (*mcp.CallToolResult, any, error) {
		snap := s.eng.Snapshot()
		if snap == nil {
			return errorResult("No snapshot available. Run generate_snapshot first."), nil, nil
		}

		repos := map[string]struct{}{}
		for _, repo := range s.eng.Store().RepoLabels() {
			repos[strings.ToLower(repo)] = struct{}{}
		}
		matched := filterInsights(snap.Insights, args.Explainer, args.Repo, args.MinConfidence, len(repos) > 1)
		if len(matched) == 0 {
			return textResult(noMatchInsightsMessage(args.Explainer, args.Repo, args.MinConfidence, snap, s.eng.Store())), nil, nil
		}

		switch resolveOutputMode(args.OutputMode, modeSummary) {
		case modeFull:
			return jsonResultCapped(matched, args.MaxTokens)
		case modeCompact:
			return textResult(capTokens(renderInsightsCompact(matched), args.MaxTokens, false)), nil, nil
		default:
			return textResult(capTokens(renderInsightsSummary(matched), args.MaxTokens, false)), nil, nil
		}
	})

	// Tool: set_baseline
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "set_baseline",
		Description: "Pin the current snapshot as the baseline for diff_snapshot. " +
			"Call this once at the START of a task (after generate_snapshot), make your changes, " +
			"re-run generate_snapshot, then call diff_snapshot to see exactly what the change did to the architecture. " +
			"The pinned baseline survives repeated generate_snapshot runs, so it stays valid across several edit rounds — " +
			"unlike the auto-rotated 'previous' snapshot, which only ever holds the immediately-preceding run.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args setBaselineArgs) (*mcp.CallToolResult, any, error) {
		repoPath := s.currentRepoPath()
		if repoPath == "" {
			return errorResult("No snapshot available. Run generate_snapshot first, then set_baseline."), nil, nil
		}
		if err := s.eng.SetBaseline(repoPath); err != nil {
			return errorResult(fmt.Sprintf("could not set baseline: %v", err)), nil, nil
		}
		return textResult(
			"Baseline pinned from the current snapshot.\n\n" +
				"Now make your changes, re-run generate_snapshot, then call diff_snapshot to see what changed " +
				"(new findings, new coupling, added/removed symbols). The baseline persists across re-snapshots until you pin a new one."), nil, nil
	})

	// Tool: diff_snapshot
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "diff_snapshot",
		Description: "Show what changed in the architecture between a baseline snapshot and the current one — " +
			"the deterministic \"what did my change actually do?\" answer that replaces re-reading files. " +
			"This is a DELTA, not a linter: it reports only what CHANGED (findings that newly appeared or were resolved, " +
			"new/removed coupling edges, added/removed symbols/modules/routes) and stays silent about pre-existing state, " +
			"so a pattern that was already there before and after never fires. " +
			"baseline= selects what to compare against: 'pinned' (DEFAULT — the snapshot you froze with set_baseline), " +
			"'previous' (the immediately-preceding generate_snapshot run, rotated automatically), or an explicit path to a directory holding facts.jsonl. " +
			"Typical loop: generate_snapshot → set_baseline → edit → generate_snapshot → diff_snapshot. " +
			"focus= narrows the report to entries referencing a module/file/symbol (use it to verify just what you touched). " +
			"output_mode ladder: 'summary' (DEFAULT — headline regressions/improvements + structural tally) → 'compact' (adds finding descriptions, evidence, and the changed edges/facts) → 'full' (complete JSON). " +
			"New findings carry their original confidence and caveats; confidence < 1.0 is a candidate to verify, not a verdict.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args diffSnapshotArgs) (*mcp.CallToolResult, any, error) {
		snap := s.eng.Snapshot()
		if snap == nil || s.eng.Store().Count() == 0 {
			return errorResult("No snapshot available. Run generate_snapshot first."), nil, nil
		}
		repoPath := s.currentRepoPath()

		// Current = the live engine state (what the agent just generated). Build it
		// from the store so it works even when the snapshot was auto-loaded.
		//
		// FactsRef, not All: diff.Compute only reads its inputs (it groups facts into
		// its own slices and sorts those), and this store is the published bundle,
		// which is immutable once swapped in — exactly FactsRef's contract. All would
		// copy the entire fact set, plus every relation slice, for a read: on a
		// kernel-sized graph that is gigabytes of transient garbage on a query path.
		current := &facts.Snapshot{Meta: s.eng.MetaFor(repoPath), Facts: s.eng.Store().FactsRef(), Insights: snap.Insights}

		// Resolve the baseline directory from the selector.
		sel := strings.TrimSpace(args.Baseline)
		baseline, err := engine.LoadSnapshotDir(s.resolveBaselineDir(repoPath, sel))
		if err != nil {
			switch strings.ToLower(sel) {
			case "", "pinned":
				return errorResult("No pinned baseline found. Call set_baseline after your first generate_snapshot, " +
					"then make changes, re-run generate_snapshot, and diff_snapshot again. " +
					"(Or pass baseline='previous' to compare against the immediately-preceding snapshot.)"), nil, nil
			case "previous":
				return errorResult("No previous snapshot yet — generate_snapshot must have run at least twice. " +
					"(Or use set_baseline + baseline='pinned' for a stable task baseline.)"), nil, nil
			default:
				return errorResult(fmt.Sprintf("could not load baseline from %q: %v", sel, err)), nil, nil
			}
		}

		d := diff.Compute(baseline, current)
		if args.Focus != "" {
			d = d.Focused(s.normalizeToRelative(args.Focus))
		}

		// Conformance is computed only when the caller declares an intent, or when a
		// plain delta is worth checking against its own edit sites. It needs a graph over
		// the BASELINE's facts — a store assembled from a loaded snapshot has no index
		// until one is built — so it is not free, and it is not run for callers who asked
		// for a delta and nothing more.
		var conf *conformance.Report
		if args.Target != "" || len(args.ExpectedPackages) > 0 {
			baseStore := facts.NewStore()
			baseStore.Add(baseline.Facts...)
			rep := conformance.Compute(baseStore, s.eng.Store(), d, conformance.Options{
				Target:           args.Target,
				ExpectedPackages: args.ExpectedPackages,
				MaxDepth:         args.MaxDepth,
				MaxNodes:         args.MaxNodes,
			})
			conf = &rep
		}
		// Whether the CURRENT snapshot still matches the working tree is something only
		// the caller can establish, and it is the one caveat that invalidates the whole
		// delta: if the tree moved since the snapshot, this diff describes neither the
		// old state nor the new one. Checked here rather than in the freshness banner
		// because it re-hashes the repo — affordable at a deliberate decision point,
		// not on every tool call. Focused() preserves Comparability, so this is added
		// after narrowing and still renders above the delta.
		drift.AddWarning(d, s.eng, repoPath, "diff_snapshot")

		switch resolveOutputMode(args.OutputMode, modeSummary) {
		case modeFull:
			if conf == nil {
				return jsonResultCapped(d, args.MaxTokens)
			}
			// Embedded, so the delta's fields stay at the top level and a consumer that
			// never asks for conformance sees the shape it always saw.
			return jsonResultCapped(struct {
				*diff.SnapshotDiff
				Conformance *conformance.Report `json:"conformance,omitempty"`
			}{d, conf}, args.MaxTokens)
		case modeCompact:
			return textResult(capTokens(conformanceFirst(conf, d.RenderCompact()), args.MaxTokens, false)), nil, nil
		default:
			return textResult(capTokens(conformanceFirst(conf, d.RenderSummary()), args.MaxTokens, false)), nil, nil
		}
	})

	// Tool: snapshot_receipt
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "snapshot_receipt",
		Description: "Show the receipt for the current snapshot — a compact manifest that proves WHAT the deterministic graph was generated over, " +
			"and how complete the extraction was. Fields: enola version, git ref + dirty-tree status, snapshot_id (a content fingerprint, stable across reruns on identical inputs), " +
			"the extractor/explainer sets actually used, the ignore-glob hash, SHA-256 hashes of the output artifacts, and extraction-quality metrics " +
			"(files seen vs parsed vs skipped, parse-error count, and cross-repo coverage gaps / unresolved edges). " +
			"Read it before trusting an impact_analysis or a diff, and to spot thin extraction (a missing detection, a bad ignore glob, a failing extractor). " +
			"output_mode: 'summary' (DEFAULT — the headline provenance + quality metrics) → 'full' (complete JSON receipt).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args snapshotReceiptArgs) (*mcp.CallToolResult, any, error) {
		snap := s.eng.Snapshot()
		if snap == nil || s.eng.Store().Count() == 0 {
			return errorResult("No snapshot available. Run generate_snapshot first."), nil, nil
		}
		receipt := snap.Meta.Receipt()
		if resolveOutputMode(args.OutputMode, modeSummary) == modeFull {
			return jsonResultCapped(receipt, args.MaxTokens)
		}
		return textResult(capTokens(renderReceipt(receipt), args.MaxTokens, false)), nil, nil
	})

	// Tool: compare_receipts
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "compare_receipts",
		Description: "Compare the current snapshot's receipt against a baseline's, BEFORE trusting a diff between them. " +
			"Returns (1) a comparability verdict — whether the two were generated over equivalent inputs (same repo, enola version, extractor set, ignore globs); " +
			"a mismatch means a diff_snapshot between them would report spurious churn — and (2) metric deltas (files parsed, parse errors, coverage gaps, unresolved edges, fact/insight counts), " +
			"flagging extraction-quality REGRESSIONS (enola's own extraction got thinner). Use it as the gate before diff_snapshot, or poll it to drive improvements to enola's coverage. " +
			"baseline= selects what to compare against: 'pinned' (DEFAULT — set_baseline snapshot), 'previous' (the immediately-preceding run), or an explicit path to a directory holding receipt.json/snapshot.meta.json. " +
			"output_mode: 'summary' (DEFAULT — markdown) → 'full' (complete JSON).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args compareReceiptsArgs) (*mcp.CallToolResult, any, error) {
		snap := s.eng.Snapshot()
		if snap == nil || s.eng.Store().Count() == 0 {
			return errorResult("No snapshot available. Run generate_snapshot first."), nil, nil
		}
		baseDir := s.resolveBaselineDir(s.currentRepoPath(), args.Baseline)
		baseline, err := engine.LoadSnapshotDir(baseDir)
		if err != nil {
			sel := strings.ToLower(strings.TrimSpace(args.Baseline))
			switch sel {
			case "", "pinned":
				return errorResult("No pinned baseline found. Call set_baseline after your first generate_snapshot, then compare_receipts again. (Or pass baseline='previous'.)"), nil, nil
			case "previous":
				return errorResult("No previous snapshot yet — generate_snapshot must have run at least twice."), nil, nil
			default:
				return errorResult(fmt.Sprintf("could not load baseline from %q: %v", args.Baseline, err)), nil, nil
			}
		}
		rc := diff.CompareReceipts(baseline.Meta, s.eng.MetaFor(s.currentRepoPath()))
		if resolveOutputMode(args.OutputMode, modeSummary) == modeFull {
			return jsonResultCapped(rc, args.MaxTokens)
		}
		return textResult(capTokens(rc.Render(), args.MaxTokens, false)), nil, nil
	})
}

// conformanceFirst puts the scope verdict ahead of the delta.
//
// A caller who declared an intent asked a yes/no question, and the answer must not sit
// below a page of structural tallies — the delta is the evidence, not the finding.
func conformanceFirst(conf *conformance.Report, body string) string {
	if conf == nil {
		return body
	}
	if s := conf.Render(); s != "" {
		return strings.TrimLeft(s, "\n") + "\n" + body
	}
	return body
}

// resolveBaselineDir maps a baseline selector ('pinned'/”/'previous'/explicit
// path) to the on-disk directory holding that snapshot's artifacts — the same
// resolution diff_snapshot uses, shared so compare_receipts stays consistent.
func (s *Server) resolveBaselineDir(repoPath, selector string) string {
	return engine.ResolveBaselineDir(s.eng.OutputDir(repoPath), selector)
}

// renderReceipt produces the compact markdown summary for the snapshot_receipt
// tool: provenance first, then the extraction-quality metrics that flag thin
// extraction.
func renderReceipt(r facts.Receipt) string {
	var sb strings.Builder
	sb.WriteString("# Snapshot receipt\n\n")

	sb.WriteString("## Provenance\n\n")
	fmt.Fprintf(&sb, "- **Snapshot ID:** `%s`\n", orDashStr(r.SnapshotID))
	fmt.Fprintf(&sb, "- **enola version:** %s\n", orDashStr(r.EnolaVersion))
	fmt.Fprintf(&sb, "- **Generated:** %s (%s)\n", orDashStr(r.GeneratedAt), r.Duration)
	fmt.Fprintf(&sb, "- **Repository:** %s\n", orDashStr(r.RepoPath))
	if r.Git != nil {
		dirty := "clean"
		if r.Git.Dirty {
			dirty = "**dirty (uncommitted changes)**"
		}
		fmt.Fprintf(&sb, "- **Git:** %s @ `%s` — %s\n", orDashStr(r.Git.Ref), shortSHA(r.Git.Commit), dirty)
	} else {
		sb.WriteString("- **Git:** not a git repository\n")
	}
	fmt.Fprintf(&sb, "- **Extractors:** %s\n", strings.Join(r.Extractors, ", "))
	if r.ConfigHash != "" {
		fmt.Fprintf(&sb, "- **Config hash:** `%s`\n", r.ConfigHash)
	}
	fmt.Fprintf(&sb, "- **Facts:** %d · **Insights:** %d\n\n", r.FactCount, r.InsightCount)

	q := r.Quality
	sb.WriteString("## Extraction quality\n\n")
	fmt.Fprintf(&sb, "- **Files:** %d parsed / %d seen · %d file(s) + %d directory tree(s) skipped by ignore globs\n",
		q.FilesParsed, q.FilesSeen, q.FilesSkipped, q.DirsSkipped)
	fmt.Fprintf(&sb, "- **Parse errors:** %d\n", q.ParseErrors)
	fmt.Fprintf(&sb, "- **Heuristic insights:** %d of %d (confidence < 1.0; the rest are structural facts)\n", q.HeuristicInsights, r.InsightCount)
	if q.Coverage != nil {
		fmt.Fprintf(&sb, "- **Cross-repo coverage:** %d service(s), %d coverage gap(s), %d unresolved edge(s)\n",
			q.Coverage.ServicesTotal, q.Coverage.CoverageGaps, q.Coverage.UnresolvedEdges)
	}
	if len(q.ParseErrorSample) > 0 {
		sb.WriteString("\n**Parse-error sample:**\n")
		for _, pe := range q.ParseErrorSample {
			fmt.Fprintf(&sb, "- [%s] %s%s\n", pe.Extractor, oneLine(pe.Msg), fileSuffix(pe.File))
		}
	}
	sb.WriteString("\n_Use output_mode='full' for the complete JSON receipt (output hashes, skipped-path sample, per-metric detail)._\n")
	return sb.String()
}

// orDashStr returns s, or an em dash when empty, for receipt rendering.
func orDashStr(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// shortSHA truncates a git commit SHA to its first 12 chars for display.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// fileSuffix renders a " (file)" suffix when a parse error names a file.
func fileSuffix(file string) string {
	if file == "" {
		return ""
	}
	return " (" + file + ")"
}

// loopHint returns the next-step guidance appended to a single-repo
// generate_snapshot summary. It is context-aware: before any baseline is pinned
// it points the agent at set_baseline (so it pins BEFORE editing); once a baseline
// exists it points at diff_snapshot (to see what changed). This makes the
// edit-verify loop self-guiding without the agent having to know it up front.
func (s *Server) loopHint(absRepo string) string {
	baselineFacts := filepath.Join(s.eng.OutputDir(absRepo), engine.BaselineSubdir, "facts.jsonl")
	if _, err := os.Stat(baselineFacts); err == nil {
		return "\n\n**Verify your change:** a baseline is pinned — call diff_snapshot to see exactly what changed since set_baseline " +
			"(new/resolved findings, new coupling, added/removed symbols). Re-pin from here with set_baseline if you're starting a new change."
	}
	return "\n\n**About to change code?** Call set_baseline now to freeze this snapshot as the baseline, then after editing re-run " +
		"generate_snapshot and diff_snapshot to see exactly what your change did — no need to re-read files to confirm it."
}

// currentRepoPath returns the absolute repo path of the live snapshot, falling
// back to the configured repo. Empty only when neither is available.
func (s *Server) currentRepoPath() string {
	if snap := s.eng.Snapshot(); snap != nil && snap.Meta.RepoPath != "" {
		return snap.Meta.RepoPath
	}
	if abs, err := filepath.Abs(s.cfg.Repo); err == nil {
		return abs
	}
	return ""
}

type queryInsightsArgs struct {
	Explainer     string  `json:"explainer,omitempty" jsonschema:"Filter to insights produced by this explainer. One of: cycles, layers, crossrepo, coverage, unused-routes, messaging-coverage, god-class, hotspots, dependency-depth, exported-surface, complexity-outliers, intent, constraints, domain, query-loops, entry-points, dead-methods, vendored-candidates, import-closure, package-metrics, dead-code, performance. Empty = all."`
	Repo          string  `json:"repo,omitempty" jsonschema:"Filter to insights about this repo label. In multi-repo snapshots this matches the repo-prefix path segment of each insight's evidence files (so 'golf' matches golf/... but not golf-ui/...); single-repo snapshots fall back to a substring match. Empty = all repos."`
	MinConfidence float64 `json:"min_confidence,omitempty" jsonschema:"Only return insights with confidence >= this (0.0-1.0). Default 0 (all). Unused-routes is emitted at 0.6 as a review candidate."`
	OutputMode    string  `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT — one row per insight: explainer, confidence, title) → 'compact' (adds description, evidence sample, actions) → 'full' (complete JSON)."`
	MaxTokens     int     `json:"max_tokens,omitempty" jsonschema:"Optional hard cap on output size (approx tokens). Default: no cap."`
}

// setBaselineArgs has no parameters: set_baseline always pins the current snapshot.
type setBaselineArgs struct{}

type diffSnapshotArgs struct {
	Baseline         string   `json:"baseline,omitempty" jsonschema:"What to compare against: 'pinned' (DEFAULT — the snapshot frozen by set_baseline), 'previous' (the immediately-preceding generate_snapshot run, rotated automatically), or an explicit path to a directory containing facts.jsonl."`
	Focus            string   `json:"focus,omitempty" jsonschema:"Optional: narrow the diff to entries referencing this module, file, or symbol (substring match). Use it to verify only the area you changed."`
	OutputMode       string   `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT — headline regressions/improvements + structural tally) → 'compact' (adds finding descriptions, evidence, changed edges/facts) → 'full' (complete JSON)."`
	MaxTokens        int      `json:"max_tokens,omitempty" jsonschema:"Optional hard cap on output size (approx tokens). Default: no cap."`
	Target           string   `json:"target,omitempty" jsonschema:"Optional: the symbol, type or package you INTENDED to change. Turns the diff into a conformance check — reverse-dependency impact analysis runs on the PRE-change graph and any package the change reached outside that predicted radius is reported as spillover. Omit for a plain delta."`
	ExpectedPackages []string `json:"expected_packages,omitempty" jsonschema:"Optional: packages you expected this change to touch, in addition to whatever the impact analysis predicts. Anything touched that is neither expected nor predicted is spillover."`
	MaxDepth         int      `json:"max_depth,omitempty" jsonschema:"Reverse-dependency traversal depth for the predicted radius (1-10). Default 3. Only used with target/expected_packages."`
	MaxNodes         int      `json:"max_nodes,omitempty" jsonschema:"Max nodes per impact traversal (1-500). Default 200. Only used with target/expected_packages."`
}

type coverageReportArgs struct {
	Repo       string `json:"repo,omitempty" jsonschema:"Optional: limit the report to one service (repo label). Default: all services."`
	OutputMode string `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT) returns a markdown table; 'full' returns JSON."`
}

type snapshotReceiptArgs struct {
	OutputMode string `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT — headline provenance + extraction-quality metrics) → 'full' (complete JSON receipt)."`
	MaxTokens  int    `json:"max_tokens,omitempty" jsonschema:"Optional hard cap on output size (approx tokens). Default: no cap."`
}

type compareReceiptsArgs struct {
	Baseline   string `json:"baseline,omitempty" jsonschema:"What to compare the current receipt against: 'pinned' (DEFAULT — the snapshot frozen by set_baseline), 'previous' (the immediately-preceding generate_snapshot run), or an explicit path to a directory containing receipt.json / snapshot.meta.json."`
	OutputMode string `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT — markdown: comparability verdict, metric deltas, quality regressions) → 'full' (complete JSON)."`
	MaxTokens  int    `json:"max_tokens,omitempty" jsonschema:"Optional hard cap on output size (approx tokens). Default: no cap."`
}

// edgeCoverage is one edge type's detected-vs-resolved tally for a service.
// ambiguousMatchThreshold is the candidate count at or above which
// resolveNodeName refuses to guess and forces the caller to re-invoke with an
// exact name.
const ambiguousMatchThreshold = 3

// maxAlternatives caps how many candidate names are echoed back in a
// nameResolution so the response stays readable.
const maxAlternatives = 10

// maxCandidates caps how many ranked, scored candidates are surfaced in a
// nameResolution.
const maxCandidates = 3

// autoPickConfidence is the pickConfidence threshold above which an ambiguous
// resolution (at or above ambiguousMatchThreshold matches) is resolved
// automatically to its top-scoring candidate instead of being refused.
const autoPickConfidence = 0.80

// maxPathCandidates caps how many ranked candidates find_path considers per
// endpoint when a name is ambiguous.
const maxPathCandidates = 8

// maxPathAttempts caps the total FindPath probes find_path runs across the
// candidate/seed combinations, so a doubly-ambiguous query stays bounded.
const maxPathAttempts = 40

// nameResolution reports how a user-provided name was resolved to a concrete
// fact name. It is surfaced in tool responses ONLY when the input matched more
// than one fact (i.e. Ambiguous is true), so callers can detect and correct a
// possibly-wrong pick. Matched is empty when the match count crossed
// ambiguousMatchThreshold and no candidate scored confidently enough to
// auto-pick; the caller should then choose from Candidates (optionally using the
// repo:/kind:/file: scope prefixes) or re-invoke with an exact name.
type nameResolution struct {
	Query        string            `json:"query"`
	Matched      string            `json:"matched,omitempty"`
	Alternatives []string          `json:"alternatives,omitempty"`
	Candidates   []scoredCandidate `json:"candidates,omitempty"`
	Confidence   float64           `json:"confidence,omitempty"`
	AutoPicked   bool              `json:"auto_picked,omitempty"`
	Ambiguous    bool              `json:"ambiguous"`
}

// canonicalImpactTarget maps reference-only TypeScript/JavaScript file nodes to the
// extensionless module target imports actually point at. A file_ref records top-level
// calls for dead-code analysis; it is not the dependency node, so reverse traversal from
// it can truthfully see no edges while the corresponding module has many callers.
func canonicalImpactTarget(store *facts.Store, target string) (string, *nameResolution) {
	exact := store.LookupByExactName(target)
	referenceOnly := false
	for _, f := range exact {
		switch f.Kind {
		case facts.KindFileRef, facts.KindTestRef:
			referenceOnly = true
		default:
			return target, nil
		}
	}
	if !referenceOnly {
		return target, nil
	}

	ext := strings.ToLower(filepath.Ext(target))
	switch ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
	default:
		return target, nil
	}
	candidate := strings.TrimSuffix(target, filepath.Ext(target))
	// A module target may be an implicit graph node (relations name it even when no
	// standalone fact does), so confirm it through either a fact or a relation.
	confirmed := len(store.LookupByExactName(candidate)) > 0
	if !confirmed {
		for _, f := range store.All() {
			for _, rel := range f.Relations {
				if rel.Target == candidate {
					confirmed = true
					break
				}
			}
			if confirmed {
				break
			}
		}
	}
	if !confirmed {
		return target, nil
	}
	return candidate, &nameResolution{Query: target, Matched: candidate, AutoPicked: true}
}

// resolveNodeName resolves a user-provided name to an exact fact name.
//
// The input may carry scope prefixes — repo:<label>, kind:<k>, file:<prefix> —
// to disambiguate an otherwise-ambiguous term (see parseScopedQuery). A plain
// input keeps the legacy substring-match behavior.
//
// It returns the resolved name and, when resolution was ambiguous (more than one
// fact matched and no confident pick existed), a non-nil *nameResolution
// describing the ambiguity (ranked Candidates with scores + a Confidence). For
// exact matches, single matches, and confident suffix-exact matches the
// resolution is nil.
//
// When the candidate count reaches ambiguousMatchThreshold the method picks the
// top-scoring candidate only if its pickConfidence exceeds autoPickConfidence;
// otherwise it returns an empty name with a resolution-only response so the
// caller can choose from Candidates or re-invoke with a scoped/exact name.
func (s *Server) resolveNodeName(store *facts.Store, input string) (string, *nameResolution, error) {
	input = s.maybePrefixRepoLabel(input)
	query := input
	sq := parseScopedQuery(input)
	scoped := sq.Repo != "" || len(sq.Kinds) > 0 || sq.FilePrefix != "" || sq.SymbolKind != ""

	term := s.normalizeToRelative(sq.Term)

	// Try exact match first (unscoped only — a scope filter signals the caller
	// wants the candidate set narrowed, not bypassed).
	if !scoped {
		if exact := store.LookupByExactName(term); len(exact) > 0 {
			return exact[0].Name, nil, nil
		}
	}

	results := s.gatherCandidates(store, sq, term)
	if len(results) == 0 {
		// Fallback 1: exact name match on KindService facts (service nodes are
		// named after repo labels and are often missed by substring search).
		for _, svc := range store.ByKind(facts.KindService) {
			if strings.EqualFold(svc.Name, term) {
				return svc.Name, nil, nil
			}
		}
		// Fallback 2: input matches a known repo label whose service node may
		// have an empty name (primary repo) or hasn't been loaded yet.
		if name, ok := s.resolveRepoLabelToServiceNode(store, term); ok {
			return name, nil, nil
		}
		// Fallback 3: the term is a FILE basename (e.g. "auth_routes") with no
		// fact named after it — resolve to the symbol(s) declared in that file, so
		// file-shaped targets are pathable. A single owner resolves directly;
		// several are surfaced as candidates.
		if fileMatches := s.resolveByFileBasename(store, sq, term); len(fileMatches) > 0 {
			if len(fileMatches) == 1 {
				return fileMatches[0].Name, nil, nil
			}
			ranked := rankCandidates(fileMatches, sq)
			return "", &nameResolution{
				Query:        query,
				Alternatives: candidateNames(fileMatches, ""),
				Candidates:   topCandidates(ranked),
				Ambiguous:    true,
			}, nil
		}
		if sugg := s.suggestNames(store, sq, term); len(sugg) > 0 {
			return "", nil, fmt.Errorf("no facts matching %q; did you mean: %s "+
				"(tip: scope with repo:/kind:/file:)", query, strings.Join(sugg, ", "))
		}
		return "", nil, fmt.Errorf("no facts matching %q", query)
	}
	if len(results) == 1 {
		return results[0].Name, nil, nil
	}

	// A service node whose name exactly matches the term is a confident pick.
	for _, svc := range store.ByKind(facts.KindService) {
		if strings.EqualFold(svc.Name, term) {
			return svc.Name, nil, nil
		}
	}

	// Multiple matches: rank by relevance score and judge how decisively the top
	// candidate wins.
	ranked := rankCandidates(results, sq)
	confidence := pickConfidence(ranked, sq.Term)
	top := ranked[0].Name

	// In multi-repo mode, if the top-tier matches span 2+ repos and no repo:
	// scope was given, refuse to silently guess the user's repo — surface the
	// candidates (which carry their repo) so the caller can pin it down.
	if s.crossRepoAmbiguous(ranked, sq) {
		return "", &nameResolution{
			Query:        query,
			Alternatives: candidateNames(results, ""),
			Candidates:   topCandidates(ranked),
			Confidence:   confidence,
			Ambiguous:    true,
		}, nil
	}

	// One candidate clearly dominates (e.g. a unique suffix-exact name among
	// substring matches). Auto-resolve to it, but surface the resolution with its
	// confidence and the alternatives so the caller can see — and override — the
	// pick rather than it being silent.
	if confidence > autoPickConfidence {
		return top, &nameResolution{
			Query:        query,
			Matched:      top,
			Alternatives: candidateNames(results, top),
			Candidates:   topCandidates(ranked),
			Confidence:   confidence,
			AutoPicked:   true,
			Ambiguous:    true,
		}, nil
	}

	// Below the ambiguity threshold, return the best guess (flagged ambiguous),
	// preserving the long-standing "small ambiguity → pick anyway" behavior.
	if len(results) < ambiguousMatchThreshold {
		return top, &nameResolution{
			Query:        query,
			Matched:      top,
			Alternatives: candidateNames(results, top),
			Candidates:   topCandidates(ranked),
			Confidence:   confidence,
			Ambiguous:    true,
		}, nil
	}

	// Too ambiguous to guess: surface ranked candidates and refuse to pick.
	return "", &nameResolution{
		Query:        query,
		Alternatives: candidateNames(results, ""),
		Candidates:   topCandidates(ranked),
		Confidence:   confidence,
		Ambiguous:    true,
	}, nil
}

// gatherCandidates returns the facts matching a (possibly scoped) query. Unscoped
// inputs use the legacy substring Query to preserve existing semantics. Scoped
// inputs use QueryAdvanced with repo/kind/file-prefix filters (expanding the file
// prefix across repos in multi-repo mode) and an optional symbol_kind post-filter.
func (s *Server) gatherCandidates(store *facts.Store, sq scopedQuery, term string) []facts.Fact {
	scoped := sq.Repo != "" || len(sq.Kinds) > 0 || sq.FilePrefix != "" || sq.SymbolKind != ""
	if !scoped {
		return store.Query("", "", term, "")
	}

	prefixes := []string{""}
	if sq.FilePrefix != "" {
		prefixes = s.expandFilePrefix(sq.FilePrefix)
	}

	seen := make(map[string]struct{})
	var out []facts.Fact
	for _, pfx := range prefixes {
		res, _ := store.QueryAdvanced(facts.QueryOpts{
			Repo:       sq.Repo,
			Kinds:      sq.Kinds,
			FilePrefix: pfx,
			Name:       term,
			Limit:      500,
		})
		for _, f := range res {
			if sq.SymbolKind != "" {
				if sk, _ := f.Props["symbol_kind"].(string); sk != sq.SymbolKind {
					continue
				}
			}
			if _, dup := seen[f.Name]; dup {
				continue
			}
			seen[f.Name] = struct{}{}
			out = append(out, f)
		}
	}
	return out
}

// topCandidates returns up to maxCandidates ranked candidates.
func topCandidates(ranked []scoredCandidate) []scoredCandidate {
	if len(ranked) > maxCandidates {
		return ranked[:maxCandidates]
	}
	return ranked
}

// resolveRepoLabelToServiceNode maps a user-supplied label to the corresponding
// KindService fact name. Handles appended repos (label in RepoPaths) and the
// primary repo (base name of Snapshot.Meta.RepoPath), whose service node has
// Repo == "" and Name == "".
func (s *Server) resolveRepoLabelToServiceNode(store *facts.Store, input string) (string, bool) {
	if s.eng == nil {
		return "", false
	}
	services := store.ByKind(facts.KindService)
	inputLower := strings.ToLower(input)

	for label := range s.eng.RepoPaths() {
		if strings.ToLower(label) == inputLower {
			for _, svc := range services {
				if strings.ToLower(svc.Repo) == inputLower || strings.ToLower(svc.Name) == inputLower {
					return svc.Name, true
				}
			}
		}
	}

	if snap := s.eng.Snapshot(); snap != nil {
		if strings.ToLower(filepath.Base(snap.Meta.RepoPath)) == inputLower {
			for _, svc := range services {
				if svc.Repo == "" {
					return svc.Name, true
				}
			}
		}
	}

	return "", false
}

// maybePrefixRepoLabel rewrites "<repo> <term>" → "repo:<repo> <term>" when the
// first whitespace token is exactly a known repo label and a remainder follows.
// It is a no-op in single-repo mode (RepoPaths is nil), when the input has no
// remainder, or when the first token is already a scope token. This lets a bare
// "go-auth AuthHandler" resolve the same as "repo:go-auth AuthHandler".
func (s *Server) maybePrefixRepoLabel(input string) string {
	if s.eng == nil {
		return input
	}
	fields := strings.Fields(input)
	if len(fields) < 2 {
		return input
	}
	if _, _, ok := splitScopeToken(fields[0]); ok {
		return input // already scoped
	}
	if paths := s.eng.RepoPaths(); paths[fields[0]] != "" {
		return "repo:" + input
	}
	return input
}

// crossRepoAmbiguous reports whether, in multi-repo mode with no repo: scope, the
// candidates sharing the top match tier span 2+ repos — meaning auto-picking one
// would silently guess the user's repo. A unique top-tier match (e.g. a single
// suffix-exact name in one repo) is NOT cross-repo ambiguous and still resolves.
func (s *Server) crossRepoAmbiguous(ranked []scoredCandidate, sq scopedQuery) bool {
	if sq.Repo != "" || len(ranked) < 2 || s.eng == nil || len(s.eng.RepoPaths()) < 2 {
		return false
	}
	term := strings.ToLower(sq.Term)
	topTier := matchTier(ranked[0].Name, term)
	repos := make(map[string]struct{})
	for _, c := range ranked {
		if matchTier(c.Name, term) != topTier {
			break // ranked is tier-sorted; stop at the first lower tier
		}
		repos[c.Repo] = struct{}{}
	}
	return len(repos) >= 2
}

// suggestNames does a relaxed substring search to recover near-misses when an
// input matched nothing exactly. It searches on the longest alphanumeric run of
// the term (and its last dotted segment) and returns up to 5 fact names,
// repo-qualified in multi-repo mode, so a no-match error is never a dead end.
func (s *Server) suggestNames(store *facts.Store, sq scopedQuery, term string) []string {
	probe := longestAlnumRun(term)
	if seg := lastSegment(term); len(seg) > len(probe) {
		probe = longestAlnumRun(seg)
	}
	if len(probe) < 3 {
		return nil
	}
	matches, _ := store.QueryAdvanced(facts.QueryOpts{Name: probe, Repo: sq.Repo, Limit: 50})
	multiRepo := s.eng != nil && len(s.eng.RepoPaths()) > 1
	seen := make(map[string]struct{})
	out := make([]string, 0, 5)
	for _, m := range matches {
		name := m.Name
		if multiRepo && m.Repo != "" {
			name = "repo:" + m.Repo + " " + m.Name
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
		if len(out) >= 5 {
			break
		}
	}
	return out
}

// resolveByFileBasename maps a file-basename term to the symbols declared in the
// matching file(s). Enola does not name a fact after a source file, so a target
// like "auth_routes" (the file auth_routes.go) is otherwise unresolvable for
// graph tools; this returns the pathable symbol nodes that live in that file.
// Honors a repo: scope when present. Returns distinct symbol facts.
func (s *Server) resolveByFileBasename(store *facts.Store, sq scopedQuery, term string) []facts.Fact {
	lt := strings.ToLower(term)
	seen := make(map[string]struct{})
	var out []facts.Fact
	for _, f := range store.ByKind(facts.KindSymbol) {
		if f.File == "" || !fileBaseMatches(f.File, lt) {
			continue
		}
		if sq.Repo != "" && !strings.EqualFold(f.Repo, sq.Repo) {
			continue
		}
		if _, dup := seen[f.Name]; dup {
			continue
		}
		seen[f.Name] = struct{}{}
		out = append(out, f)
	}
	return out
}

// fileBaseMatches reports whether lowerTerm equals path's basename, with or
// without a trailing source extension (case-insensitive). "a/b/auth_routes.go"
// matches both "auth_routes.go" and "auth_routes"; it never matches a bare
// extension like "go".
func fileBaseMatches(path, lowerTerm string) bool {
	base := strings.ToLower(path)
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if base == lowerTerm {
		return true
	}
	if i := strings.LastIndexByte(base, '.'); i > 0 && codeExtensions[base[i+1:]] {
		return base[:i] == lowerTerm
	}
	return false
}

// rankedCandidatesFor returns the facts matching an input (after repo-prefix
// auto-detection and scope parsing), ranked most-relevant first. Used by find_path
// to consider more candidates than the capped set carried in a nameResolution.
func (s *Server) rankedCandidatesFor(store *facts.Store, input string) []scoredCandidate {
	input = s.maybePrefixRepoLabel(input)
	sq := parseScopedQuery(input)
	term := s.normalizeToRelative(sq.Term)
	return rankCandidates(s.gatherCandidates(store, sq, term), sq)
}

// pathCandidates returns the ranked node names find_path should try for one
// endpoint, most-likely first and capped at maxPathCandidates. It leads with the
// resolved name (preserving resolveNodeName's exact/service/file-basename
// fallbacks), then any candidates from the resolution, then broadens with a fresh
// ranking so a heavily-ambiguous name (10+ matches) is not limited to the few
// candidates echoed in the resolution object.
func (s *Server) pathCandidates(store *facts.Store, input, resolved string, res *nameResolution) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, maxPathCandidates)
	add := func(n string) {
		if n == "" || len(out) >= maxPathCandidates {
			return
		}
		if _, dup := seen[n]; dup {
			return
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	add(resolved)
	if res != nil {
		for _, c := range res.Candidates {
			add(c.Name)
		}
	}
	for _, c := range s.rankedCandidatesFor(store, input) {
		add(c.Name)
	}
	return out
}

// firstOr returns the first element of s, or fallback when s is empty.
func firstOr(s []string, fallback string) string {
	if len(s) == 0 {
		return fallback
	}
	return s[0]
}

// bestPath tries to connect any from-candidate to any to-candidate, expanding each
// to-candidate with RollupSeeds (a path to a type usually ends at one of its
// methods/constructor). It scans in ranked order, keeps the shortest path found,
// and is bounded by maxPathAttempts (returning early on a ≤2-hop hit). When no
// path exists it returns a not-found PathResult naming the first from/to tried.
func (s *Server) bestPath(graph *facts.Graph, fromCands, toCands []string, relKinds []string, maxDepth int) facts.PathResult {
	// Self-defensive: callers are expected to pass non-empty candidate slices, but
	// guard so an empty slice yields a clean not-found result instead of indexing
	// fromCands[0]/toCands[0] out of range.
	if len(fromCands) == 0 || len(toCands) == 0 {
		return facts.PathResult{From: firstOr(fromCands, ""), To: firstOr(toCands, ""), Found: false}
	}
	var best facts.PathResult
	attempts := 0
	for _, from := range fromCands {
		for _, to := range toCands {
			for _, target := range graph.RollupSeeds(to) {
				if attempts >= maxPathAttempts {
					if best.Found {
						return best
					}
					return facts.PathResult{From: fromCands[0], To: toCands[0], Found: false}
				}
				attempts++
				r := graph.FindPath(from, target, relKinds, maxDepth)
				if !r.Found {
					continue
				}
				if !best.Found || len(r.Path) < len(best.Path) {
					best = r
				}
				if len(best.Path) <= 2 { // direct or one-hop: good enough
					return best
				}
			}
		}
	}
	if best.Found {
		return best
	}
	return facts.PathResult{From: fromCands[0], To: toCands[0], Found: false}
}

// effectiveMaxDepth mirrors graph.FindPath's clamping (0 → 10, cap 20) so the
// not-found note reports the depth actually searched.
func effectiveMaxDepth(maxDepth int) int {
	if maxDepth <= 0 {
		return 10
	}
	if maxDepth > 20 {
		return 20
	}
	return maxDepth
}

// longestAlnumRun returns the longest maximal run of [A-Za-z0-9_] in s. Used to
// derive a robust substring probe from a dotted/spaced term.
func longestAlnumRun(s string) string {
	best, start := "", -1
	flush := func(end int) {
		if start >= 0 && end-start > len(best) {
			best = s[start:end]
		}
		start = -1
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			if start < 0 {
				start = i
			}
		} else {
			flush(i)
		}
	}
	flush(len(s))
	return best
}

// candidateNames collects up to maxAlternatives fact names from results,
// preserving store order and excluding exclude (the chosen match) when set.
func candidateNames(results []facts.Fact, exclude string) []string {
	names := make([]string, 0, len(results))
	for _, r := range results {
		if r.Name == exclude {
			continue
		}
		names = append(names, r.Name)
		if len(names) >= maxAlternatives {
			break
		}
	}
	return names
}

// exploreArgs are the arguments for the explore tool.
type exploreArgs struct {
	Focus      string `json:"focus" jsonschema:"required,Module name, file path, or symbol name to explore"`
	Depth      int    `json:"depth,omitempty" jsonschema:"How deep to follow relations (1=direct only, 2=include relations of relations). Default 1, max 2."`
	OutputMode string `json:"output_mode,omitempty" jsonschema:"At depth=2: 'summary' (default) returns aggregated Insights (dependency hotspots, cycle/layer warnings, size metrics); 'compact'/'full' instead list per-symbol relations. Ignored at depth=1."`
	MaxTokens  int    `json:"max_tokens,omitempty" jsonschema:"Optional hard cap on output size (approx tokens). Output is truncated on a line boundary with a notice. Default: no cap."`
}

// traverseArgs are the arguments for the traverse tool.
type traverseArgs struct {
	Start         string   `json:"start" jsonschema:"required,Starting node name (fact name, module name, or symbol name). Substring match; supports scoped prefixes repo:/kind:/file: to disambiguate (e.g. 'repo:go-auth kind:struct AuthHandler')."`
	Direction     string   `json:"direction,omitempty" jsonschema:"'forward' follows outgoing relations (what does X depend on?), 'reverse' follows incoming relations (what depends on X?). Default: forward."`
	RelationKinds []string `json:"relation_kinds,omitempty" jsonschema:"Filter to specific relation types: imports, calls, declares, implements, depends_on, has_method. Default: all."`
	MaxDepth      int      `json:"max_depth,omitempty" jsonschema:"Maximum traversal depth (1-20). Default: 5."`
	MaxNodes      int      `json:"max_nodes,omitempty" jsonschema:"Maximum nodes to return (1-500). Traversal stops when this limit is reached. Default: 100."`
	NodeKinds     []string `json:"node_kinds,omitempty" jsonschema:"Filter results to specific fact kinds: module, symbol, dependency, route, storage. Default: all."`
	OutputMode    string   `json:"output_mode,omitempty" jsonschema:"Verbosity ladder: 'summary' (DEFAULT — aggregated counts by node/relation kind, internal/external split, hottest modules; smallest) → 'compact' (per-node markdown grouped by depth) → 'full' (raw JSON node/edge graph; can be VERY large). Start with summary; escalate only when you need node-level detail."`
	MaxTokens     int      `json:"max_tokens,omitempty" jsonschema:"Optional hard cap on output size (approx tokens). Output is truncated on a line boundary with a notice telling you to narrow. Default: no cap."`
}

// findPathArgs are the arguments for the find_path tool.
type findPathArgs struct {
	From          string   `json:"from" jsonschema:"required,Source node name. Substring match; supports scoped prefixes repo:/kind:/file: to disambiguate (e.g. 'repo:go-auth Login')."`
	To            string   `json:"to" jsonschema:"required,Target node name. Substring match; supports scoped prefixes repo:/kind:/file: to disambiguate (e.g. 'kind:struct AuthMiddleware')."`
	RelationKinds []string `json:"relation_kinds,omitempty" jsonschema:"Filter to specific relation types. Default: all."`
	MaxDepth      int      `json:"max_depth,omitempty" jsonschema:"Maximum path length to search (1-20). Default: 10."`
}

// impactAnalysisArgs are the arguments for the impact_analysis tool.
type endpointImpactArgs struct {
	Endpoint  string `json:"endpoint" jsonschema:"required,The HTTP endpoint being changed. Substring match on the path, optionally prefixed with a verb: 'GET /v1/candidates' or just '/v1/candidates'."`
	MaxRoutes int    `json:"max_routes,omitempty" jsonschema:"How many matched endpoints to follow (1-200). A bare prefix can match hundreds. Default: 25."`
}

type impactAnalysisArgs struct {
	Target         string `json:"target" jsonschema:"required,The node being changed (fact name, substring match). Supports scoped prefixes repo:/kind:/file: to disambiguate."`
	MaxDepth       int    `json:"max_depth,omitempty" jsonschema:"How many hops of impact to compute (1-10). Default: 3."`
	MaxNodes       int    `json:"max_nodes,omitempty" jsonschema:"Maximum impacted nodes to return (1-500). Default: 200."`
	IncludeForward bool   `json:"include_forward,omitempty" jsonschema:"Include what the target depends on (what might break the target). Default: false."`
	OutputMode     string `json:"output_mode,omitempty" jsonschema:"Verbosity ladder: 'summary' (DEFAULT — total dependents, breakdown by kind/depth, hotspot modules, cross-repo reach, relevant cycle/layer insights; smallest) → 'compact' (per-depth dependent list) → 'full' (raw JSON by_depth/edges graph; can be VERY large). Start with summary; escalate only when you need node-level detail."`
	MaxTokens      int    `json:"max_tokens,omitempty" jsonschema:"Optional hard cap on output size (approx tokens). Output is truncated on a line boundary with a notice telling you to narrow. Default: no cap."`
}

// exploreModule renders a module exploration if the focus matches a module name.
func (s *Server) exploreModule(store *facts.Store, focus string, depth int, mode string, sb *strings.Builder) bool {
	modules := store.LookupByExactName(focus)
	// Filter to only module-kind facts
	var mod *facts.Fact
	for i := range modules {
		if modules[i].Kind == facts.KindModule {
			mod = &modules[i]
			break
		}
	}
	if mod == nil {
		return false
	}

	sb.WriteString(fmt.Sprintf("# Module: %s\n\n", mod.Name))

	// Props summary
	if lang, ok := mod.Props["language"].(string); ok {
		sb.WriteString(fmt.Sprintf("- Language: %s\n", lang))
	}
	if pkg, ok := mod.Props["package"].(string); ok {
		sb.WriteString(fmt.Sprintf("- Package: %s\n", pkg))
	}
	sb.WriteString("\n")

	// Find symbols declared in this module (symbols whose "declares" relation targets this module)
	declaredSymbols := store.ReverseLookup(mod.Name, facts.RelDeclares)
	if len(declaredSymbols) > 0 {
		sb.WriteString(fmt.Sprintf("## Symbols (%d)\n\n", len(declaredSymbols)))
		sb.WriteString("| Name | Kind | File | Line | Exported |\n")
		sb.WriteString("|------|------|------|------|----------|\n")
		for _, sym := range declaredSymbols {
			symKind, _ := sym.Props["symbol_kind"].(string)
			exported := "no"
			if exp, ok := sym.Props["exported"].(bool); ok && exp {
				exported = "yes"
			}
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %d | %s |\n",
				sym.Name, symKind, sym.File, sym.Line, exported))
		}
		sb.WriteString("\n")
	}

	// Dependencies: facts with kind=dependency whose file starts with the module path,
	// plus direct depends_on relations from the module fact itself (packwerk).
	deps, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindDependency, FilePrefix: mod.Name + "/"})
	// Collect all dependency targets grouped by relation kind.
	depsByKind := make(map[string][]string) // relKind → targets
	seen := make(map[string]struct{})
	for _, dep := range deps {
		for _, r := range dep.Relations {
			key := r.Kind + ":" + r.Target
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			depsByKind[r.Kind] = append(depsByKind[r.Kind], r.Target)
		}
	}
	// Also include the module's own depends_on relations (from packwerk).
	for _, r := range mod.Relations {
		key := r.Kind + ":" + r.Target
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		depsByKind[r.Kind] = append(depsByKind[r.Kind], r.Target)
	}
	totalDeps := 0
	for _, targets := range depsByKind {
		totalDeps += len(targets)
	}
	if totalDeps > 0 {
		sb.WriteString(fmt.Sprintf("## Dependencies (%d)\n\n", totalDeps))
		for _, relKind := range []string{facts.RelDependsOn, facts.RelImports, facts.RelImplements} {
			targets := depsByKind[relKind]
			if len(targets) == 0 {
				continue
			}
			sb.WriteString(fmt.Sprintf("### %s (%d)\n\n", capitalize(relKind), len(targets)))
			for _, t := range targets {
				sb.WriteString(fmt.Sprintf("- %s\n", t))
			}
			sb.WriteString("\n")
		}
	}

	// Reverse dependencies: who depends on or imports this module
	dependents := store.ReverseLookup(mod.Name, facts.RelImports)
	revDeps := store.ReverseLookup(mod.Name, facts.RelDependsOn)
	allDependents := append(dependents, revDeps...)
	if len(allDependents) > 0 {
		depSeen := make(map[string]struct{})
		sb.WriteString(fmt.Sprintf("## Dependents (%d)\n\n", len(allDependents)))
		for _, dep := range allDependents {
			if _, dup := depSeen[dep.Name]; dup {
				continue
			}
			depSeen[dep.Name] = struct{}{}
			sb.WriteString(fmt.Sprintf("- %s\n", dep.Name))
		}
		sb.WriteString("\n")
	}

	// Nested subtree: modules and symbols beneath this directory. TypeScript/JS
	// (and any per-directory module language) nests modules per directory, so a
	// directory that is itself a module often has a large subtree of child
	// modules whose symbols would otherwise be hidden by this exact-module match.
	s.writeNestedModules(store, mod.Name, len(declaredSymbols), sb)

	// At depth=2: the default 'summary' mode emits an aggregated Insights section
	// (what is architecturally significant); compact/full keep the raw per-symbol
	// relations dump.
	if depth >= 2 {
		if mode == modeCompact || mode == modeFull {
			s.writeSymbolRelations(declaredSymbols, sb)
		} else {
			s.writeModuleInsights(mod.Name, declaredSymbols, depsByKind, allDependents, sb)
		}
	}

	return true
}

// writeSymbolRelations writes the raw per-symbol relation dump (depth=2 detail
// for explore in compact/full mode).
func (s *Server) writeSymbolRelations(declaredSymbols []facts.Fact, sb *strings.Builder) {
	if len(declaredSymbols) == 0 {
		return
	}
	sb.WriteString("## Symbol Relations\n\n")
	limit := len(declaredSymbols)
	if limit > 20 {
		limit = 20
	}
	for _, sym := range declaredSymbols[:limit] {
		if len(sym.Relations) <= 1 {
			continue // skip symbols with only a "declares" relation
		}
		fmt.Fprintf(sb, "**%s**\n", sym.Name)
		for _, r := range sym.Relations {
			if r.Kind == facts.RelDeclares {
				continue
			}
			fmt.Fprintf(sb, "  - %s → %s\n", r.Kind, r.Target)
		}
		sb.WriteString("\n")
	}
}

// writeModuleInsights writes the aggregated depth=2 Insights for a module:
// cross-module dependency hotspots, architectural warnings (cycles / layer
// violations from the explainers), and size metrics. This replaces the raw
// symbol-relations dump in the default summary view.
func (s *Server) writeModuleInsights(modName string, declaredSymbols []facts.Fact, depsByKind map[string][]string, allDependents []facts.Fact, sb *strings.Builder) {
	sb.WriteString("## Insights\n\n")

	// Cross-module dependency hotspots: which modules this one depends on most,
	// and which modules depend on it most.
	outByModule := map[string]int{}
	for _, targets := range depsByKind {
		for _, t := range targets {
			outByModule[moduleForName(t)]++
		}
	}
	if len(outByModule) > 0 {
		sb.WriteString("### Depends most on\n\n")
		for _, m := range topCounts(outByModule, 5) {
			fmt.Fprintf(sb, "- %s (%d)\n", m, outByModule[m])
		}
		sb.WriteString("\n")
	}

	inByModule := map[string]int{}
	depSeen := map[string]struct{}{}
	for _, d := range allDependents {
		if _, dup := depSeen[d.Name]; dup {
			continue
		}
		depSeen[d.Name] = struct{}{}
		inByModule[moduleForName(d.Name)]++
	}
	if len(inByModule) > 0 {
		sb.WriteString("### Most depended on by\n\n")
		for _, m := range topCounts(inByModule, 5) {
			fmt.Fprintf(sb, "- %s (%d)\n", m, inByModule[m])
		}
		sb.WriteString("\n")
	}

	// Architectural warnings already computed by the explainers (cycles, layers).
	writeInsightList(sb, "Architectural warnings", s.insightsFor(modName))

	// Size metrics.
	totalDeps := 0
	for _, targets := range depsByKind {
		totalDeps += len(targets)
	}
	fmt.Fprintf(sb, "### Size metrics\n\n- Declared symbols: %d\n- Outgoing dependencies: %d\n- Dependents: %d\n\n",
		len(declaredSymbols), totalDeps, len(depSeen))
	sb.WriteString("_Use output_mode=compact or full for the per-symbol relation list._\n")
}

// moduleForName derives a module/package key from a fact's dotted or slashed name
// for aggregating insights by module.
func moduleForName(name string) string {
	if i := strings.LastIndexByte(name, '/'); i > 0 {
		return name[:i]
	}
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

// writeNestedModules appends a summary of the modules and symbols nested beneath
// modName (i.e. facts whose file path is under "<modName>/"). It lets a directory
// that is itself a module surface its descendant modules and an aggregate symbol
// count, instead of stopping at the module's own directly-declared symbols.
func (s *Server) writeNestedModules(store *facts.Store, modName string, directSymbols int, sb *strings.Builder) {
	prefix := modName + "/"

	// Accurate totals (QueryAdvanced returns the pre-limit total). Symbols declared
	// directly in this module also live under "<modName>/" (e.g. src/app/layout.tsx),
	// so subtract them to count only symbols in nested child modules.
	_, symTotal := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindSymbol, FilePrefix: prefix, Limit: 1})
	nestedSymbols := symTotal - directSymbols
	if nestedSymbols < 0 {
		nestedSymbols = 0
	}
	modFacts, modTotal := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindModule, FilePrefix: prefix, Limit: 500})
	if modTotal == 0 && nestedSymbols == 0 {
		return
	}

	// Group descendant modules by their immediate child segment under modName,
	// counting how many modules live under each child.
	childCounts := make(map[string]int)
	for _, m := range modFacts {
		rest := strings.TrimPrefix(m.Name, prefix)
		seg := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seg = rest[:i]
		}
		if seg == "" {
			continue
		}
		childCounts[seg]++
	}

	children := make([]string, 0, len(childCounts))
	for seg := range childCounts {
		children = append(children, seg)
	}
	sort.Strings(children)

	sb.WriteString(fmt.Sprintf("## Nested modules (%d)\n\n", len(children)))
	sb.WriteString(fmt.Sprintf("Subtree: %d modules, %d symbols\n\n", modTotal, nestedSymbols))

	const maxChildren = 50
	shown := children
	if len(shown) > maxChildren {
		shown = shown[:maxChildren]
	}
	for _, seg := range shown {
		full := prefix + seg
		if n := childCounts[seg]; n > 1 {
			sb.WriteString(fmt.Sprintf("- %s (%d modules)\n", full, n))
		} else {
			sb.WriteString(fmt.Sprintf("- %s\n", full))
		}
	}
	if len(children) > maxChildren {
		sb.WriteString(fmt.Sprintf("\n... and %d more nested modules\n", len(children)-maxChildren))
	}
	sb.WriteString("\nUse explore on a nested module above to drill in.\n\n")
}

// maxModuleSubstringList caps how many candidate modules a non-delegated substring
// match lists, so a broad term (e.g. "golf" across several repos) yields a compact,
// actionable summary instead of dumping hundreds of lines.
const maxModuleSubstringList = 15

// exploreModuleSubstring tries substring matching on module names when exact
// module match fails. It ranks matches (exact > suffix-exact > prefix > substring)
// and, when a single match clearly dominates, delegates to the full exploreModule
// rendering. Otherwise it renders a bounded, repo-grouped summary so a broad term
// does not dump 100 raw module lines; the caller is told how to narrow it.
func (s *Server) exploreModuleSubstring(store *facts.Store, focus string, depth int, mode string, sb *strings.Builder) bool {
	matches, total := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindModule, Name: focus, Limit: 500})
	if len(matches) == 0 {
		return false
	}

	term := strings.ToLower(focus)
	tier := func(name string) int {
		n := strings.ToLower(name)
		switch {
		case n == term:
			return 3
		case hasShortName(n, term):
			return 2
		case strings.HasPrefix(n, term):
			return 1
		default:
			return 0
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		ti, tj := tier(matches[i].Name), tier(matches[j].Name)
		if ti != tj {
			return ti > tj
		}
		return matches[i].Name < matches[j].Name
	})

	// A single match, or a unique exact/suffix-exact top match, is unambiguous —
	// drill straight in.
	if len(matches) == 1 || (tier(matches[0].Name) >= 2 && tier(matches[1].Name) < tier(matches[0].Name)) {
		return s.exploreModule(store, matches[0].Name, depth, mode, sb)
	}

	// Multiple plausible matches — summarize, grouped by repo, capped.
	shown := matches
	if len(shown) > maxModuleSubstringList {
		shown = shown[:maxModuleSubstringList]
	}
	fmt.Fprintf(sb, "# Modules matching %q (showing %d of %d)\n\n", focus, len(shown), total)

	byRepo := make(map[string]int)
	repoOrder := make([]string, 0)
	for _, m := range matches {
		r := m.Repo
		if r == "" {
			r = "(primary)"
		}
		if _, ok := byRepo[r]; !ok {
			repoOrder = append(repoOrder, r)
		}
		byRepo[r]++
	}
	if len(repoOrder) > 1 {
		sort.Strings(repoOrder)
		sb.WriteString("By repo: ")
		parts := make([]string, 0, len(repoOrder))
		for _, r := range repoOrder {
			parts = append(parts, fmt.Sprintf("%s (%d)", r, byRepo[r]))
		}
		sb.WriteString(strings.Join(parts, ", "))
		sb.WriteString("\n\n")
	}

	for _, m := range shown {
		if m.Repo != "" {
			fmt.Fprintf(sb, "- `%s` (repo:%s)\n", m.Name, m.Repo)
		} else {
			fmt.Fprintf(sb, "- `%s`\n", m.Name)
		}
	}
	sb.WriteString("\nNarrow with a `repo:<label>` scope, a full module name, or a more specific substring.\n")
	return true
}

// exploreFile renders a file exploration if the focus matches an exact file path.
// In multi-repo mode, it also tries repo-label prefixed paths and common extensions.
func (s *Server) exploreFile(store *facts.Store, focus string, depth int, sb *strings.Builder) bool {
	fileFacts := store.ByFile(focus)

	// In multi-repo mode, try repo-label prefixed paths.
	if len(fileFacts) == 0 {
		for _, label := range s.repoLabels() {
			fileFacts = store.ByFile(label + "/" + focus)
			if len(fileFacts) > 0 {
				focus = label + "/" + focus
				break
			}
		}
	}

	// Try appending common extensions (with and without repo labels).
	if len(fileFacts) == 0 {
		extensions := []string{".go", ".ts", ".tsx", ".kt", ".swift", ".rb"}
		candidates := make([]string, 0, len(extensions)*(1+len(s.repoLabels())))
		for _, ext := range extensions {
			candidates = append(candidates, focus+ext)
			for _, label := range s.repoLabels() {
				candidates = append(candidates, label+"/"+focus+ext)
			}
		}
		for _, c := range candidates {
			fileFacts = store.ByFile(c)
			if len(fileFacts) > 0 {
				focus = c
				break
			}
		}
	}

	if len(fileFacts) == 0 {
		return false
	}

	sb.WriteString(fmt.Sprintf("# File: %s\n\n", focus))
	sb.WriteString(fmt.Sprintf("Total facts: %d\n\n", len(fileFacts)))

	// Group by kind
	byKind := make(map[string][]facts.Fact)
	for _, f := range fileFacts {
		byKind[f.Kind] = append(byKind[f.Kind], f)
	}

	for _, kind := range []string{facts.KindModule, facts.KindSymbol, facts.KindDependency, facts.KindRoute, facts.KindStorage} {
		ff := byKind[kind]
		if len(ff) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("## %ss (%d)\n\n", capitalize(kind), len(ff)))
		for _, f := range ff {
			sb.WriteString(fmt.Sprintf("- **%s**", f.Name))
			if f.Line > 0 {
				sb.WriteString(fmt.Sprintf(" (line %d)", f.Line))
			}
			if sk, ok := f.Props["symbol_kind"].(string); ok {
				sb.WriteString(fmt.Sprintf(" [%s]", sk))
			}
			sb.WriteString("\n")
			if depth >= 2 {
				for _, r := range f.Relations {
					sb.WriteString(fmt.Sprintf("  - %s → %s\n", r.Kind, r.Target))
				}
			}
		}
		sb.WriteString("\n")
	}

	return true
}

// exploreSymbol renders a symbol exploration if the focus matches symbol names via substring.
func (s *Server) exploreSymbol(store *facts.Store, focus string, depth int, sb *strings.Builder) bool {
	results := store.Query(facts.KindSymbol, "", focus, "")
	if len(results) == 0 {
		return false
	}

	if len(results) > 10 {
		results = results[:10]
	}

	sb.WriteString(fmt.Sprintf("# Symbol: %s\n\n", focus))

	for i, sym := range results {
		if i > 0 {
			sb.WriteString("---\n\n")
		}

		sb.WriteString(fmt.Sprintf("## %s\n\n", sym.Name))
		sb.WriteString(fmt.Sprintf("- File: %s\n", sym.File))
		sb.WriteString(fmt.Sprintf("- Line: %d\n", sym.Line))
		if sk, ok := sym.Props["symbol_kind"].(string); ok {
			sb.WriteString(fmt.Sprintf("- Kind: %s\n", sk))
		}
		if lang, ok := sym.Props["language"].(string); ok {
			sb.WriteString(fmt.Sprintf("- Language: %s\n", lang))
		}
		if exp, ok := sym.Props["exported"].(bool); ok {
			sb.WriteString(fmt.Sprintf("- Exported: %v\n", exp))
		}
		sb.WriteString("\n")

		// Relations
		if len(sym.Relations) > 0 {
			sb.WriteString("### Relations\n\n")
			for _, r := range sym.Relations {
				sb.WriteString(fmt.Sprintf("- %s → %s\n", r.Kind, r.Target))
			}
			sb.WriteString("\n")
		}

		// Resolve relation targets (depth >= 1)
		if depth >= 1 && len(sym.Relations) > 0 {
			sb.WriteString("### Related Facts\n\n")
			seen := make(map[string]struct{})
			for _, r := range sym.Relations {
				if _, dup := seen[r.Target]; dup {
					continue
				}
				seen[r.Target] = struct{}{}
				related := store.LookupByExactName(r.Target)
				for _, rf := range related {
					sb.WriteString(fmt.Sprintf("- **%s** (%s) — %s", rf.Name, rf.Kind, rf.File))
					if rf.Line > 0 {
						sb.WriteString(fmt.Sprintf(":%d", rf.Line))
					}
					sb.WriteString("\n")
				}
			}
			sb.WriteString("\n")
		}

		// Reverse relations: who calls/imports/depends on this symbol
		callers := store.ReverseLookup(sym.Name, "")
		if len(callers) > 0 {
			sb.WriteString("### Referenced By\n\n")
			limit := len(callers)
			if limit > 20 {
				limit = 20
			}
			for _, c := range callers[:limit] {
				for _, r := range c.Relations {
					if r.Target == sym.Name {
						sb.WriteString(fmt.Sprintf("- %s (%s)\n", c.Name, r.Kind))
						break
					}
				}
			}
			if len(callers) > 20 {
				sb.WriteString(fmt.Sprintf("- ... and %d more\n", len(callers)-20))
			}
			sb.WriteString("\n")
		}
	}

	return true
}

// exploreDirectory renders a directory summary if the focus matches a file prefix.
func (s *Server) exploreDirectory(store *facts.Store, focus string, sb *strings.Builder) bool {
	prefix := focus
	if prefix == "." {
		// "." means repo root — match all files (no prefix filter).
		prefix = ""
	} else if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	// In multi-repo mode, expand bare prefixes to include repo labels.
	prefixes := s.expandFilePrefix(prefix)
	dirFacts, total := store.QueryAdvanced(facts.QueryOpts{FilePrefix: prefixes[0], Limit: 500})
	for _, p := range prefixes[1:] {
		extra, extraTotal := store.QueryAdvanced(facts.QueryOpts{FilePrefix: p, Limit: 500})
		dirFacts = append(dirFacts, extra...)
		total += extraTotal
	}
	if total == 0 {
		return false
	}

	sb.WriteString(fmt.Sprintf("# Directory: %s\n\n", focus))
	sb.WriteString(fmt.Sprintf("Total facts: %d\n\n", total))

	// Count by kind
	kindCount := make(map[string]int)
	files := make(map[string]struct{})
	for _, f := range dirFacts {
		kindCount[f.Kind]++
		if f.File != "" {
			files[f.File] = struct{}{}
		}
	}

	sb.WriteString("## Summary\n\n")
	sb.WriteString(fmt.Sprintf("- Files: %d\n", len(files)))
	for _, kind := range []string{facts.KindModule, facts.KindSymbol, facts.KindDependency, facts.KindRoute, facts.KindStorage} {
		if c, ok := kindCount[kind]; ok {
			sb.WriteString(fmt.Sprintf("- %ss: %d\n", capitalize(kind), c))
		}
	}
	sb.WriteString("\n")

	// List modules
	var modules []facts.Fact
	var symbols []facts.Fact
	for _, f := range dirFacts {
		switch f.Kind {
		case facts.KindModule:
			modules = append(modules, f)
		case facts.KindSymbol:
			symbols = append(symbols, f)
		}
	}

	if len(modules) > 0 {
		sb.WriteString(fmt.Sprintf("## Modules (%d)\n\n", len(modules)))
		for _, m := range modules {
			sb.WriteString(fmt.Sprintf("- %s\n", m.Name))
		}
		sb.WriteString("\n")
	}

	if len(symbols) > 0 {
		sb.WriteString("## Key Symbols (showing up to 30)\n\n")
		limit := len(symbols)
		if limit > 30 {
			limit = 30
		}
		sb.WriteString("| Name | Kind | File | Line |\n")
		sb.WriteString("|------|------|------|------|\n")
		for _, sym := range symbols[:limit] {
			symKind, _ := sym.Props["symbol_kind"].(string)
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %d |\n",
				sym.Name, symKind, sym.File, sym.Line))
		}
		if len(symbols) > 30 {
			sb.WriteString(fmt.Sprintf("\n... and %d more symbols\n", len(symbols)-30))
		}
		sb.WriteString("\n")
	}

	return true
}

// showSymbolArgs are the arguments for the show_symbol tool.
type showSymbolArgs struct {
	Name         string `json:"name" jsonschema:"required,Symbol name to look up (substring match)"`
	ContextLines int    `json:"context_lines,omitempty" jsonschema:"Number of source lines to show around the symbol (default 60)"`
}

// governingIntentArgs are the arguments for the governing_intent tool.
type governingIntentArgs struct {
	Target    string `json:"target" jsonschema:"required,A fact name (exact match)\\, a file path (label-prefixed or repo-relative)\\, or a compiled page path"`
	Repo      string `json:"repo,omitempty" jsonschema:"Repo label to disambiguate a file path measured in more than one repo"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"Optional hard cap on output size (approx tokens)"`
}

// constraintsForArgs are the arguments for the constraints_for tool.
type constraintsForArgs struct {
	Target    string `json:"target" jsonschema:"required,A file path (repo-relative\\, may not exist yet) or an exact fact name"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"Optional hard cap on output size (approx tokens)"`
}

type planCheckArgs struct {
	Paths     []string `json:"paths,omitempty" jsonschema:"Repo-relative file paths the intended change touches (they may not exist yet)"`
	Symbols   []string `json:"symbols,omitempty" jsonschema:"Exact fact names the intended change touches"`
	Patch     string   `json:"patch,omitempty" jsonschema:"A unified diff to evaluate counterfactually over a scratch copy — the working tree is never touched"`
	MaxTokens int      `json:"max_tokens,omitempty" jsonschema:"Optional hard cap on output size (approx tokens)"`
}

// constraintsForResponse is the constraints_for tool's JSON shape: the
// components containing the target, each with the rules binding it, plus the
// current constraint violations whose evidence names the target.
type constraintsForResponse struct {
	Target     string                         `json:"target"`
	Components []constraints.ComponentBinding `json:"components"`
	Violations []facts.Insight                `json:"violations,omitempty"`
}

// readSourceWindow reads lines from a file around the given line number.
// The window is asymmetric: 1/4 of context before the line, 3/4 after,
// since symbol declarations are at the start of the interesting code.
func readSourceWindow(absFile string, centerLine, contextLines int) (string, error) {
	data, err := os.ReadFile(absFile)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	before := contextLines / 4
	after := contextLines - before
	startLine := centerLine - before
	if startLine < 1 {
		startLine = 1
	}
	endLine := centerLine + after
	if endLine > len(lines) {
		endLine = len(lines)
	}

	var sb strings.Builder
	for i := startLine; i <= endLine; i++ {
		sb.WriteString(fmt.Sprintf("%4d│ %s\n", i, lines[i-1]))
	}
	return sb.String(), nil
}

// capitalize returns s with its first letter uppercased.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// normalizeToRelative converts an absolute filesystem path to a store-relative
// path by stripping known repo root prefixes. If the path is already relative
// or doesn't match any known repo root, it is returned unchanged.
func (s *Server) normalizeToRelative(p string) string {
	if !filepath.IsAbs(p) {
		return p
	}

	// Try multi-repo paths first (populated in append mode).
	for label, absRoot := range s.eng.RepoPaths() {
		rel, err := filepath.Rel(absRoot, p)
		if err == nil && !strings.HasPrefix(rel, "..") {
			// Prefix with repo label so it matches the prefixed fact files.
			return filepath.ToSlash(filepath.Join(label, rel))
		}
	}

	// Fall back to the single-snapshot repo path.
	snap := s.eng.Snapshot()
	if snap != nil {
		rel, err := filepath.Rel(snap.Meta.RepoPath, p)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}

	return p
}

// repoLabels returns the known repo labels from multi-repo mode, or nil.
func (s *Server) repoLabels() []string {
	if s.eng == nil {
		return nil
	}
	rp := s.eng.RepoPaths()
	if len(rp) == 0 {
		return nil
	}
	labels := make([]string, 0, len(rp))
	for l := range rp {
		labels = append(labels, l)
	}
	return labels
}

// expandFilePrefix expands a relative file prefix for multi-repo mode.
// When repoPaths are configured and the prefix doesn't already start with a
// known repo label, it returns all "{label}/{prefix}" variants that have
// matches in the store. If only one repo matches, it returns that single
// expanded prefix. If multiple repos match, it returns all variants.
// In single-repo mode or when the prefix already has a repo label, it returns
// the input unchanged.
func (s *Server) expandFilePrefix(prefix string) []string {
	if prefix == "" || filepath.IsAbs(prefix) || s.eng == nil {
		return []string{prefix}
	}

	repoPaths := s.eng.RepoPaths()
	if len(repoPaths) == 0 {
		return []string{prefix}
	}

	// Check if prefix already starts with a known repo label.
	for label := range repoPaths {
		if prefix == label || strings.HasPrefix(prefix, label+"/") {
			return []string{prefix}
		}
	}

	// Try prefixing with each repo label and check for matches.
	store := s.eng.Store()
	var expanded []string
	for label := range repoPaths {
		candidate := label + "/" + prefix
		// Quick check: does the store have any facts with this file prefix?
		_, total := store.QueryAdvanced(facts.QueryOpts{FilePrefix: candidate, Limit: 1})
		if total > 0 {
			expanded = append(expanded, candidate)
		}
	}

	if len(expanded) == 0 {
		// No matches with any repo label; return original (maybe it matches as-is).
		return []string{prefix}
	}
	return expanded
}

// The MCP result builders and the output-mode/token-cap helpers live in
// pkg/mcputil so tools outside this module share one implementation.
// These file-local names forward to it, keeping the server's many call sites
// unchanged.

func errorResult(msg string) *mcp.CallToolResult { return mcputil.ErrorResult(msg) }

func jsonResult(v any) (*mcp.CallToolResult, any, error) { return mcputil.JSONResult(v) }

func textResult(s string) *mcp.CallToolResult { return mcputil.TextResult(s) }

func jsonResultCapped(v any, maxTokens int) (*mcp.CallToolResult, any, error) {
	return mcputil.JSONResultCapped(v, maxTokens)
}

// Output mode names shared across the tools. summary (smallest) → compact → full
// (largest); "names" is query_facts-only and stays local.
const (
	modeSummary = mcputil.ModeSummary
	modeCompact = mcputil.ModeCompact
	modeFull    = mcputil.ModeFull
	modeNames   = "names"
)

func resolveOutputMode(mode, def string) string { return mcputil.ResolveOutputMode(mode, def) }

// wantsFullOutput reports whether the caller asked for the raw JSON graph rather
// than a markdown summary.
func wantsFullOutput(mode string) bool {
	return strings.EqualFold(mode, modeFull)
}

// wantsSummary reports whether the caller asked for the aggregated summary view.
func wantsSummary(mode string) bool {
	return strings.EqualFold(mode, modeSummary)
}

func capTokens(s string, maxTokens int, isJSON bool) string {
	return mcputil.CapTokens(s, maxTokens, isJSON)
}

// moduleOf returns a node's owning module: its file directory when a file is
// known, otherwise the package-ish prefix of its dotted name. Used to aggregate
// traversal/impact nodes by module in the summary views.
func moduleOf(n facts.TraversalNode) string {
	if n.File != "" {
		if i := strings.LastIndexByte(n.File, '/'); i > 0 {
			return n.File[:i]
		}
		return "."
	}
	name := n.Name
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

// topCounts returns the up-to-n highest-count keys from m, sorted by count
// descending then name ascending for stable output.
func topCounts(m map[string]int, n int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	return keys
}

// insightsFor returns the architectural insights (cycles, layer violations,
// cross-repo, etc.) computed at snapshot time whose title or evidence references
// focus. It lets the live tools surface what the explainers already found instead
// of recomputing it. Returns nil when no snapshot or no matching insights.
func (s *Server) insightsFor(focus string) []facts.Insight {
	if s.eng == nil || focus == "" {
		return nil
	}
	snap := s.eng.Snapshot()
	if snap == nil || len(snap.Insights) == 0 {
		return nil
	}
	var out []facts.Insight
	for _, in := range snap.Insights {
		if strings.Contains(in.Title, focus) || strings.Contains(in.Description, focus) {
			out = append(out, in)
			continue
		}
		for _, ev := range in.Evidence {
			if (ev.Fact != "" && strings.Contains(ev.Fact, focus)) ||
				(ev.File != "" && strings.Contains(ev.File, focus)) ||
				(ev.Symbol != "" && strings.Contains(ev.Symbol, focus)) {
				out = append(out, in)
				break
			}
		}
	}
	return out
}

// writeInsightList appends a compact bullet list of insights (title + one-line
// description) under the given heading. No-op when insights is empty.
func writeInsightList(sb *strings.Builder, heading string, insights []facts.Insight) {
	if len(insights) == 0 {
		return
	}
	fmt.Fprintf(sb, "## %s (%d)\n\n", heading, len(insights))
	for _, in := range insights {
		fmt.Fprintf(sb, "- **%s** — %s\n", in.Title, in.Description)
	}
	sb.WriteString("\n")
}

// compactPerDepthCap bounds how many nodes are listed per depth bucket in the
// compact graph-tool output, keeping responses well under the MCP token budget
// even for high-fan-in nodes. The accurate totals still reflect the full set.
const compactPerDepthCap = 40

// writeResolutionNote renders the name-resolution context (if any) for the
// compact graph-tool output: when a name was ambiguous and refused, it lists the
// candidates so the caller can re-run with an exact name; when it was auto-picked
// it notes the match and alternatives.
func writeResolutionNote(sb *strings.Builder, res *nameResolution) {
	if res == nil {
		return
	}
	if res.Matched == "" {
		fmt.Fprintf(sb, "> ⚠ Ambiguous %q — refine with an exact name or a repo:/kind:/file: scope. Candidates:\n", res.Query)
		writeCandidateList(sb, res)
		sb.WriteString("\n")
		return
	}
	fmt.Fprintf(sb, "> Resolved %q → %s", res.Query, res.Matched)
	if res.Confidence > 0 {
		fmt.Fprintf(sb, " (confidence %.2f)", res.Confidence)
	}
	sb.WriteString("\n")
	if len(res.Alternatives) > 0 {
		fmt.Fprintf(sb, "> alternatives: %s\n", strings.Join(res.Alternatives, ", "))
	}
	sb.WriteString("\n")
}

// writeCandidateList prints a resolution's ranked candidates (falling back to the
// plain alternative names when no scored candidates are present).
func writeCandidateList(sb *strings.Builder, res *nameResolution) {
	if len(res.Candidates) > 0 {
		for _, c := range res.Candidates {
			line := "> - " + c.Name
			if c.Kind != "" {
				line += " (" + c.Kind + ")"
			}
			if c.File != "" {
				line += " — " + c.File
			}
			sb.WriteString(line + "\n")
		}
		return
	}
	for _, a := range res.Alternatives {
		sb.WriteString("> - " + a + "\n")
	}
}

// otherKinds returns the conflated kinds except the one already displayed, so the
// annotation adds information instead of repeating the node's own kind.
func otherKinds(conflated []string, shown string) []string {
	out := make([]string, 0, len(conflated))
	for _, k := range conflated {
		if k != shown {
			out = append(out, k)
		}
	}
	return out
}

// compactNodeLine formats a single traversal node as a markdown list item.
func compactNodeLine(n facts.TraversalNode) string {
	s := "- " + n.Name
	if n.Kind != "" {
		s += " (" + n.Kind + ")"
	}
	if n.Unresolved {
		s += " [unresolved]"
	}
	if len(n.Conflated) > 1 {
		// The node's name is shared by facts of several kinds, so it merges them.
		// Say so rather than letting the single kind above read as precise.
		s += " [also: " + strings.Join(otherKinds(n.Conflated, n.Kind), ", ") + "]"
	}
	if n.File != "" {
		s += " — " + n.File
		if n.Line > 0 {
			s += fmt.Sprintf(":%d", n.Line)
		}
	}
	return s
}

// writeNodesByDepth groups nodes by their depth (skipping depth 0, the origin)
// and writes one capped section per depth.
func writeNodesByDepth(sb *strings.Builder, nodes []facts.TraversalNode) {
	byDepth := map[int][]facts.TraversalNode{}
	for _, n := range nodes {
		if n.Depth > 0 {
			byDepth[n.Depth] = append(byDepth[n.Depth], n)
		}
	}
	depths := make([]int, 0, len(byDepth))
	for d := range byDepth {
		depths = append(depths, d)
	}
	sort.Ints(depths)
	for _, d := range depths {
		group := byDepth[d]
		fmt.Fprintf(sb, "## Depth %d (%d)\n\n", d, len(group))
		shown := group
		if len(shown) > compactPerDepthCap {
			shown = shown[:compactPerDepthCap]
		}
		for _, n := range shown {
			sb.WriteString(compactNodeLine(n) + "\n")
		}
		if len(group) > compactPerDepthCap {
			fmt.Fprintf(sb, "... and %d more at depth %d\n", len(group)-compactPerDepthCap, d)
		}
		sb.WriteString("\n")
	}
}

// renderImpactCompact renders an impact analysis as a compact markdown summary.
// writeGoverningIntent renders the knowledge pages anchored to the target's
// file — the decision trail governing the code whose blast radius is being
// analyzed. Silence when nothing anchors.
func writeGoverningIntent(sb *strings.Builder, pages []facts.GoverningPage) {
	if len(pages) == 0 {
		return
	}
	sb.WriteString("## Governing intent\n\nKnowledge pages anchored to the target's file:\n\n")
	for _, p := range pages {
		sb.WriteString("- " + p.Page + pageMeta(p.Type, p.Status) + "\n")
		for _, r := range p.Relations {
			sb.WriteString("  - " + r.Rel + " " + r.To + pageMeta(r.ToType, r.ToStatus) + "\n")
		}
	}
	sb.WriteString("\n")
}

// pageMeta renders a page's type/status suffix, empty when neither is set.
func pageMeta(pageType, status string) string {
	var meta []string
	if pageType != "" {
		meta = append(meta, pageType)
	}
	if status != "" {
		meta = append(meta, status)
	}
	if len(meta) == 0 {
		return ""
	}
	return " (" + strings.Join(meta, ", ") + ")"
}

// renderGovernedCode renders the forward half of the reverse query: the
// anchors a page declares and the measured coverage under each — a zero is
// visible rather than silent, since an anchor nothing measures is exactly
// what the caller is looking for.
func renderGovernedCode(page string, cov []facts.AnchorCoverage) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "### %s\n\n", page)
	if len(cov) == 0 {
		sb.WriteString("The page compiles but declares no anchors — it governs no code directly.\n")
		return sb.String()
	}
	sb.WriteString("Declared anchors and measured coverage:\n\n")
	for _, c := range cov {
		fmt.Fprintf(&sb, "- %s %s — %d measured fact(s) in %d file(s)\n", c.Repo, c.Path, c.MeasuredFacts, c.MeasuredFiles)
	}
	return sb.String()
}

func renderImpactCompact(resp impactResponse) string {
	var sb strings.Builder
	r := resp.ImpactResult
	fmt.Fprintf(&sb, "# Impact: %s\n\n", r.Target)
	writeResolutionNote(&sb, resp.Resolution)

	// Ambiguous-and-refused: no traversal was run; the candidate list above is
	// the actionable content.
	if resp.Resolution != nil && resp.Resolution.Matched == "" {
		return sb.String()
	}

	if r.Summary != "" {
		sb.WriteString(r.Summary + "\n\n")
	} else {
		sb.WriteString("No dependents found.\n\n")
	}

	writeGoverningIntent(&sb, r.GoverningIntent)

	// Flatten ByDepth into a node slice for grouped rendering.
	var nodes []facts.TraversalNode
	for _, group := range r.ByDepth {
		nodes = append(nodes, group...)
	}
	writeNodesByDepth(&sb, nodes)

	if r.Forward != nil && len(r.Forward.Nodes) > 0 {
		fmt.Fprintf(&sb, "# Forward dependencies of %s (what it depends on)\n\n", r.Target)
		writeNodesByDepth(&sb, r.Forward.Nodes)
	}

	fmt.Fprintf(&sb, "_Stats: %d visited, max depth %d", r.Stats.NodesVisited, r.Stats.MaxDepthReached)
	if r.Stats.Truncated {
		sb.WriteString(", truncated — use max_nodes to widen or output_mode=full for the raw graph")
	}
	sb.WriteString("._\n")
	return sb.String()
}

// renderTraverseCompact renders a graph traversal as a compact markdown summary.
func renderTraverseCompact(resp traverseResponse, start, direction string) string {
	var sb strings.Builder
	if direction == "" {
		direction = "forward"
	}
	fmt.Fprintf(&sb, "# Traverse: %s (%s)\n\n", start, direction)
	writeResolutionNote(&sb, resp.Resolution)

	if resp.Resolution != nil && resp.Resolution.Matched == "" {
		return sb.String()
	}

	// Node count excludes depth-0 origin(s).
	reached := 0
	for _, n := range resp.Nodes {
		if n.Depth > 0 {
			reached++
		}
	}
	fmt.Fprintf(&sb, "Reached **%d** nodes", reached)
	if resp.Stats.Truncated {
		sb.WriteString(" (truncated — more exist beyond the cap)")
	}
	sb.WriteString("\n\n")

	writeNodesByDepth(&sb, resp.Nodes)

	fmt.Fprintf(&sb, "_Edges: %d (use output_mode=full for the raw node/edge graph). Stats: %d visited, max depth %d._\n",
		len(resp.Edges), resp.Stats.NodesVisited, resp.Stats.MaxDepthReached)
	return sb.String()
}

// renderTraverseSummary renders a graph traversal as an aggregated summary:
// counts by node kind and edge relation kind, internal/external split, and the
// hottest target modules — no per-node list. This is the default, token-cheapest
// view; escalate to compact/full for node-level detail.
func (s *Server) renderTraverseSummary(store *facts.Store, resp traverseResponse, start, direction string) string {
	var sb strings.Builder
	if direction == "" {
		direction = "forward"
	}
	fmt.Fprintf(&sb, "# Traverse summary: %s (%s)\n\n", start, direction)
	writeResolutionNote(&sb, resp.Resolution)
	if resp.Resolution != nil && resp.Resolution.Matched == "" {
		return sb.String()
	}

	byKind := map[string]int{}
	byModule := map[string]int{}
	reached := 0
	for _, n := range resp.Nodes {
		if n.Depth == 0 {
			continue // skip origin
		}
		reached++
		byKind[n.Kind]++
		byModule[moduleOf(n)]++
	}

	byRel := map[string]int{}
	for _, e := range resp.Edges {
		byRel[e.Kind]++
	}

	verb := "depends on"
	if direction == "reverse" {
		verb = "is used by"
	}
	fmt.Fprintf(&sb, "%s **%s** %d nodes across %d modules", start, verb, reached, len(byModule))
	if resp.Stats.Truncated {
		sb.WriteString(" (truncated — more exist beyond the cap; raise max_nodes or narrow relation_kinds)")
	}
	sb.WriteString("\n\n")

	if len(byKind) > 0 {
		sb.WriteString("## By node kind\n\n")
		for _, k := range topCounts(byKind, len(byKind)) {
			fmt.Fprintf(&sb, "- %s: %d\n", k, byKind[k])
		}
		sb.WriteString("\n")
	}

	if len(byRel) > 0 {
		total := len(resp.Edges)
		sb.WriteString("## By relation kind\n\n")
		for _, k := range topCounts(byRel, len(byRel)) {
			pct := 0
			if total > 0 {
				pct = byRel[k] * 100 / total
			}
			fmt.Fprintf(&sb, "- %s: %d (%d%%)\n", k, byRel[k], pct)
		}
		sb.WriteString("\n")
	}

	// Internal vs external split for dependency nodes (looked up via the fact store).
	var internal, external, stdlib, framework int
	for _, n := range resp.Nodes {
		if n.Depth == 0 || n.Kind != facts.KindDependency {
			continue
		}
		for _, f := range store.LookupByExactName(n.Name) {
			src, _ := f.Props["source"].(string)
			switch src {
			case "external":
				external++
			case "stdlib":
				stdlib++
			case "internal":
				internal++
			case "framework":
				framework++
			}
			break
		}
	}
	if internal+external+stdlib+framework > 0 {
		fmt.Fprintf(&sb, "## Dependency sources\n\n- internal: %d\n- external: %d\n- stdlib: %d\n- framework: %d\n\n", internal, external, stdlib, framework)
	}

	if len(byModule) > 0 {
		sb.WriteString("## Hottest modules\n\n")
		for _, m := range topCounts(byModule, 5) {
			fmt.Fprintf(&sb, "- %s (%d nodes)\n", m, byModule[m])
		}
		sb.WriteString("\n")
	}

	// NodesVisited counts every node the walk PASSED THROUGH, including those a
	// node_kinds filter excluded from the result (graph.go traverses through them to
	// reach their neighbours). Printing it as a bare "N nodes" put an unfiltered
	// number beside a filtered list — node_kinds=["route"] could report "used by 2
	// nodes" above "Stats: 431 nodes". Label the two counts for what they are.
	fmt.Fprintf(&sb, "_Stats: %d nodes matched, %d walked, %d edges, max depth %d. Use output_mode=compact for the per-depth node list, output_mode=full for the raw graph._\n",
		reached, resp.Stats.NodesVisited, len(resp.Edges), resp.Stats.MaxDepthReached)
	return sb.String()
}

// renderImpactSummary renders an impact analysis as an aggregated summary:
// total dependents, breakdown by kind and depth, hotspot modules, cross-repo
// reach, and any architectural insights touching the target — no per-node list.
func (s *Server) renderImpactSummary(resp impactResponse) string {
	var sb strings.Builder
	r := resp.ImpactResult
	fmt.Fprintf(&sb, "# Impact summary: %s\n\n", r.Target)
	writeResolutionNote(&sb, resp.Resolution)
	if resp.Resolution != nil && resp.Resolution.Matched == "" {
		return sb.String()
	}

	if r.Summary != "" {
		sb.WriteString(r.Summary + "\n\n")
	}
	fmt.Fprintf(&sb, "**%d** total transitive dependents within max_depth %d.\n\n", r.TotalDependents, r.Stats.MaxDepthReached)

	byKind := map[string]int{}
	byModule := map[string]int{}
	depths := make([]int, 0, len(r.ByDepth))
	for d := range r.ByDepth {
		depths = append(depths, d)
	}
	sort.Ints(depths)
	for _, d := range depths {
		for _, n := range r.ByDepth[d] {
			byKind[n.Kind]++
			byModule[moduleOf(n)]++
		}
	}

	if len(byKind) > 0 {
		sb.WriteString("## Dependents by kind\n\n")
		for _, k := range topCounts(byKind, len(byKind)) {
			fmt.Fprintf(&sb, "- %s: %d\n", k, byKind[k])
		}
		sb.WriteString("\n")
	}

	if len(depths) > 0 {
		sb.WriteString("## By depth\n\n")
		for _, d := range depths {
			fmt.Fprintf(&sb, "- depth %d: %d\n", d, len(r.ByDepth[d]))
		}
		sb.WriteString("\n")
	}

	if len(byModule) > 0 {
		sb.WriteString("## Hotspot modules\n\n")
		for _, m := range topCounts(byModule, 5) {
			fmt.Fprintf(&sb, "- %s (%d dependents)\n", m, byModule[m])
		}
		sb.WriteString("\n")
	}

	if len(r.CrossRepoImpact) > 0 {
		fmt.Fprintf(&sb, "## Cross-repo impact\n\nOther repos with a dependent: %s\n\n", strings.Join(r.CrossRepoImpact, ", "))
	}

	writeGoverningIntent(&sb, r.GoverningIntent)

	if r.Forward != nil && len(r.Forward.Nodes) > 0 {
		fwd := map[string]int{}
		for _, n := range r.Forward.Nodes {
			if n.Depth > 0 {
				fwd[n.Kind]++
			}
		}
		sb.WriteString("## Forward dependencies (what the target depends on)\n\n")
		for _, k := range topCounts(fwd, len(fwd)) {
			fmt.Fprintf(&sb, "- %s: %d\n", k, fwd[k])
		}
		sb.WriteString("\n")
	}

	writeInsightList(&sb, "Architectural insights touching this target", s.insightsFor(r.Target))

	sb.WriteString("_Use output_mode=compact for the per-depth dependent list, output_mode=full for the raw graph._\n")
	return sb.String()
}

// traverseResponse wraps a graph traversal with the optional name resolution.
// The embedded TraversalResult inlines nodes/edges/stats alongside resolution.
type traverseResponse struct {
	Resolution *nameResolution `json:"resolution,omitempty"`
	facts.TraversalResult
}

// impactResponse wraps an impact analysis with the optional name resolution.
type impactResponse struct {
	Resolution *nameResolution `json:"resolution,omitempty"`
	facts.ImpactResult
}

// findPathResponse wraps a shortest-path result. find_path resolves two names,
// so it carries one resolution per side.
type findPathResponse struct {
	FromResolution *nameResolution `json:"from_resolution,omitempty"`
	ToResolution   *nameResolution `json:"to_resolution,omitempty"`
	// Note explains a found:false result — whether the endpoints were ambiguous
	// (with the candidates tried) or resolved uniquely but unreachable.
	Note string `json:"note,omitempty"`
	// FromTried/ToTried are the ranked endpoint candidates find_path attempted,
	// most-likely first, so the caller can see what was searched.
	FromTried []string `json:"from_tried,omitempty"`
	ToTried   []string `json:"to_tried,omitempty"`
	facts.PathResult
}
