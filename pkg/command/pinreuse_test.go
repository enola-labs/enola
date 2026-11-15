package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/bootstrap"
)

func writeGoRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for path, body := range map[string]string{
		"go.mod":     "module testmod\n\ngo 1.21\n",
		"pkg/a/a.go": "package a\n\nfunc A() {}\n",
		"pkg/b/b.go": "package b\n\nimport \"testmod/pkg/a\"\n\nfunc B() { a.A() }\n",
	} {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// A pin right after a generate freezes the snapshot on disk; a pin after a file moved
// does not get to, and neither does a pin over a dir that never held a snapshot.
func TestSnapshotIsCurrent_OnlyWhenEveryMemberMatchesItsTree(t *testing.T) {
	repoA, repoB := writeGoRepo(t), writeGoRepo(t)
	cfgPath := filepath.Join(t.TempDir(), "mcp-arch.yaml")
	if err := os.WriteFile(cfgPath, []byte("repo: "+repoA+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	repos := []string{repoA, repoB}

	if _, stale := snapshotIsCurrent(eng, repos); stale == "" {
		t.Fatal("a dir with no snapshot read as current")
	}

	if _, err := eng.GenerateCluster(context.Background(), repos, false); err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		if err := eng.WriteArtifacts(repo); err != nil {
			t.Fatal(err)
		}
	}
	if _, stale := snapshotIsCurrent(eng, repos); stale != "" {
		t.Fatalf("a snapshot just written for an unchanged tree did not read as current: %s", stale)
	}

	metaPath := filepath.Join(eng.OutputDir(repoB), "snapshot.meta.json")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte(strings.Replace(string(raw), "\"snapshot_id\": \"sha256:", "\"snapshot_id\": \"sha256:0", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stale := snapshotIsCurrent(eng, repos); stale == "" {
		t.Fatal("members holding different unions still read as current")
	}
	if err := os.WriteFile(metaPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repoB, "pkg", "a", "a.go"), []byte("package a\n\nfunc A() {}\n\nfunc A2() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stale := snapshotIsCurrent(eng, repos); stale == "" {
		t.Fatal("a moved file in one member still read as current")
	}
}
