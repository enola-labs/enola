package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clusterRepos(t *testing.T, names ...string) []string {
	t.Helper()
	root := t.TempDir()
	var out []string
	for _, n := range names {
		p := filepath.Join(root, n)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// One repository's graph is that repository's own: no new place for it.
func TestGraphOutputDir_OneRepositoryIsItsOwnOutputDir(t *testing.T) {
	c := Default()
	repos := clusterRepos(t, "api")
	if got, want := c.GraphOutputDir(repos), c.OutputDirFor(repos[0]); got != want {
		t.Errorf("GraphOutputDir = %s, want the repository's own %s", got, want)
	}
}

// A cluster's directory is outside every member, and the same for the same members
// whatever order they arrive in: a cluster.yaml, a folder walk and an MCP list each
// have their own, and all three must find one graph.
func TestGraphOutputDir_ClusterIsKeyedByTheMemberSet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Default()
	repos := clusterRepos(t, "api", "web", "worker")
	a := c.GraphOutputDir(repos)
	b := c.GraphOutputDir([]string{repos[2], repos[0], repos[1]})
	if a != b {
		t.Fatalf("member order changed the cluster's directory: %s vs %s", a, b)
	}
	for _, r := range repos {
		if rel, err := filepath.Rel(r, a); err == nil && !strings.HasPrefix(rel, "..") {
			t.Errorf("cluster directory %s is inside member %s", a, r)
		}
	}
	home, _ := os.UserHomeDir()
	if filepath.Dir(a) != filepath.Join(home, ".enola", "graphs") {
		t.Errorf("cluster directory %s, want it under ~/.enola/graphs", a)
	}
	if other := c.GraphOutputDir(repos[:2]); other == a {
		t.Errorf("a different member set got the same directory %s", a)
	}
}

// The same repository referenced through a symlink is the same member.
func TestGraphOutputDir_SymlinkedMemberIsTheSameMember(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Default()
	repos := clusterRepos(t, "api", "web")
	link := filepath.Join(t.TempDir(), "api-link")
	if err := os.Symlink(repos[0], link); err != nil {
		t.Skip("no symlinks here")
	}
	if a, b := c.GraphOutputDir(repos), c.GraphOutputDir([]string{link, repos[1]}); a != b {
		t.Errorf("symlinked member moved the cluster: %s vs %s", a, b)
	}
}

func TestGraphOutputDir_ClusterDirAndRoot(t *testing.T) {
	repos := clusterRepos(t, "api", "web")

	c := Default()
	c.Output.Root = t.TempDir()
	got := c.GraphOutputDir(repos)
	if filepath.Dir(got) != filepath.Join(c.Output.Root, "clusters") {
		t.Errorf("with output.root, cluster directory %s, want it under %s/clusters", got, c.Output.Root)
	}

	c.Output.ClusterDir = t.TempDir()
	if got := c.GraphOutputDir(repos); got != c.Output.ClusterDir {
		t.Errorf("output.cluster_dir set, GraphOutputDir = %s, want exactly %s", got, c.Output.ClusterDir)
	}
	// A single repository ignores it: its graph stays its own.
	if got, want := c.GraphOutputDir(repos[:1]), c.OutputDirFor(repos[0]); got != want {
		t.Errorf("output.cluster_dir moved a single repository's output to %s", got)
	}
}

// A relative cluster_dir means the same thing wherever the config is run from, like
// the repos: entries beside it.
func TestLoad_RelativeClusterDirResolvesAgainstTheConfig(t *testing.T) {
	cfg, err := loadYAML(t, "repos:\n  - a\n  - b\noutput:\n  cluster_dir: graph\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(cfg.SourcePath), "graph"); cfg.Output.ClusterDir != want {
		t.Errorf("cluster_dir = %s, want %s", cfg.Output.ClusterDir, want)
	}
}
