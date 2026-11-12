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
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/auth"
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

	// checks coalesces proofs that a pooled graph is current: sessions asking for the
	// same graph while one walk of its trees is running share that walk's answer.
	checkMu sync.Mutex
	checks  map[any]*currentCheck

	// fixedServer serves every session of a fixed instance; nil otherwise. See
	// ServeFixed.
	fixedServer *Server

	mu       sync.Mutex
	graphs   map[string]*pooledGraph
	sessions map[*Server]*sessionState
	// restoring are the reloads from disk in flight, by pool key.
	restoring map[string]*restoreCall
	// sizes remembers the estimate of each graph only sessions hold, by graph key, so
	// accounting at the end of every call does not stat its files again.
	sizes map[any]int64
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
	// id is the session's identity in session marks, "<token id>/<session id>".
	id string
	// graph is what the session served when it was last seen, so a change (a generate,
	// a restore) is noticed and published.
	graph *engine.Graph
	// reload names the repositories of a graph the pool dropped from under this
	// session, to load again at its next call, and reloadID the snapshot it was. Nil
	// when nothing was dropped. The artifacts on disk are reloaded only while they
	// still hold that snapshot: another session indexing these repositories, alone or
	// linked with others, rewrites them, and serving that would answer for repositories
	// this session never asked about.
	reload   []string
	reloadID string
	// inflight counts this session's calls running now: a graph a call is reading is
	// never dropped.
	inflight int
}

// restoreCall is a reload from disk in flight. Sessions whose graph it is wait for it
// and share what it read, rather than each reading a copy the budget does not see.
type restoreCall struct {
	done    chan struct{}
	g       *engine.Graph
	err     error
	waiting []*sessionState
}

// errReplaced is why a dropped graph is not reloaded: the artifacts it would be read
// from now hold another snapshot.
var errReplaced = errors.New("its artifacts on disk now hold another snapshot, written since by a session that indexed these repositories, alone or linked with others")

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
		slots:     make(chan struct{}, limit),
		graphs:    map[string]*pooledGraph{},
		checks:    map[any]*currentCheck{},
		sessions:  map[*Server]*sessionState{},
		restoring: map[string]*restoreCall{},
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
func (p *Pool) newSession(token string) (*Server, error) {
	sc := p.sc
	sc.pool = p
	s, err := p.build(sc)
	if err != nil {
		return nil, err
	}
	s.token = token
	return s, nil
}

