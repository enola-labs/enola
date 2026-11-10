package server

// Serving over Streamable HTTP, for consumers that are not an agent this process was
// started by: another service that calls enola as a tool. Everything here exists
// because such a server is reachable by more than its operator, and nothing in it is
// active on the stdio path an agent starts.
//
// The rules, in the order a call meets them: a bearer token from the operator's
// token file, then the tool allowlist, then path confinement to the operator's roots,
// then the concurrency cap and the per-call timeout. Every call is logged once, after
// it ran, without its arguments.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServeConfig is what a served instance enforces on every tool call. Its zero value
// is not usable: Roots must name at least one directory.
type ServeConfig struct {
	// Roots are the absolute, symlink-resolved directories every filesystem path a
	// call names or reaches must lie in. See NewRoots.
	Roots Roots
	// MaxConcurrent caps tool calls running at once; zero means runtime.NumCPU().
	MaxConcurrent int
	// CallTimeout cancels a tool call that runs longer; zero means no limit.
	CallTimeout time.Duration
	// MemoryBudget is the estimated memory the pool's graphs may hold before the least
	// recently used one is unloaded; zero means no budget. See Pool.
	MemoryBudget int64

	// pool is the served instance this session's Server belongs to; set by Pool.
	pool *Pool
}

// HTTPConfig is the transport half: who may connect and how long a session lives.
type HTTPConfig struct {
	// Tokens are the accepted bearer tokens. At least one is required.
	Tokens []string
	// SessionTimeout closes a session idle for this long; zero means 30 minutes.
	SessionTimeout time.Duration
	// MaxBodyBytes caps a request body; zero means 16 MiB, room for a plan_check patch.
	MaxBodyBytes int64
}

// ServedTools are the tools a served instance exposes. An allowlist, not a denylist:
// a tool added to the server later stays invisible to HTTP consumers until it is
// named here, because what is safe to hand an agent on its own machine is not
// automatically safe to hand a service. TestServedToolsAreDecided fails for a tool
// that is in neither this list nor UnservedTools.
var ServedTools = []string{
	"analyze_performance", "architecture_blame", "architecture_history", "compare_receipts",
	"constraints_for", "coverage_report", "diff_snapshot", "endpoint_impact", "explore",
	"find_orphans", "find_path", "generate_snapshot", "governing_intent", "impact_analysis",
	"locate", "package_metrics", "plan_check", "query_facts", "query_insights",
	"set_baseline", "show_symbol", "snapshot_receipt", "touched", "traverse",
}

// UnservedTools are registered tools deliberately kept off HTTP. Empty today; a tool
// lands here, with a reason beside it, when it is decided against.
var UnservedTools = []string{}

// Roots is the set of directories a served instance may read and write under.
type Roots []string

// NewRoots resolves dirs to absolute, symlink-free directories. A root that does not
// exist or is not a directory is an error: a typo must not leave the server confined
// to nothing and refusing every call with a message about the caller's path.
func NewRoots(dirs []string) (Roots, error) {
	if len(dirs) == 0 {
		return nil, errors.New("at least one root directory is required")
	}
	var out Roots
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, fmt.Errorf("root %s: %w", d, err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("root %s: %w", d, err)
		}
		info, err := os.Stat(real)
		if err != nil {
			return nil, fmt.Errorf("root %s: %w", d, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("root %s is not a directory", d)
		}
		if !slices.Contains(out, real) {
			out = append(out, real)
		}
	}
	return out, nil
}

// ErrOutsideRoots is returned for a path that is not inside any root.
var ErrOutsideRoots = errors.New("outside the directories this server serves")

// Confine reports whether p lies inside a root once symlinks are resolved, so a link
// inside a root that points out of it is refused. A path that does not exist is
// refused too: it cannot be resolved, so it cannot be shown to be inside.
func (r Roots) Confine(p string) error {
	abs, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s does not exist", p)
		}
		return fmt.Errorf("%s: %w", p, err)
	}
	for _, root := range r {
		rel, err := filepath.Rel(root, real)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)) {
			return nil
		}
	}
	return fmt.Errorf("%s is %w", p, ErrOutsideRoots)
}

// serveInstructions are the instructions a served instance gives its consumers. The
// stdio ones speak to a coding agent in its own session (deferred tool loading,
// "if asked whether a project needs enola"); a service calling enola as a tool needs
// what the tools answer and the order they are used in, and the paths it may name.
func serveInstructions() string {
	return "enola answers questions about a codebase's STRUCTURE from a graph of facts: modules, symbols, routes, storage and dependencies, across one repository or several linked ones. " +
		"Index first: generate_snapshot(repo_path) for one repository, generate_snapshot(repo_paths=[...]) for several linked as one set, for example a backend and the frontends that call it. " +
		"Paths are on the server's filesystem and must lie inside the directories it serves; others are refused. " +
		"Each session has its own loaded graph; indexing repositories another session already indexed, unchanged since, reuses that graph at once. " +
		"Then: which packages or files depend on X (impact_analysis(target=X)), who calls X (traverse(start=X, direction=reverse)), how A reaches B (find_path), what a backend endpoint change reaches in its clients (endpoint_impact), what nothing uses (find_orphans, query_insights(explainer='dead-code')). " +
		"query_insights returns the findings already computed (cycles, layer violations, unused routes, hotspots and more). " +
		"To grade a change: generate_snapshot, set_baseline, change, generate_snapshot, diff_snapshot; diff_snapshot reports only what changed."
}

