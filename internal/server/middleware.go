package server

// The middleware every tool call passes through: the startup restore, the value
// accounting, the freshness banner and the update notice, with the corpus and
// reload state they keep.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/updatecheck"
	"github.com/enola-labs/enola/pkg/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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
// re-snapshotting. RestoreInBackground calls it once the restore publishes; a nil or
// empty map is ignored, and a later snapshot overwrites what it seeded.
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

// RestoreInBackground restores the workspace's graph from disk while the server is
// already serving, instead of before it. Call it once, before Run.
//
// The restore used to run in main, before stdio was read. A large graph takes tens of
// seconds to load, and for all of it the process could neither answer the handshake
// nor see its agent quit: a closed stdin went unread, so quitting the agent left the
// server loading, and the next session started another. Now stdio is served from the
// start, so the handshake is immediate and a closed stdin ends the process mid-restore
// (the restore only reads). Tool calls wait for the restore, as they would have waited
// for the process to start; one whose request is cancelled stops waiting.
//
// genMu is taken here, before the goroutine starts, so a generate_snapshot cannot run
// ahead of the restore and be overwritten by it: it waits, then runs as usual.
func (s *Server) RestoreInBackground(restore func() map[string]int) {
	done := make(chan struct{})
	s.restored = done
	s.genMu.Lock()
	go func() {
		defer close(done)
		defer s.genMu.Unlock()
		if corpus := restore(); corpus != nil {
			s.SeedCorpus(corpus)
		}
		s.markReceiptSeen()
	}()
}

// markReceiptSeen records the workspace receipt as it stands when the restore
// finishes, so reloadIfRewritten answers only to a rewrite that happens while this
// server runs. A sibling session's graph written before the restart is not news: the
// restore already chose between it and this session's own, and said so if it mattered.
func (s *Server) markReceiptSeen() {
	if s.cfg == nil {
		return
	}
	path, err := engine.WorkspaceReceiptPath(s.cfg.Repo)
	if err != nil {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	s.reloadMu.Lock()
	s.reloadSeenMod = info.ModTime()
	s.reloadMu.Unlock()
}

// restoreMiddleware holds every tool call until the startup restore has published.
// Everything else, the handshake and tools/list included, passes straight through.
func (s *Server) restoreMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" && s.restored != nil {
			select {
			case <-s.restored:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return next(ctx, method, req)
	}
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
	// Every agent session started in one directory shares this receipt. One naming
	// the repos this server serves is a refresh of this graph; one naming other repos
	// is a sibling session's graph, and swapping it in would answer this session's
	// questions about code it never asked for. Keep serving, and say so once.
	if same, loaded := s.eng.ServesReposOf(gr); loaded && !same {
		s.reloadTriedID = gr.SnapshotID
		log.Printf("[server] workspace receipt names another graph (snapshot %s); not reloading", shortID(gr.SnapshotID))
		s.AddNotice(fmt.Sprintf("Another agent session in this workspace snapshotted a different graph at %s (%s). "+
			"This session keeps serving its own; call generate_snapshot to switch.", gr.GeneratedAt, gr.RepoLabels(6)))
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

// AddNotice queues a one-time message for the agent, attached to the next tool
// result. Empty messages are ignored.
func (s *Server) AddNotice(msg string) {
	if msg == "" {
		return
	}
	s.noticeMu.Lock()
	s.notices = append(s.notices, msg)
	s.noticeMu.Unlock()
}

// takeNotices returns and clears the queued notices, so each is said exactly once
// even when tool calls arrive concurrently.
func (s *Server) takeNotices() string {
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	out := strings.Join(s.notices, "\n")
	s.notices = nil
	return out
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
		// Which graph is being served goes first: a stale warning about the wrong graph
		// is beside the point.
		banner := s.freshnessBanner()
		if notice := s.takeNotices(); notice != "" {
			banner = strings.TrimSpace("⚠️ " + notice + "\n" + banner)
		}
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
