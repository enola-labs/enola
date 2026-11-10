package server

// A served instance gives every MCP session its own Server and engine, so a
// consumer's generate_snapshot changes what THAT session serves and nobody else's.
// What sessions share is graphs: a graph one session built is kept in the pool, and
// another session asking for the same repositories, unchanged on disk, adopts it
// instead of loading a second copy. The pool holds graphs to a memory budget,
// dropping the least recently used one that no call is reading; a session whose graph
// was dropped reloads it from its artifacts on disk at its next call.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SessionFactory builds the Server one new session is served by, with an engine of
// its own. Supplied by the composition root, which knows which plugins and tools a
// server carries; NewServed is the core of it.
type SessionFactory func(sc ServeConfig) (*Server, error)

// Restorer loads the graph of repoPaths from their artifacts on disk into eng.
type Restorer func(eng *engine.Engine, repoPaths []string) error

// Pool is a served instance: the sessions, the graphs they share, and the limits
// every session is held to together.
type Pool struct {
	sc      ServeConfig
	build   SessionFactory
	restore Restorer
	// outputDirFor locates a repository's artifacts, for sizing a graph.
	outputDirFor func(string) string
	slots        chan struct{}

	mu       sync.Mutex
	graphs   map[string]*pooledGraph
	sessions map[*Server]*sessionState
	// evictions counts graphs dropped to stay inside the budget, for the log and tests.
	evictions int
}

type pooledGraph struct {
	key      string
	paths    []string
	g        *engine.Graph
	bytes    int64
	lastUsed time.Time
}

type sessionState struct {
	// graph is what the session served when it was last seen, so a change (a generate,
	// a restore) is noticed and published.
	graph *engine.Graph
	// reload names the repositories of a graph the pool dropped from under this
	// session, to load again at its next call. Nil when nothing was dropped.
	reload []string
	// inflight counts this session's calls running now: a graph a call is reading is
	// never dropped.
	inflight int
}

// NewPool creates a served instance. sc.MemoryBudget of zero means no budget.
func NewPool(sc ServeConfig, build SessionFactory, restore Restorer, outputDirFor func(string) string) (*Pool, error) {
	if len(sc.Roots) == 0 {
		return nil, errors.New("a served instance needs at least one root")
	}
	limit := sc.MaxConcurrent
	if limit <= 0 {
		limit = 1
	}
	p := &Pool{
		sc: sc, build: build, restore: restore, outputDirFor: outputDirFor,
		slots:    make(chan struct{}, limit),
		graphs:   map[string]*pooledGraph{},
		sessions: map[*Server]*sessionState{},
	}
	return p, nil
}

// graphKey names a set of repositories under one extraction behaviour. Sorted, so the
// order a consumer lists them in does not split one graph into two.
func graphKey(paths []string, extractorVersion string) string {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	return extractorVersion + "\x00" + strings.Join(sorted, "\x00")
}

// newSession builds the Server a new MCP session is served by. Called by the HTTP
// handler for each initialize request.
func (p *Pool) newSession() (*Server, error) {
	sc := p.sc
	sc.pool = p
	return p.build(sc)
}

// attach registers a session once it is initialized, and detaches it when it ends.
func (p *Pool) attach(s *Server, ss *mcp.ServerSession) {
	p.mu.Lock()
	p.sessions[s] = &sessionState{}
	p.mu.Unlock()
	go func() {
		_ = ss.Wait()
		p.mu.Lock()
		delete(p.sessions, s)
		p.mu.Unlock()
		// The session's engine is unreachable once the SDK drops the session; its
		// graph stays in the pool for the next session that asks for it.
	}()
}

// Warm loads repoPaths from disk into the pool, so the first session asking for them
// adopts the graph instead of building it. For `enola serve <repo>`.
func (p *Pool) Warm(repoPaths []string) error {
	s, err := p.newSession()
	if err != nil {
		return err
	}
	if err := p.restore(s.eng, repoPaths); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publishLocked(s.eng.Graph())
	p.enforceLocked()
	return nil
}