// serveMiddleware installs the per-call rules of a served instance. Registered after
// every other middleware, so outermost: a refused call never waits on the startup
// restore, takes a concurrency slot, or is priced by the value middleware.
func (s *Server) serveMiddleware(cfg ServeConfig) {
	var slots chan struct{}
	if cfg.pool != nil {
		slots = cfg.pool.slots // one cap for every session together
	} else {
		limit := cfg.MaxConcurrent
		if limit <= 0 {
			limit = runtime.NumCPU()
		}
		slots = make(chan struct{}, limit)
	}
	served := make(map[string]bool, len(ServedTools))
	for _, n := range ServedTools {
		served[n] = true
	}

	// Innermost: the session's graph, reloaded before the call if the pool dropped it
	// and published after it if the call built one.
	if pool := cfg.pool; pool != nil {
		s.mcp.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				if method != "tools/call" {
					return next(ctx, method, req)
				}
				pool.begin(s)
				defer pool.end(s)
				return next(ctx, method, req)
			}
		})
	}

	// The slot and the deadline cover the handler and the graph's reload.
	s.mcp.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			defer func() { <-slots }()
			if cfg.CallTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, cfg.CallTimeout)
				defer cancel()
			}
			return next(ctx, method, req)
		}
	})

	s.mcp.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok {
				return next(ctx, method, req)
			}
			if err := confineArgs(cfg.Roots, params.Arguments); err != nil {
				return errorResult(err.Error()), nil
			}
			return next(ctx, method, req)
		}
	})

	s.mcp.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "tools/list":
				res, err := next(ctx, method, req)
				if lr, ok := res.(*mcp.ListToolsResult); ok && lr != nil {
					lr.Tools = slices.DeleteFunc(slices.Clone(lr.Tools), func(t *mcp.Tool) bool { return !served[t.Name] })
				}
				return res, err
			case "tools/call":
				if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && !served[params.Name] {
					return nil, fmt.Errorf("unknown tool %q", params.Name)
				}
			}
			return next(ctx, method, req)
		}
	})

	s.mcp.AddReceivingMiddleware(auditMiddleware)
}

// auditMiddleware logs one line per tool call after it ran: who (the token's id, never
// the token), which session, which tool, the outcome and the sizes. The arguments are
// left out: a patch or a search pattern is the consumer's business, not the log's.
func auditMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		start := time.Now()
		res, err := next(ctx, method, req)
		tool, argBytes := "", 0
		if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
			tool, argBytes = params.Name, len(params.Arguments)
		}
		who := "-"
		if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil {
			who = extra.TokenInfo.UserID
		}
		session := "-"
		if ss := req.GetSession(); ss != nil && ss.ID() != "" {
			session = ss.ID()
		}
		outcome, respBytes := "ok", 0
		switch ctr, _ := res.(*mcp.CallToolResult); {
		case err != nil:
			outcome = "error"
		case ctr != nil && ctr.IsError:
			outcome = "tool_error"
		}
		if ctr, ok := res.(*mcp.CallToolResult); ok && ctr != nil {
			respBytes = resultBytes(ctr)
		}
		log.Printf("[audit] token=%s session=%s tool=%s outcome=%s duration=%s args_bytes=%d response_bytes=%d",
			who, session, tool, outcome, time.Since(start).Round(time.Millisecond), argBytes, respBytes)
		return res, err
	}
}

// confineArgs refuses a call whose arguments name a filesystem path outside roots.
// It goes by argument NAME, so a tool added later with a repo_path argument is covered
// without being listed here: every key ending in _path or _paths is a path, and so is
// baseline unless it is one of its two selectors. The engine's own guard catches what
// arguments only lead to, such as the members of a cluster config.
func confineArgs(roots Roots, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil // the tool's own schema validation reports it
	}
	for key, val := range args {
		isPath := strings.HasSuffix(key, "_path") || strings.HasSuffix(key, "_paths") || key == "baseline"
		if !isPath {
			continue
		}
		var paths []string
		var one string
		if err := json.Unmarshal(val, &one); err == nil {
			paths = []string{one}
		} else if err := json.Unmarshal(val, &paths); err != nil {
			continue
		}
		for _, p := range paths {
			if p == "" {
				continue
			}
			if key == "baseline" {
				switch strings.ToLower(strings.TrimSpace(p)) {
				case "pinned", "previous":
					continue
				}
			}
			if err := roots.Confine(p); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	return nil
}

// tokenID names a token in the log without revealing it: the first 12 hex digits of
// its SHA-256, enough to tell consumers apart and useless for authenticating.
func tokenID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:6])
}

// tokenVerifier accepts exactly the given tokens, compared in constant time.
func tokenVerifier(tokens []string) auth.TokenVerifier {
	type entry struct {
		sum [32]byte
		id  string
	}
	known := make([]entry, 0, len(tokens))
	for _, t := range tokens {
		known = append(known, entry{sha256.Sum256([]byte(t)), tokenID(t)})
	}
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		sum := sha256.Sum256([]byte(token))
		match := ""
		for _, k := range known {
			if subtle.ConstantTimeCompare(sum[:], k.sum[:]) == 1 {
				match = k.id
			}
		}
		if match == "" {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: match}, nil
	}
}

// wrapHTTP puts bearer authentication and a body size limit in front of an MCP
// handler and mounts it at /mcp; every other path is 404.
func wrapHTTP(mcpHandler http.Handler, tokens []string, maxBody int64) http.Handler {
	authed := auth.RequireBearerToken(tokenVerifier(tokens), &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(mcpHandler)
	limited := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		authed.ServeHTTP(w, r)
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", limited)
	return mux
}

// serveHTTP serves handler on ln until ctx is cancelled, then shuts down, giving
// calls in flight ten seconds to finish.
func serveHTTP(ctx context.Context, ln net.Listener, handler http.Handler) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		// No WriteTimeout: a response is an event stream that lasts as long as its
		// call, and generate_snapshot on a large repository runs for minutes. The
		// per-call timeout bounds that instead.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return ctx.Err()
}
