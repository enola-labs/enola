package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
)

// A workspace snapshotted before is restored as the server starts, so the
// instructions stop sending the agent to generate_snapshot first. A graph is on disk
// either as the workspace's graph receipt or as a snapshot in its output directory.
func TestStartHint_FollowsTheSavedGraph(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.Default()
	cfg.Repo = t.TempDir()

	if got := startHint(cfg); !strings.HasPrefix(got, "Start with generate_snapshot") {
		t.Errorf("nothing saved: %q, want generate_snapshot first", got)
	}

	out := filepath.Join(cfg.Repo, cfg.Output.Dir)
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "facts.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := startHint(cfg); !strings.Contains(got, "is loaded as the server starts") {
		t.Errorf("snapshot in the output directory: %q, want the loaded hint", got)
	}

	cfg.Repo = t.TempDir()
	receipt, err := engine.WorkspaceReceiptPath(cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(receipt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receipt, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := startHint(cfg); !strings.Contains(got, "is loaded as the server starts") {
		t.Errorf("workspace receipt: %q, want the loaded hint", got)
	}

	if got := startHint(nil); !strings.HasPrefix(got, "Start with generate_snapshot") {
		t.Errorf("no config: %q, want generate_snapshot first", got)
	}
}
