package server_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type progressLog struct {
	mu    sync.Mutex
	notes []*mcp.ProgressNotificationParams
}

func (p *progressLog) handler(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notes = append(p.notes, req.Params)
}

func (p *progressLog) seen() []*mcp.ProgressNotificationParams {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*mcp.ProgressNotificationParams(nil), p.notes...)
}

func attachProgressClient(t *testing.T, srv *bootstrap.Server, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := srv.MCP().Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "enola-test", Version: "0"}, opts).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callWithToken(t *testing.T, cs *mcp.ClientSession, token any, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	params := &mcp.CallToolParams{Name: name, Arguments: args}
	if token != nil {
		params.SetProgressToken(token)
	}
	res, err := cs.CallTool(context.Background(), params)
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: %s", name, text(res))
	}
	return res
}

func checkProgress(t *testing.T, notes []*mcp.ProgressNotificationParams, token any) {
	t.Helper()
	if len(notes) == 0 {
		t.Fatal("no progress notification before the answer")
	}
	last := -1.0
	for _, n := range notes {
		if n.ProgressToken != token {
			t.Errorf("progress token %v, want %v", n.ProgressToken, token)
		}
		if n.Progress <= last {
			t.Errorf("progress %v after %v: it must increase with each notification", n.Progress, last)
		}
		if n.Total <= 0 || n.Progress > n.Total {
			t.Errorf("progress %v of %v: files extracted of files found", n.Progress, n.Total)
		}
		if !strings.Contains(n.Message, "files") {
			t.Errorf("progress message %q does not say what is counted", n.Message)
		}
		last = n.Progress
	}
}

// generate_snapshot with a progress token hears how far the extraction has
// got; without one it hears nothing, as before.
func TestE2E_GenerateReportsProgressToATokenedRequest(t *testing.T) {
	eng, cfg := newTestEngine(t)
	srv, err := bootstrap.NewServer(eng, cfg)
	if err != nil {
		t.Fatalf("bootstrap.NewServer: %v", err)
	}
	var log progressLog
	cs := attachProgressClient(t, srv, &mcp.ClientOptions{ProgressNotificationHandler: log.handler})
	repo := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), t.TempDir())

	callWithToken(t, cs, nil, "generate_snapshot", map[string]any{"repo_path": repo})
	if n := len(log.seen()); n != 0 {
		t.Fatalf("a generate that carried no progress token was sent %d progress notifications", n)
	}
	callWithToken(t, cs, "gen-1", "generate_snapshot", map[string]any{"repo_path": repo, "fresh": true})
	checkProgress(t, log.seen(), "gen-1")
}
