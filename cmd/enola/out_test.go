package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/config"
)

func TestTakeOut(t *testing.T) {
	cases := []struct {
		in       []string
		rest     []string
		out      string
		wantsErr bool
	}{
		{[]string{"--generate", "."}, []string{"--generate", "."}, "", false},
		{[]string{"--generate", "--out", "/x", "."}, []string{"--generate", "."}, "/x", false},
		{[]string{"--out=/y", "--generate"}, []string{"--generate"}, "/y", false},
		{[]string{"--generate", "--out"}, nil, "", true},
		{[]string{"--out", "--generate"}, nil, "", true},
		{[]string{"--out="}, nil, "", true},
	}
	for _, c := range cases {
		rest, out, err := takeOut(c.in)
		if (err != nil) != c.wantsErr {
			t.Errorf("takeOut(%v) err = %v", c.in, err)
			continue
		}
		if c.wantsErr {
			continue
		}
		if out != c.out || !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("takeOut(%v) = %v, %q; want %v, %q", c.in, rest, out, c.rest, c.out)
		}
	}
}

func clusterCfg(t *testing.T) (*config.Config, []string) {
	t.Helper()
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := config.Default()
	c.Repos = []string{a, b}
	return c, []string{a, b}
}

func TestApplyOut(t *testing.T) {
	c, repos := clusterCfg(t)
	dir := filepath.Join(t.TempDir(), "graph")
	if err := applyOut(c, dir); err != nil {
		t.Fatal(err)
	}
	if c.Output.ClusterDir != dir {
		t.Errorf("ClusterDir = %s, want %s", c.Output.ClusterDir, dir)
	}
	if got := c.GraphOutputDir(repos); got != dir {
		t.Errorf("GraphOutputDir = %s, want %s", got, dir)
	}

	// Inside a member: that member's next snapshot would index the cluster's graph.
	c, repos = clusterCfg(t)
	if err := applyOut(c, filepath.Join(repos[1], "out")); err == nil {
		t.Error("--out inside a member was accepted")
	}

	// One repository writes to its own output dir; --out there means nothing.
	c = config.Default()
	c.Repo = t.TempDir()
	if err := applyOut(c, t.TempDir()); err == nil {
		t.Error("--out on a single repository was accepted")
	}
}