// attach registers a session once it is initialized, and detaches it when it ends.
func (p *Pool) attach(s *Server, ss *mcp.ServerSession) {
	id := s.token + "/" + ss.ID()
	s.eng.SetSessionID(id)
	p.mu.Lock()
	p.sessions[s] = &sessionState{id: id}
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

// live reports whether the session with this mark identity is still connected.
func (p *Pool) live(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, st := range p.sessions {
		if st.id == id {
			return true
		}
	}
	return false
}

// begin runs before each tool call of s: it counts the call as in flight and reloads a
// graph the pool dropped from under the session. A call that builds a graph anyway,
// generate_snapshot without append, skips the reload. A non-empty result refuses the
// call with that message: the graph could not be reloaded, and an answer from no graph
// would be wrong without saying so.
func (p *Pool) begin(s *Server, builds bool) string {
	p.mu.Lock()
	st := p.sessions[s]
	if st == nil {
		// Not attached: the call arrived before the initialized notification. Serve it;
		// it is accounted from its next call on.
		p.mu.Unlock()
		return ""
	}
	st.inflight++
	paths, want := st.reload, st.reloadID
	st.reload, st.reloadID = nil, ""
	if paths == nil || builds {
		p.mu.Unlock()
		return ""
	}
	key := graphKey(paths, engine.ExtractorVersion())
	where := strings.Join(paths, ", ")
	if pg := p.graphs[key]; pg != nil {
		// Another session holds these repositories. If it is another snapshot of them,
		// it is the one generate_snapshot would serve now too.
		st.graph, pg.lastUsed = pg.g, time.Now()
		p.mu.Unlock()
		s.eng.Adopt(pg.g)
		s.AddNotice(fmt.Sprintf("The graph of %s was unloaded to stay inside the server's memory budget; this call is served the copy another session holds.", where))
		return ""
	}
	rc, leader := p.restoring[key], false
	if rc == nil {
		rc, leader = &restoreCall{done: make(chan struct{})}, true
		p.restoring[key] = rc
	}
	rc.waiting = append(rc.waiting, st)
	p.mu.Unlock()

	if leader {
		// Outside the pool lock: a restore reads a whole graph from disk.
		g, err := p.reload(s, paths, want)
		p.mu.Lock()
		delete(p.restoring, key)
		rc.g, rc.err = g, err
		if g != nil {
			// In the pool and held by every session waiting for it before the lock is let
			// go: counted by the budget, and busy, so it is not dropped again at once.
			p.publishLocked(g)
			for _, w := range rc.waiting {
				w.graph = g
			}
		}
		p.mu.Unlock()
		close(rc.done)
		if err != nil {
			log.Printf("[pool] did not reload %s: %v", where, err)
		} else {
			p.phase("restore", g.FactCount())
		}
	} else {
		<-rc.done
		if rc.g != nil {
			if want != "" && rc.g.SnapshotID() != want {
				p.mu.Lock()
				st.graph = nil
				p.mu.Unlock()
				return fmt.Sprintf("The graph of %s was unloaded to stay inside the server's memory budget and was not reloaded: %v. Call generate_snapshot to build it again.", where, errReplaced)
			}
			s.eng.Adopt(rc.g)
		}
	}
	if rc.err != nil {
		return fmt.Sprintf("The graph of %s was unloaded to stay inside the server's memory budget and was not reloaded: %v. Call generate_snapshot to build it again.", where, rc.err)
	}
	s.AddNotice(fmt.Sprintf("The graph of %s was unloaded to stay inside the server's memory budget, and was reloaded from disk for this call.", where))
	return ""
}

// reload reads the graph of paths from disk into s's engine, as long as the artifacts
// still hold the snapshot want: checked before reading, which costs one small file,
// and again after, for a generate that rewrote them in between.
func (p *Pool) reload(s *Server, paths []string, want string) (*engine.Graph, error) {
	if want != "" && p.diskSnapshotID(paths) != want {
		return nil, errReplaced
	}
	s.genMu.Lock()
	err := p.restore(s.eng, paths)
	s.genMu.Unlock()
	if err != nil {
		return nil, err
	}
	g := s.eng.Graph()
	if g == nil {
		return nil, errors.New("the artifacts held no graph")
	}
	if want != "" && g.SnapshotID() != want {
		s.eng.Adopt(nil)
		return nil, errReplaced
	}
	return g, nil
}

// diskSnapshotID is the snapshot the artifacts of paths hold now, as the restore reads
// them: from the first repository's output directory.
func (p *Pool) diskSnapshotID(paths []string) string {
	raw, err := os.ReadFile(filepath.Join(p.outputDirFor(paths[0]), "snapshot.meta.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if json.Unmarshal(raw, &meta) != nil {
		return ""
	}
	return meta.SnapshotID
}

// phase tells the operator's OnPhase that the instance changed what it holds:
//
//	load     a fixed instance read its graph at start
//	reload   a fixed instance read the graph the operator wrote again
//	build    a session's call built a graph
//	restore  a session's graph, unloaded for the budget, was read back from disk
//	unload   graphs were unloaded for the budget, and the heap given back
//
// Never called with p.mu held: the memory instrument stops the world for a GC.
func (p *Pool) phase(name string, factCount int) {
	if p.sc.OnPhase != nil {
		p.sc.OnPhase(name, factCount)
	}
}

// end runs after each tool call of s: it publishes a graph the call built and holds
// the pool to its budget.
func (p *Pool) end(s *Server) {
	built := 0
	defer func() {
		if built > 0 {
			p.phase("build", built)
		}
	}()
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.sessions[s]
	if st == nil {
		return
	}
	st.inflight--
	now := time.Now()
	if g := s.eng.Graph(); g != nil && !g.Same(st.graph) {
		pg := p.graphs[graphKey(g.RepoPaths(), g.ExtractorVersion())]
		switch {
		case pg != nil && pg.g.Same(g):
			// Adopted from the pool: built by nobody now.
			st.graph, pg.lastUsed = g, now
		case pg != nil && pg.g.SnapshotID() == g.SnapshotID():
			// Built the snapshot another session already holds, the two builds having
			// run at once. Serve the held one and let this copy go, unless another call
			// of this session is still reading it.
			built = g.FactCount()
			if st.inflight == 0 {
				s.eng.Adopt(pg.g)
				st.graph, pg.lastUsed = pg.g, now
			} else {
				st.graph = g
			}
		default:
			built = g.FactCount()
			st.graph = g
			p.publishLocked(g)
		}
	} else if pg := p.entryFor(st.graph); pg != nil {
		pg.lastUsed = now
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

// holding is one graph the instance holds: in the pool, or only by sessions, such as a
// session's older snapshot of repositories another session has rebuilt since.
type holding struct {
	g        *engine.Graph
	paths    []string
	bytes    int64
	lastUsed time.Time
	// key is its pool key when the pool holds it; "" when only sessions do.
	key string
}

// holdingsLocked is every distinct graph held, each counted once however many
// sessions serve it. A graph only sessions hold has no last use and goes first.
func (p *Pool) holdingsLocked() []*holding {
	seen := map[any]*holding{}
	var out []*holding
	for key, pg := range p.graphs {
		h := &holding{g: pg.g, paths: pg.paths, bytes: pg.bytes, lastUsed: pg.lastUsed, key: key}
		seen[pg.g.Key()] = h
		out = append(out, h)
	}
	sizes := map[any]int64{}
	for _, st := range p.sessions {
		if st.graph == nil || seen[st.graph.Key()] != nil {
			continue
		}
		key := st.graph.Key()
		b, ok := p.sizes[key]
		if !ok {
			b = st.graph.EstimatedBytes(p.outputDirFor)
		}
		sizes[key] = b
		h := &holding{g: st.graph, paths: st.graph.RepoPaths(), bytes: b}
		seen[key] = h
		out = append(out, h)
	}
	p.sizes = sizes // only the graphs still held, so a dropped one is forgotten
	return out
}

// enforceLocked drops least recently used graphs until what the instance holds fits
// its budget. A graph a call is reading right now is never dropped, so the instance
// can sit above the budget while every graph is busy; it is logged, and the next call
// to end tries again.
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
				p.phase("unload", 0)
			}()
		}
	}()
	for {
		held := p.holdingsLocked()
		var resident int64
		for _, h := range held {
			resident += h.bytes
		}
		if resident <= p.sc.MemoryBudget {
			if evicted {
				log.Printf("[pool] holding %d graphs, %d MiB estimated, within the %d MiB budget", len(held), resident>>20, p.sc.MemoryBudget>>20)
			}
			return
		}
		victim := p.victimLocked(held)
		if victim == nil {
			log.Printf("[pool] holding %d graphs, %d MiB estimated, over the %d MiB budget, and every one is in use",
				len(held), resident>>20, p.sc.MemoryBudget>>20)
			return
		}
		if victim.key != "" {
			delete(p.graphs, victim.key)
		}
		p.evictions++
		evicted = true
		for s, st := range p.sessions {
			if victim.g.Same(st.graph) {
				s.eng.Adopt(nil)
				st.graph, st.reload, st.reloadID = nil, victim.paths, victim.g.SnapshotID()
			}
		}
		log.Printf("[pool] unloaded %s (%d MiB) to stay inside the %d MiB budget",
			strings.Join(victim.paths, ", "), victim.bytes>>20, p.sc.MemoryBudget>>20)
	}
}

