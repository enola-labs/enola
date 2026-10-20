package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRestoreWaitsBeforeTheFreshnessReload: the SDK runs the last-registered
// receiving middleware outermost, so the restore has to be registered last. When it
// was registered first, a tool call made while the startup restore was loading
// reached the freshness check, which reads the workspace receipt, before it waited.
func TestRestoreWaitsBeforeTheFreshnessReload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := config.Default()
	cfg.Repo = t.TempDir()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	s, err := New(eng, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetReloader(func() map[string]int { return nil })

	// An unreadable receipt is the probe: the freshness check notes the file's
	// modification time before it reads it, and then gives up.
	receipt, err := engine.WorkspaceReceiptPath(cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(receipt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receipt, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	checked := func() bool {
		s.reloadMu.Lock()
		defer s.reloadMu.Unlock()
		return !s.reloadSeenMod.IsZero()
	}

	release := make(chan struct{})
	s.RestoreInBackground(func() map[string]int {
		<-release
		return nil
	})

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := s.mcp.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "enola-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	var releaseOnce sync.Once
	publish := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(publish)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "query_facts", Arguments: map[string]any{"kind": "module"}})
	}()

	time.Sleep(150 * time.Millisecond)
	if checked() {
		t.Fatal("the freshness check ran while the startup restore was still loading")
	}
	publish()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool call never ran after the restore published")
	}
	if !checked() {
		t.Fatal("the freshness check never ran after the restore published")
	}
}
