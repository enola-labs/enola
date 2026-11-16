package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/engine"
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
	if err := eng.WriteGraph(); err != nil {
		t.Fatal(err)
	}
	if _, stale := snapshotIsCurrent(eng, repos); stale != "" {
		t.Fatalf("a snapshot just written for an unchanged tree did not read as current: %s", stale)
	}

	// A member the cluster's directory does not describe has no snapshot there.
	membersPath := filepath.Join(eng.GraphDir(repos), engine.MembersMetaFile)
	raw, err := os.ReadFile(membersPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(membersPath, []byte(strings.ReplaceAll(string(raw), repoB, repoB+"-elsewhere")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stale := snapshotIsCurrent(eng, repos); stale == "" {
		t.Fatal("a member missing from the cluster's directory still read as current")
	}
	if err := os.WriteFile(membersPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repoB, "pkg", "a", "a.go"), []byte("package a\n\nfunc A() {}\n\nfunc A2() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stale := snapshotIsCurrent(eng, repos); stale == "" {
		t.Fatal("a moved file in one member still read as current")
	}
}
