package server_test

import (
	"context"
	"os"
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
		if !strings.Contains(n.Message, "files") && !strings.Contains(n.Message, "stage") && !strings.Contains(n.Message, "artifacts") {
			t.Errorf("progress message %q does not say what is counted", n.Message)
		}
		last = n.Progress
	}
	if end := notes[len(notes)-1]; end.Progress != end.Total {
		t.Errorf("the last notification is %v of %v: a finished run must end on its total", end.Progress, end.Total)
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

// A set of repositories ends on one total that covers them all, not on the
// share of the first.
func TestE2E_GenerateSetProgressEndsOnItsTotal(t *testing.T) {
	write := func(name string, files map[string]string) string {
		repo := filepath.Join(t.TempDir(), name)
		for rel, body := range files {
			path := filepath.Join(repo, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return repo
	}
	alpha := write("alpha", map[string]string{"go.mod": "module alpha\n\ngo 1.21\n", "a.go": "package alpha\n\nfunc A() {}\n", "NOTES.md": "notes\n"})
	beta := write("beta", map[string]string{"go.mod": "module beta\n\ngo 1.21\n", "b.go": "package beta\n\nfunc B() {}\n", "NOTES.md": "notes\n"})
	eng, cfg := newTestEngine(t)
	srv, err := bootstrap.NewServer(eng, cfg)
	if err != nil {
		t.Fatalf("bootstrap.NewServer: %v", err)
	}
	var log progressLog
	cs := attachProgressClient(t, srv, &mcp.ClientOptions{ProgressNotificationHandler: log.handler})
	callWithToken(t, cs, "set-1", "generate_snapshot", map[string]any{"repo_paths": []string{alpha, beta}})
	checkProgress(t, log.seen(), "set-1")
}
