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

// architecture_history pointed at a folder of repositories, or at its cluster config,
// reads the cluster's own history: the snapshots of all members together, which no
// member's history holds. A member's path still reads that member alone.
func TestReadHistory_ClusterReadsItsOwnHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	api, web := filepath.Join(dir, "api"), filepath.Join(dir, "web")
	for _, m := range []string{api, web} {
		if err := os.MkdirAll(filepath.Join(m, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{cfg: config.Default()}

	// Nothing recorded yet: the error names the cluster and where older cluster
	// revisions were kept.
	_, _, _, err := s.readHistory(dir)
	if err == nil || !strings.Contains(err.Error(), "Cluster "+dir) || !strings.Contains(err.Error(), "member's path") {
		t.Fatalf("want an error naming the cluster and the members' older revisions, got %v", err)
	}

	clusterRoot, err := history.ClusterRoot([]string{api, web}, "")
	if err != nil {
		t.Fatal(err)
	}
	appendEntry(t, clusterRoot, "sha256:c1")
	webRoot, _ := history.Root(web, "")
	appendEntry(t, webRoot, "sha256:w1")

	cfg := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(cfg, []byte("repos:\n  - api\n  - web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, cfg} {
		entries, got, note, err := s.readHistory(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if got != clusterRoot || len(entries) != 1 || entries[0].ID != "sha256:c1" {
			t.Errorf("%s: read %s %v, want the cluster's history %s", p, got, entries, clusterRoot)
		}
		if !strings.Contains(note, "Cluster "+cfg) || !strings.Contains(note, "members: api, web") {
			t.Errorf("%s: note must name the cluster and its members: %q", p, note)
		}
	}

	entries, got, note, err := s.readHistory(web)
	if err != nil || got != webRoot || note != "" || len(entries) != 1 || entries[0].ID != "sha256:w1" {
		t.Errorf("member path: read %s %v note %q err %v, want web's own history", got, entries, note, err)
	}
}

func appendEntry(t *testing.T, root, id string) {
	t.Helper()
	entry := history.Entry{ID: id, Repo: "r", At: "2026-11-01T10:00:00Z", Epoch: "e1"}
	if _, err := inthistory.Append(root, entry, inthistory.Options{}); err != nil {
		t.Fatal(err)
	}
}
