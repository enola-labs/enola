package engine_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// A cluster's artifacts do not depend on the order its members are indexed in, and a
// refresh of one member writes what a full generate of the same trees writes. Both
// failed on docs.jsonl: the first member's whole comments were lost, and a refresh
// restored the other members' comments flattened.
func TestCluster_RefreshWritesWhatAFullGenerateWrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	src := filepath.Join("testdata", "repos")
	base := t.TempDir()
	api := copyTree(t, filepath.Join(src, "go_httpclient_multirepo", "api"), base)
	consumer := copyTree(t, filepath.Join(src, "go_httpclient_multirepo", "consumer"), base)
	sample := copyTree(t, filepath.Join(src, "go_sample"), base)
	members := []string{api, consumer, sample}

	generate := func(order []string, dir string) {
		t.Helper()
		cfg := config.Default()
		cfg.Repos = order
		cfg.Output.ClusterDir = dir
		eng, err := bootstrap.NewEngineFromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := eng.GenerateCluster(context.Background(), order, false); err != nil {
			t.Fatal(err)
		}
		if err := eng.WriteGraph(); err != nil {
			t.Fatal(err)
		}
	}
	same := func(a, b, what string) {
		t.Helper()
		for _, name := range []string{"facts.jsonl", "insights.json", "flows.json", "docs.jsonl"} {
			x, _ := os.ReadFile(filepath.Join(a, name))
			y, _ := os.ReadFile(filepath.Join(b, name))
			if len(x) == 0 || !bytes.Equal(x, y) {
				t.Errorf("%s: %s differs", what, name)
			}
		}
	}

	first, last := filepath.Join(base, "out-first"), filepath.Join(base, "out-last")
	generate(members, first)
	generate([]string{sample, consumer, api}, last)
	same(first, last, "member order")

	// Refresh api after an edit, into the union first holds; then a full generate.
	server := filepath.Join(api, "server.go")
	data, err := os.ReadFile(server)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server, bytes.ReplaceAll(data, []byte("/things/{id}"), []byte("/items/{id}")), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Repos = members
	cfg.Output.ClusterDir = first
	eng, err := bootstrap.NewEngineFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.SetRestoreDocTexts(true)
	if err := bootstrap.RestoreSnapshot(eng, members); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.GenerateSnapshot(context.Background(), api, true); err != nil {
		t.Fatal(err)
	}
	if err := eng.WriteGraph(); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(base, "out-full")
	generate(members, full)
	same(first, full, "refresh against a full generate")
}
