package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	inthistory "github.com/enola-labs/enola/internal/history"
	"github.com/enola-labs/enola/pkg/history"
)

// architecture_history pointed at a folder of repositories read the folder's own history,
// which a cluster snapshot never writes: it records into each member. It now reads the
// first member that has revisions and says which.
func TestReadHistory_FolderOfReposReadsAMember(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	for _, n := range []string{"api", "web"} {
		if err := os.MkdirAll(filepath.Join(dir, n, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{cfg: config.Default()}

	// No member has history yet: the error still names the cluster.
	if _, _, _, err := s.readHistory(dir); err == nil || !strings.Contains(err.Error(), "Cluster "+dir) {
		t.Fatalf("want an error naming the cluster, got %v", err)
	}

	root, err := history.Root(filepath.Join(dir, "web"), "")
	if err != nil {
		t.Fatal(err)
	}
	entry := history.Entry{ID: "sha256:aaaa", Repo: "web", At: "2026-11-01T10:00:00Z", Epoch: "e1"}
	if _, err := inthistory.Append(root, entry, inthistory.Options{}); err != nil {
		t.Fatal(err)
	}

	entries, got, note, err := s.readHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != root || len(entries) != 1 {
		t.Errorf("read %s (%d entries), want web's history %s", got, len(entries), root)
	}
	if !strings.Contains(note, "history of web") || !strings.Contains(note, "members: api, web") {
		t.Errorf("note must name the member read and the members: %q", note)
	}

	// A member path is one repository, as before.
	if _, _, note, err := s.readHistory(filepath.Join(dir, "web")); err != nil || note != "" {
		t.Errorf("member path: note %q err %v, want none", note, err)
	}
}
