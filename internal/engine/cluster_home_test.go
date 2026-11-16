package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// A cluster's own directory holds the union once, the union's meta, a receipt with an
// entry per member, and each member's own provenance in members.json: everything a
// reader used to take from the members' copies.
func TestClusterHome_HoldsTheUnionAndEveryMembersProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := t.TempDir()
	a := copyTree(t, filepath.Join("testdata", "repos", "go_sample"), filepath.Join(base, "a"))
	b := copyTree(t, filepath.Join("testdata", "repos", "go_sample"), filepath.Join(base, "b"))
	eng, cfg, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.GenerateCluster(context.Background(), []string{a, b}, false); err != nil {
		t.Fatal(err)
	}
	if err := eng.WriteGraph(); err != nil {
		t.Fatal(err)
	}

	dir := cfg.GraphOutputDir([]string{a, b})
	if !strings.HasPrefix(dir, filepath.Join(home, ".enola", "graphs")) {
		t.Fatalf("cluster directory %s is not under ~/.enola/graphs", dir)
	}
	for _, name := range unionArtifacts {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s missing from the cluster's directory: %v", name, err)
		}
	}

	var meta facts.SnapshotMeta
	readJSON(t, filepath.Join(dir, "snapshot.meta.json"), &meta)
	if meta.FactCount == 0 || meta.OutputHashes["facts.jsonl"] == "" {
		t.Errorf("cluster meta has no facts or output hashes: %+v", meta.OutputHashes)
	}

	var rcpt facts.Receipt
	readJSON(t, filepath.Join(dir, "receipt.json"), &rcpt)
	if len(rcpt.Members) != 2 {
		t.Errorf("cluster receipt has %d member entries, want 2", len(rcpt.Members))
	}

	members, err := engine.ReadMembersMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{a, b} {
		m, ok := members[repo]
		if !ok {
			t.Errorf("members.json has no entry for %s (has %d)", repo, len(members))
			continue
		}
		if len(m.FileHashes) == 0 {
			t.Errorf("%s: no file hashes, so drift could not be read from the cluster's directory", repo)
		}
		if m.FactCount != 0 || m.OutputHashes != nil {
			t.Errorf("%s: member entry carries union fields; they belong to snapshot.meta.json", repo)
		}
	}
}

// A graph of one repository has no cluster directory, and nothing is written for it.
func TestClusterHome_NothingForOneRepository(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	a := copyTree(t, filepath.Join("testdata", "repos", "go_sample"), filepath.Join(t.TempDir(), "a"))
	eng, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.GenerateCluster(context.Background(), []string{a}, false); err != nil {
		t.Fatal(err)
	}
	if err := eng.WriteGraph(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(home, ".enola", "graphs"))
	for _, e := range entries {
		if strings.Contains(e.Name(), "-cluster-") {
			t.Errorf("a single repository wrote a cluster directory: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(eng.OutputDir(a), "facts.jsonl")); err != nil {
		t.Errorf("a single repository's graph is not in its own output dir: %v", err)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
