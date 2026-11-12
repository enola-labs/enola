package server

// A served instance started with a repository or a cluster config serves that graph
// and nothing else: the operator chooses what is indexed, consumers only query. Every
// session, however short, is served by one Server holding that graph, so a consumer
// that opens a session per call (identity resolved per call, several replicas behind
// a load balancer) queries it at once, and a session costs nothing to keep.
//
// The graph is refreshed by the operator, out of band: `enola --generate` over the
// same repositories and output root, on whatever schedule suits them. The server
// notices the new artifacts on disk and reloads them, serving the old graph until the
// new one is in.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FixedTools are left out of ServedTools on an instance serving a fixed graph: they
// would change what every consumer is served. plan_check stays, without its patch
// argument, which indexes a scratch copy per call; see refuseFixedArgs.
var FixedTools = []string{"generate_snapshot", "set_baseline"}

// fileStamp says whether a file changed: its size and modification time.
type fileStamp struct {
	size  int64
	mtime time.Time
}

func stampOf(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{size: info.Size(), mtime: info.ModTime()}
}

// fixedGraph is the graph a fixed instance serves, and what it needs to reload it.
type fixedGraph struct {
	paths   []string
	restore Restorer
	// meta is the snapshot.meta.json the reload watches: the first repository's. A
	// cluster's generate writes the whole union into every member, so one is enough.
	meta string

	mu sync.Mutex
	// loaded is the stamp of meta when the served graph was read; seen is a stamp that
	// differs from it, waiting to hold still for a whole interval before it is read.
	loaded, seen fileStamp
	// err is why the graph is not loaded, for the consumer's error; nil once it is.
	err error
	// loads counts the graphs read, so a reload is told from the first load.
	loads int
	// phase is the pool's, told of every graph read.
	phase func(name string, factCount int)
}

// ServeFixed makes the pool serve repoPaths, restored from their artifacts on disk,
// to every session, and reloads them when the artifacts change. The restore runs in
// the background; calls made meanwhile wait for it. The poller stops with ctx.
func (p *Pool) ServeFixed(ctx context.Context, repoPaths []string) error {
	if len(repoPaths) == 0 {
		return errors.New("no repositories to serve")
	}
	sc := p.sc
	sc.pool, sc.fixed, sc.fixedPaths = p, true, repoPaths
	s, err := p.build(sc)
	if err != nil {
		return err
	}
	s.token = "-"
	fg := &fixedGraph{
		paths:   repoPaths,
		restore: p.restore,
		meta:    filepath.Join(p.outputDirFor(repoPaths[0]), "snapshot.meta.json"),
		phase:   p.phase,
	}
	s.fixed = fg
	p.fixedServer = s

	s.RestoreInBackground(func() map[string]int {
		fg.load(s.eng, stampOf(fg.meta))
		return nil
	})
	interval := p.sc.ReloadInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go fg.poll(ctx, s, interval)
	return nil
}

// load reads the graph into eng, remembering the stamp of the meta it read. On a
// failure the graph already served stays, and the next change is tried again.
func (fg *fixedGraph) load(eng *engine.Engine, st fileStamp) {
	err := fg.restore(eng, fg.paths)
	fg.mu.Lock()
	fg.seen = fileStamp{}
	if err != nil {
		if eng.Graph() == nil {
			fg.err = err
		}
		log.Printf("[serve] could not load the graph of %s: %v", strings.Join(fg.paths, ", "), err)
		// Remember what failed, so the same broken artifacts are not read every interval.
		fg.loaded = st
		fg.mu.Unlock()
		return
	}
	reload := fg.loads > 0
	fg.loaded, fg.err = st, nil
	fg.loads++
	fg.mu.Unlock()
	if reload {
		fg.phase("reload", eng.Graph().FactCount())
	} else {
		fg.phase("load", eng.Graph().FactCount())
	}
	snap := eng.Snapshot()
	log.Printf("[serve] serving the graph of %s: %d facts, snapshot %s from %s",
		strings.Join(fg.paths, ", "), eng.Graph().FactCount(), snap.Meta.SnapshotID, snap.Meta.GeneratedAt)
	if reload {
		// The graph replaced is garbage once the calls reading it finish. Left to
		// itself, Go keeps that heap for reuse up to the soft limit, so a server that
		// reloads a large graph daily would hold two of them for good.
		go func() {
			debug.FreeOSMemory()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			log.Printf("[serve] heap after the reload: %d MiB live, %d MiB held from the OS", m.HeapAlloc>>20, (m.HeapSys-m.HeapReleased)>>20)
		}()
	}
}