func (p *Pool) residentLocked() int64 {
	var n int64
	for _, h := range p.holdingsLocked() {
		n += h.bytes
	}
	return n
}

// victimLocked is the least recently used graph no running call is reading.
func (p *Pool) victimLocked(held []*holding) *holding {
	busy := map[any]bool{}
	for _, st := range p.sessions {
		if st.inflight > 0 && st.graph != nil {
			busy[st.graph.Key()] = true
		}
	}
	var victim *holding
	for _, h := range held {
		if busy[h.g.Key()] {
			continue
		}
		if victim == nil || h.lastUsed.Before(victim.lastUsed) {
			victim = h
		}
	}
	return victim
}

type currentCheck struct {
	done chan struct{}
	ok   bool
}

// current reports whether g is current, as eng.Current does, sharing one walk between
// every caller that asks while it runs. A caller that arrives after a walk started
// takes its answer: a file changed between the walk passing it and the caller asking
// is missed, the same as one changed a moment after a lone walk finished.
func (p *Pool) current(eng *engine.Engine, g *engine.Graph) bool {
	key := g.Key()
	p.checkMu.Lock()
	if c, ok := p.checks[key]; ok {
		p.checkMu.Unlock()
		<-c.done
		return c.ok
	}
	c := &currentCheck{done: make(chan struct{})}
	p.checks[key] = c
	p.checkMu.Unlock()

	c.ok = eng.Current(g)
	p.checkMu.Lock()
	delete(p.checks, key)
	p.checkMu.Unlock()
	close(c.done)
	return c.ok
}

// PoolStats is what the pool holds, for tests and the log.
type PoolStats struct {
	Graphs, Sessions, Evictions int
	// Held counts the distinct graphs held, in the pool or only by sessions.
	Held          int
	ResidentBytes int64
}

// Stats reports the pool's state now.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{Graphs: len(p.graphs), Sessions: len(p.sessions), Evictions: p.evictions,
		Held: len(p.holdingsLocked()), ResidentBytes: p.residentLocked()}
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
	if !s.pool.current(s.eng, g) {
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
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		if p.fixedServer != nil {
			// One server for every session: nothing about a session is its own, so a
			// session per call costs nothing.
			return p.fixedServer.mcp
		}
		// The session's consumer is the token its initialize request authenticated
		// with; the requests after it carry the session id, not this.
		token := "-"
		if info := auth.TokenInfoFromContext(r.Context()); info != nil {
			token = info.UserID
		}
		s, err := p.newSession(token)
		if err != nil {
			log.Printf("[pool] could not start a session: %v", err)
			return nil
		}
		return s.mcp
	}, &mcp.StreamableHTTPOptions{
		SessionTimeout: sessionTimeout,
	})
	// A browser on another origin may not call the server: the SDK's option for
	// that is deprecated in favour of this middleware around its handler.
	protected := http.NewCrossOriginProtection().Handler(mcpHandler)
	return wrapHTTP(protected, cfg.Tokens, maxBody), nil
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
