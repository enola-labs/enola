package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/server"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// indexAs writes repo's artifacts the way the operator's `enola --generate` does,
// under cfg, which the server reads too.
func indexAs(t *testing.T, cfg *config.Config, repo string) {
	t.Helper()
	eng, err := bootstrap.NewEngineFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatal(err)
	}
	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatal(err)
	}
}

// startFixed serves repo, fixed, from a root holding it; indexed says whether its
// artifacts are written first.
func startFixed(t *testing.T, indexed bool) (*served, *config.Config) {
	t.Helper()
	root := t.TempDir()
	repo := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), root)
	addSymbol(t, repo, "FromTheOperator")
	_, cfg := newTestEngine(t)
	if indexed {
		indexAs(t, cfg, repo)
	}
	pool, roots, err := bootstrap.NewServedPool(t.Context(), cfg, bootstrap.ServeOptions{
		Roots:          []string{root},
		Restore:        []string{repo},
		ReloadInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := pool.HTTPHandler(bootstrap.HTTPOptions{Tokens: []string{testToken}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &served{url: ts.URL + "/mcp", root: roots[0], repo: repo, pool: pool}, cfg
}

// The consumer this mode exists for opens a session per call and never indexes. Every
// one of those sessions queries the operator's graph at once, and none of them is
// kept: a session per call holds nothing on the server.
func TestServeFixed_SessionPerCallQueriesTheServedGraph(t *testing.T) {
	s, _ := startFixed(t, true)
	for i := 0; i < 20; i++ {
		sess, err := s.connect(t, testToken)
		if err != nil {
			t.Fatal(err)
		}
		if !sess.has(t, "FromTheOperator") {
			t.Fatalf("call %d: a new session does not see the served graph", i)
		}
		_ = sess.cs.Close()
	}
	if st := s.pool.Stats(); st.Sessions != 0 || st.Graphs != 0 {
		t.Errorf("a fixed instance kept per-session state: %+v", st)
	}
}

// Consumers only query: the tools that index are not listed and do not run, and
// plan_check's patch, which indexes a scratch copy, is refused.
func TestServeFixed_ConsumersCannotIndex(t *testing.T) {
	s, _ := startFixed(t, true)
	sess, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if ins := sess.cs.InitializeResult().Instructions; !strings.Contains(ins, "already indexed") || strings.Contains(ins, "Index first") {
		t.Errorf("a fixed instance's instructions tell the consumer to index:\n%s", ins)
	}
	lr, err := sess.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range lr.Tools {
		if slices.Contains(server.FixedTools, tool.Name) {
			t.Errorf("%s is listed on a fixed instance", tool.Name)
		}
	}
	if len(lr.Tools) != len(server.ServedTools)-len(server.FixedTools) {
		t.Errorf("%d tools listed, want %d", len(lr.Tools), len(server.ServedTools)-len(server.FixedTools))
	}
	for _, name := range server.FixedTools {
		if _, err := sess.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"repo_path": s.repo}}); err == nil {
			t.Errorf("%s ran on a fixed instance", name)
		}
	}
	res := sess.call(t, "plan_check", map[string]any{"patch": "--- a/x\n+++ b/x\n"})
	if !res.IsError || !strings.Contains(text(res), "fixed graph") {
		t.Errorf("plan_check with a patch was not refused:\n%s", text(res))
	}
}

// The operator re-indexes; the server notices the new artifacts and serves them,
// without a restart and without any consumer asking.
func TestServeFixed_ReloadsWhenTheOperatorReindexes(t *testing.T) {
	s, cfg := startFixed(t, true)
	sess, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if sess.has(t, "AddedByTheOperator") {
		t.Fatal("precondition: the symbol is already served")
	}
	addSymbol(t, s.repo, "AddedByTheOperator")
	indexAs(t, cfg, s.repo)
	deadline := time.Now().Add(5 * time.Second)
	for !sess.has(t, "AddedByTheOperator") {
		if time.Now().After(deadline) {
			t.Fatal("the re-indexed graph was never served")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A session opened now, as a per-call consumer does, gets it too.
	fresh, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.has(t, "AddedByTheOperator") {
		t.Error("a new session does not see the reloaded graph")
	}
}

// Before the operator has indexed, a call says so, rather than telling the consumer
// to run a tool it does not have; once the artifacts appear, they are served.
func TestServeFixed_NotIndexedYet(t *testing.T) {
	s, cfg := startFixed(t, false)
	sess, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	res := sess.call(t, "query_facts", map[string]any{"kind": "symbol"})
	if !res.IsError || !strings.Contains(text(res), "not available") || strings.Contains(text(res), "Run generate_snapshot") {
		t.Fatalf("a call before the first index:\n%s", text(res))
	}
	indexAs(t, cfg, s.repo)
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := sess.call(t, "query_facts", map[string]any{"name": "FromTheOperator", "kind": "symbol", "output_mode": "summary"})
		if !res.IsError && !strings.Contains(text(res), "Found **0**") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first index was never served:\n%s", text(res))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A session per call without DELETE, as a client that drops its session does: still
// nothing is kept per session.
func TestServeFixed_DroppedSessionsHoldNothing(t *testing.T) {
	s, _ := startFixed(t, true)
	for i := 0; i < 50; i++ {
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`
		req, _ := http.NewRequest(http.MethodPost, s.url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("initialize: status %d", resp.StatusCode)
		}
	}
	if st := s.pool.Stats(); st.Sessions != 0 || st.Graphs != 0 {
		t.Errorf("dropped sessions left state behind: %+v", st)
	}
}