// begin runs before each tool call of s: it reloads a graph the pool dropped from
// under the session, and counts the call as in flight.
func (p *Pool) begin(s *Server) {
	p.mu.Lock()
	st := p.sessions[s]
	if st == nil {
		// Not attached: the call arrived before the initialized notification. Serve it;
		// it is accounted from its next call on.
		p.mu.Unlock()
		return
	}
	st.inflight++
	reload := st.reload
	st.reload = nil
	if reload != nil {
		if pg := p.graphs[graphKey(reload, engine.ExtractorVersion())]; pg != nil {
			s.eng.Adopt(pg.g)
			st.graph, pg.lastUsed = pg.g, time.Now()
			reload = nil
		}
	}
	p.mu.Unlock()
	if reload == nil {
		return
	}
	// Outside the pool lock: a restore reads a whole graph from disk.
	s.genMu.Lock()
	err := p.restore(s.eng, reload)
	s.genMu.Unlock()
	if err != nil {
		log.Printf("[pool] could not reload %s: %v", strings.Join(reload, ", "), err)
		s.AddNotice(fmt.Sprintf("The graph of %s was unloaded to stay inside the server's memory budget and could not be reloaded from disk (%v). Call generate_snapshot to build it again.",
			strings.Join(reload, ", "), err))
		return
	}
	s.AddNotice(fmt.Sprintf("The graph of %s was unloaded to stay inside the server's memory budget, and was reloaded from disk for this call.", strings.Join(reload, ", ")))
}

// end runs after each tool call of s: it publishes a graph the call built and holds
// the pool to its budget.
func (p *Pool) end(s *Server) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.sessions[s]
	if st == nil {
		return
	}
	st.inflight--
	if g := s.eng.Graph(); g != nil && !g.Same(st.graph) {
		st.graph = g
		p.publishLocked(g)
	} else if pg := p.entryFor(st.graph); pg != nil {
		pg.lastUsed = time.Now()
	}
	p.enforceLocked()
}

func (p *Pool) entryFor(g *engine.Graph) *pooledGraph {
	if g == nil {
		return nil
	}
	if pg := p.graphs[graphKey(g.RepoPaths(), g.ExtractorVersion())]; pg != nil && pg.g.Same(g) {
		return pg
	}
	return nil
}

// publishLocked keeps g in the pool, replacing an older graph of the same repositories.
func (p *Pool) publishLocked(g *engine.Graph) {
	if g == nil {
		return
	}
	paths := g.RepoPaths()
	key := graphKey(paths, g.ExtractorVersion())
	p.graphs[key] = &pooledGraph{key: key, paths: paths, g: g, bytes: g.EstimatedBytes(p.outputDirFor), lastUsed: time.Now()}
}

// lookup returns the pooled graph of exactly repoPaths, or nil.
func (p *Pool) lookup(repoPaths []string) *engine.Graph {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pg := p.graphs[graphKey(repoPaths, engine.ExtractorVersion())]; pg != nil {
		pg.lastUsed = time.Now()
		return pg.g
	}
	return nil
}

// enforceLocked drops least recently used graphs until the pool fits its budget. A
// graph a call is reading right now is never dropped, so the pool can sit above the
// budget while every graph in it is busy; it is logged, and the next call to end
// tries again.
func (p *Pool) enforceLocked() {
	if p.sc.MemoryBudget <= 0 {
		return
	}
	evicted := false
	defer func() {
		if evicted {
			// Unloading exists to give memory back. Left to itself, Go keeps freed
			// heap for reuse up to the soft limit, which on a large machine is most
			// of its RAM, and the process never shrinks. Rare enough to pay for.
			go func() {
				debug.FreeOSMemory()
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				log.Printf("[pool] heap after unloading: %d MiB live, %d MiB held from the OS", m.HeapAlloc>>20, (m.HeapSys-m.HeapReleased)>>20)
			}()
		}
	}()
	for p.residentLocked() > p.sc.MemoryBudget {
		victim := p.victimLocked()
		if victim == nil {
			log.Printf("[pool] %d MiB resident, over the %d MiB budget, and every graph is in use",
				p.residentLocked()>>20, p.sc.MemoryBudget>>20)
			return
		}
		delete(p.graphs, victim.key)
		p.evictions++
		evicted = true
		for s, st := range p.sessions {
			if victim.g.Same(st.graph) {
				s.eng.Adopt(nil)
				st.graph, st.reload = nil, victim.paths
			}
		}
		log.Printf("[pool] unloaded %s (%d MiB) to stay inside the %d MiB budget",
			strings.Join(victim.paths, ", "), victim.bytes>>20, p.sc.MemoryBudget>>20)
	}
}

