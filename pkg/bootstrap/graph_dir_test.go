package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/facts"
)

func touchFacts(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "facts.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A cluster's graph is restored from the directory its receipt recorded (--out), else
// from the one the config gives the same members, and not at all when a member's
// checkout is gone.
func TestGraphDirOf_Cluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	gr := &facts.GraphReceipt{Repos: []facts.GraphRepoEntry{{Label: "a", Path: a}, {Label: "b", Path: b}}}

	derived := cfg.GraphOutputDir([]string{a, b})
	touchFacts(t, derived)
	if dir, ok := graphDirOf(gr, cfg); !ok || dir != derived {
		t.Errorf("graphDirOf = %q, %v; want the config's cluster directory %s", dir, ok, derived)
	}

	gr.GraphDir = filepath.Join(t.TempDir(), "out")
	touchFacts(t, gr.GraphDir)
	if dir, ok := graphDirOf(gr, cfg); !ok || dir != gr.GraphDir {
		t.Errorf("graphDirOf = %q, %v; want the recorded %s", dir, ok, gr.GraphDir)
	}

	if err := os.RemoveAll(b); err != nil {
		t.Fatal(err)
	}
	if dir, ok := graphDirOf(gr, cfg); ok {
		t.Errorf("a cluster with a member gone was restored from %s", dir)
	}
}
