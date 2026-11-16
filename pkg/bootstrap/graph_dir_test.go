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

func goRepo(t *testing.T, dir, name string) string {
	t.Helper()
	for path, body := range map[string]string{
		"go.mod":  "module example.com/" + name + "\n\ngo 1.21\n",
		"main.go": "package main\n\nfunc main() { run() }\n\nfunc run() {}\n",
	} {
		full := filepath.Join(dir, name, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, name)
}

// A cluster generated with --out is found by a command given a config that does not
// say so: the dashboard was given the cluster.yaml already in the folder and opened
// on nothing.
func TestLoadDashboardSnapshot_FindsAGraphWrittenWithOut(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	repos := []string{goRepo(t, root, "api"), goRepo(t, root, "web")}

	gen := config.Default()
	gen.Repos = repos
	gen.Output.ClusterDir = filepath.Join(root, "my-index")
	eng, err := NewEngineFromConfig(gen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.GenerateCluster(t.Context(), repos, false); err != nil {
		t.Fatal(err)
	}
	if err := eng.WriteGraph(); err != nil {
		t.Fatal(err)
	}

	later := config.Default()
	later.Repos = []string{repos[1], repos[0]}
	if got := later.GraphOutputDir(later.Repos); got != gen.Output.ClusterDir {
		t.Fatalf("a config without cluster_dir looks in %s, not the --out directory", got)
	}
	dash, err := NewEngineFromConfig(later)
	if err != nil {
		t.Fatal(err)
	}
	if !LoadDashboardSnapshot(dash, later) {
		t.Fatal("the dashboard found no graph")
	}
	if dash.Store().Count() == 0 {
		t.Error("the dashboard loaded an empty graph")
	}
}