// poll checks the watched meta every interval and reloads once a change has held
// still for a whole interval: a generate writes several files, and a graph read
// while it is still writing them would mix two runs.
func (fg *fixedGraph) poll(ctx context.Context, s *Server, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.restored != nil {
			select {
			case <-s.restored:
			default:
				continue // the startup restore is still reading
			}
		}
		st := stampOf(fg.meta)
		fg.mu.Lock()
		switch {
		case st == fg.loaded || st == (fileStamp{}):
			fg.seen = fileStamp{}
			fg.mu.Unlock()
			continue
		case st != fg.seen:
			fg.seen = st // changed; read it once it holds still
			fg.mu.Unlock()
			continue
		}
		fg.mu.Unlock()
		// The reload swaps the graph in one step; calls running meanwhile finish on the
		// graph they started with.
		s.genMu.Lock()
		fg.load(s.eng, st)
		s.genMu.Unlock()
	}
}

// unavailable is the error a fixed instance returns while it has no graph to serve,
// in place of the advice to run generate_snapshot, which its consumers cannot.
func (fg *fixedGraph) unavailable() string {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	reason := "it has not been indexed yet"
	if fg.err != nil {
		reason = fg.err.Error()
	}
	return fmt.Sprintf("This server serves the graph of %s, and it is not available: %s. "+
		"Its operator indexes it with enola --generate; the server loads it when that finishes.",
		strings.Join(fg.paths, ", "), reason)
}

// fixedMiddleware answers every tool call with the unavailable error while the
// instance has no graph. Registered inside the restore wait, so a call made during
// the startup restore waits for it rather than getting this.
func (s *Server) fixedMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" && s.eng.Graph() == nil {
			return errorResult(s.fixed.unavailable()), nil
		}
		return next(ctx, method, req)
	}
}

// fixedArgsMiddleware refuses, on a fixed instance, the arguments refuseFixedArgs names.
func (s *Server) fixedArgsMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
			var args map[string]any
			_ = json.Unmarshal(params.Arguments, &args)
			if err := refuseFixedArgs(params.Name, args); err != nil {
				return errorResult(err.Error()), nil
			}
		}
		return next(ctx, method, req)
	}
}

// refuseFixedArgs refuses arguments that would index on a fixed instance: plan_check's
// patch is evaluated on a scratch copy indexed per call.
func refuseFixedArgs(tool string, args map[string]any) error {
	if tool == "plan_check" {
		if patch, _ := args["patch"].(string); patch != "" {
			return errors.New("patch: this server serves a fixed graph and does not index; plan_check without a patch still answers which constraints govern the paths and symbols")
		}
	}
	return nil
}

// fixedInstructions are the instructions of a fixed instance: what it serves, and that
// there is nothing to index.
func fixedInstructions(paths []string) string {
	return "enola answers questions about a codebase's STRUCTURE from a graph of facts: modules, symbols, routes, storage and dependencies. " +
		"This server serves one graph, already indexed, of: " + strings.Join(paths, ", ") + ". Query it directly; there is nothing to index first. " +
		"Which packages or files depend on X (impact_analysis(target=X)), who calls X (traverse(start=X, direction=reverse)), how A reaches B (find_path), what a backend endpoint change reaches in its clients (endpoint_impact), what nothing uses (find_orphans, query_insights(explainer='dead-code')). " +
		"query_insights returns the findings already computed (cycles, layer violations, unused routes, hotspots and more)."
}
