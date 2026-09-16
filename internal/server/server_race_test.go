package server_test

// Concurrency stress test driving the real MCP server over the in-memory transport,
// through the same jsonrpc2.Async dispatch production uses (tool calls run on their
// own goroutines, unserialized). It fires overlapping generate_snapshot calls
// interleaved with read-only tools and, to mirror the enterprise wrapper, a
// long-running read tool registered against the same *mcp.Server. Must be race-clean
// under `go test -race`.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerConcurrentToolCalls(t *testing.T) {
	ctx := context.Background()
	eng, cfg := newTestEngine(t)

	srv, err := bootstrap.NewServer(eng, cfg)
	if err != nil {
		t.Fatalf("bootstrap.NewServer: %v", err)
	}

	// Mirror the enterprise wrapper: register an extra long-running read-only tool
	// against the SAME server + engine, so a heavy reader overlaps generate_snapshot
	// exactly as find_orphans/analyze_performance do.
	mcp.AddTool(srv.MCP(), &mcp.Tool{
		Name:        "heavy_scan",
		Description: "test-only long read over the shared store",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		st := eng.Store()
		_ = st.Count()
		_ = st.ByKind(facts.KindSymbol)
		if snap := eng.Snapshot(); snap != nil {
			n := 0
			for range snap.Facts {
				n++
			}
			if snap.Meta.FactCount != n {
				return &mcp.CallToolResult{IsError: true}, nil, nil
			}
		}
		return &mcp.CallToolResult{}, nil, nil
	})

	// Wire the server to an in-memory transport and connect a client, driving tool
	// calls through the real jsonrpc2.Async dispatch.
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := srv.MCP().Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "enola-race-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	repoA := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), t.TempDir())
	repoB := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "python_sample"), t.TempDir())

	// Seed a snapshot so readers have data immediately.
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "generate_snapshot", Arguments: map[string]any{"repo_path": repoA, "fresh": true},
	}); err != nil {
		t.Fatalf("seed generate_snapshot: %v", err)
	}

	const duration = 2 * time.Second
	deadline := time.Now().Add(duration)

	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	report := func(err error) {
		if err == nil {
			return
		}
		select {
		case errCh <- err:
		default:
		}
	}

	callLoop := func(name string, args func(i int) map[string]any) {
		defer wg.Done()
		for i := 0; time.Now().Before(deadline); i++ {
			_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args(i)})
			if err != nil {
				report(err)
				return
			}
		}
	}

	// Writers: overlapping generate_snapshot, alternating fresh-A and append-B.
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go callLoop("generate_snapshot", func(i int) map[string]any {
			if i%2 == 0 {
				return map[string]any{"repo_path": repoA, "fresh": true}
			}
			return map[string]any{"repo_path": repoB, "append": true}
		})
	}

	// Readers: OSS read tools + the enterprise-style heavy_scan, all against the
	// shared store while generates churn it.
	readers := []struct {
		name string
		args func(i int) map[string]any
	}{
		{"query_facts", func(i int) map[string]any { return map[string]any{"kind": "symbol"} }},
		{"coverage_report", func(i int) map[string]any { return map[string]any{} }},
		{"query_insights", func(i int) map[string]any { return map[string]any{} }},
		{"heavy_scan", func(i int) map[string]any { return map[string]any{} }},
	}
	for _, r := range readers {
		for c := 0; c < 3; c++ {
			wg.Add(1)
			go callLoop(r.name, r.args)
		}
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("tool call transport error: %v", err)
	}
}
