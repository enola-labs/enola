package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
)

// A workspace snapshotted before is restored as the server starts, so the
// instructions stop sending the agent to generate_snapshot first, but only when a
// snapshot is on disk to restore: in its output directory, or in a repository its
// graph receipt names.
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

	// A workspace receipt points at the repositories its graph was taken over; it
	// counts only while one of them still holds the snapshot.
	cfg.Repo = t.TempDir()
	member := t.TempDir()
	receipt, err := engine.WorkspaceReceiptPath(cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(receipt), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"repos":[{"label":"m","path":%q}]}`, member)
	if err := os.WriteFile(receipt, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := startHint(cfg); !strings.HasPrefix(got, "Start with generate_snapshot") {
		t.Errorf("receipt whose snapshot is gone: %q, want generate_snapshot first", got)
	}
	memberOut := filepath.Join(member, cfg.Output.Dir)
	if err := os.MkdirAll(memberOut, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memberOut, "facts.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := startHint(cfg); !strings.Contains(got, "is loaded as the server starts") {
		t.Errorf("receipt whose member holds the snapshot: %q, want the loaded hint", got)
	}

	if got := startHint(nil); !strings.HasPrefix(got, "Start with generate_snapshot") {
		t.Errorf("no config: %q, want generate_snapshot first", got)
	}
}
