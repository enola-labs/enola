package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRestoreInBackground_ServesWhileRestoring: the startup restore used to run before
// stdio was read, so a large graph kept the process deaf for tens of seconds: no
// handshake, and a quit went unnoticed. Now everything but tool calls passes at once,
// tool calls wait for the restore, a cancelled one stops waiting, and genMu is held
// throughout so no generate_snapshot runs ahead of the restore.
func TestRestoreInBackground_ServesWhileRestoring(t *testing.T) {
	s := &Server{}
	release := make(chan struct{})
	s.RestoreInBackground(func() map[string]int {
		<-release
		return map[string]int{"/repo": 42}
	})

	handled := make(chan string, 4)
	next := func(_ context.Context, method string, _ mcp.Request) (mcp.Result, error) {
		handled <- method
		return &mcp.CallToolResult{}, nil
	}
	mw := s.restoreMiddleware(next)
	toolReq := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: &mcp.CallToolParamsRaw{Name: "query_facts"}}

	// The handshake does not wait.
	if _, err := mw(context.Background(), "initialize", &mcp.ServerRequest[*mcp.InitializeParams]{Params: &mcp.InitializeParams{}}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if got := <-handled; got != "initialize" {
		t.Fatalf("handled %q, want initialize", got)
	}

	// A tool call waits.
	waited := make(chan error, 1)
	go func() {
		_, err := mw(context.Background(), "tools/call", toolReq)
		waited <- err
	}()
	select {
	case <-handled:
		t.Fatal("a tool call ran before the restore published")
	case <-time.After(100 * time.Millisecond):
	}
	if s.genMu.TryLock() {
		t.Fatal("genMu was free during the restore: a generate_snapshot could run ahead of it")
	}

	// A cancelled one stops waiting.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mw(ctx, "tools/call", toolReq); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled tool call returned %v, want context.Canceled", err)
	}

	close(release)
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("tool call after restore: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting tool call never ran after the restore published")
	}
	if !s.genMu.TryLock() {
		t.Fatal("genMu still held after the restore")
	}
	s.genMu.Unlock()
	if c := s.corpusByRepo.Load(); c == nil || (*c)["/repo"] != 42 {
		t.Errorf("the restored corpus was not seeded: %v", c)
	}
}