func (p *Pool) residentLocked() int64 {
	var n int64
	for _, pg := range p.graphs {
		n += pg.bytes
	}
	return n
}

func (p *Pool) victimLocked() *pooledGraph {
	busy := map[string]bool{}
	for _, st := range p.sessions {
		if st.inflight > 0 && st.graph != nil {
			busy[graphKey(st.graph.RepoPaths(), st.graph.ExtractorVersion())] = true
		}
	}
	var victim *pooledGraph
	for _, pg := range p.graphs {
		if busy[pg.key] {
			continue
		}
		if victim == nil || pg.lastUsed.Before(victim.lastUsed) {
			victim = pg
		}
	}
	return victim
}

// PoolStats is what the pool holds, for tests and the log.
type PoolStats struct {
	Graphs, Sessions, Evictions int
	ResidentBytes               int64
}

// Stats reports the pool's state now.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{Graphs: len(p.graphs), Sessions: len(p.sessions), Evictions: p.evictions, ResidentBytes: p.residentLocked()}
}

// reuseGraph answers generate_snapshot from the pool when it holds a graph of exactly
// the repositories asked for and none of them changed on disk since: the session
// adopts it instead of extracting again. Nil when it cannot, and the call builds the
// graph as usual. An append, or a folder indexed as a cluster, always builds: what
// either adds up to depends on what the session holds or on the folder's contents.
func (s *Server) reuseGraph(args generateSnapshotArgs) *mcp.CallToolResult {
	if s.pool == nil || args.Append {
		return nil
	}
	var paths []string
	switch {
	case len(args.RepoPaths) > 0:
		resolved, err := resolveRepoPaths(args)
		if err != nil {
			return nil
		}
		paths = resolved
	case args.RepoPath != "":
		abs, err := filepath.Abs(args.RepoPath)
		if err != nil {
			return nil
		}
		if !args.NoCluster && len(workspace.Clusterable(abs, "")) > 0 {
			return nil
		}
		paths = []string{abs}
	default:
		return nil
	}
	g := s.pool.lookup(paths)
	if g == nil {
		return nil
	}
	s.genMu.Lock()
	defer s.genMu.Unlock()
	if !s.eng.Current(g) {
		return nil
	}
	s.eng.Adopt(g)
	s.snapshotsGenerated = true
	snap := s.eng.Snapshot()
	where := "Repository: " + snap.Meta.RepoPath
	if len(paths) > 1 {
		where = fmt.Sprintf("Repositories: %d (%s)", len(paths), strings.Join(paths, ", "))
	}
	summary := "Reused the graph this server already holds for these repositories: nothing in them changed since it was built.\n\n" +
		s.snapshotSummaryOf(snap, where, snap.Meta.Duration, snap.Meta.Extractors)
	return textResult(summary)
}

// HTTPHandler returns the Streamable HTTP handler for the pool, authenticated and
// size-limited, mounted at /mcp. Every other path is 404. Each session is served by a
// Server of its own. The SDK's DNS-rebinding protection stays on (it applies when
// bound to loopback), and browser cross-origin requests are refused; a service
// calling server to server sends no Origin and is unaffected.
func (p *Pool) HTTPHandler(cfg HTTPConfig) (http.Handler, error) {
	if len(cfg.Tokens) == 0 {
		return nil, errors.New("at least one bearer token is required")
	}
	sessionTimeout := cfg.SessionTimeout
	if sessionTimeout <= 0 {
		sessionTimeout = 30 * time.Minute
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 16 << 20
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		s, err := p.newSession()
		if err != nil {
			log.Printf("[pool] could not start a session: %v", err)
			return nil
		}
		return s.mcp
	}, &mcp.StreamableHTTPOptions{
		SessionTimeout:        sessionTimeout,
		CrossOriginProtection: http.NewCrossOriginProtection(),
	})
	return wrapHTTP(mcpHandler, cfg.Tokens, maxBody), nil
}

// RunHTTP serves HTTPHandler on ln until ctx is cancelled, then shuts down, giving
// calls in flight ten seconds to finish.
func (p *Pool) RunHTTP(ctx context.Context, ln net.Listener, cfg HTTPConfig) error {
	handler, err := p.HTTPHandler(cfg)
	if err != nil {
		return err
	}
	return serveHTTP(ctx, ln, handler)
}
