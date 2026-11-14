package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// unionArtifacts are the same bytes for every member of a cluster; the rest describe
// the member they are written to.
var (
	unionArtifacts  = []string{"facts.jsonl", "insights.json", "llm_context.md", "docs.jsonl", "flows.json"}
	memberArtifacts = []string{"snapshot.meta.json", "receipt.json"}
)

// A cluster writes each union artifact once and links it into every member's dir, and
// rotates previous/ by linking too. Regenerating one member later replaces its own
// files and leaves the other members' and its own previous/ as they were.
func TestClusterArtifacts_LinkedOnceAndKept(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := t.TempDir()
	a := copyTree(t, filepath.Join("testdata", "repos", "go_sample"), filepath.Join(base, "a"))
	b := copyTree(t, filepath.Join("testdata", "repos", "go_sample"), filepath.Join(base, "b"))
	eng, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cluster := func() {
		t.Helper()
		if _, err := eng.GenerateCluster(ctx, []string{a, b}, false); err != nil {
			t.Fatal(err)
		}
		for _, repo := range []string{a, b} {
			if err := eng.WriteArtifacts(repo); err != nil {
				t.Fatal(err)
			}
		}
	}
	stat := func(path string) os.FileInfo {
		t.Helper()
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	outA, outB := eng.OutputDir(a), eng.OutputDir(b)

	rotated := []string{"facts.jsonl", "insights.json", "snapshot.meta.json", "receipt.json", engine.RunMarkFile}
	cluster()
	first := map[string]os.FileInfo{}
	for _, name := range rotated {
		first[name] = stat(filepath.Join(outA, name))
	}
	cluster() // rotates the first write into previous/
	for _, name := range unionArtifacts {
		if !os.SameFile(stat(filepath.Join(outA, name)), stat(filepath.Join(outB, name))) {
			t.Errorf("%s: the members hold two files, not one", name)
		}
	}
	for _, name := range memberArtifacts {
		if os.SameFile(stat(filepath.Join(outA, name)), stat(filepath.Join(outB, name))) {
			t.Errorf("%s: the members share one file, but it describes each member", name)
		}
	}
	for _, name := range rotated {
		if !os.SameFile(first[name], stat(filepath.Join(outA, engine.PreviousSubdir, name))) {
			t.Errorf("previous/%s is a copy, not a link", name)
		}
	}

	keepB := map[string][]byte{}
	for _, name := range append(unionArtifacts, memberArtifacts...) {
		keepB[name], _ = os.ReadFile(filepath.Join(outB, name))
	}
	alone, _ := os.ReadFile(filepath.Join(outA, "facts.jsonl"))

	// a alone: its union shrinks to itself.
	if _, err := eng.GenerateSnapshot(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	if err := eng.WriteArtifacts(a); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(outA, "facts.jsonl")); string(got) == string(alone) {
		t.Fatal("precondition: a alone should write other facts than the cluster")
	}
	for name, want := range keepB {
		if got, _ := os.ReadFile(filepath.Join(outB, name)); string(got) != string(want) {
			t.Errorf("writing a alone changed b's %s", name)
		}
	}
	for _, name := range unionArtifacts {
		if os.SameFile(stat(filepath.Join(outA, name)), stat(filepath.Join(outB, name))) {
			t.Errorf("%s: a's new file is still b's", name)
		}
	}
	// previous/ of a holds the second cluster write, the one b still has.
	for _, name := range []string{"facts.jsonl", "insights.json"} {
		got, _ := os.ReadFile(filepath.Join(outA, engine.PreviousSubdir, name))
		if string(got) != string(keepB[name]) {
			t.Errorf("previous/%s of a is not the cluster write it was rotated from", name)
		}
	}
}

// Two graphs sharing a repository write to its dir. When the graph holding that
// repository alone replaces its files after the cluster wrote them and before the
// cluster reached its next member, the next member must not link the replacement:
// it would hold one repository's facts under the cluster's meta.
func TestClusterArtifacts_NeverLinksAFileReplacedSince(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := t.TempDir()
	a := copyTree(t, filepath.Join("testdata", "repos", "go_sample"), filepath.Join(base, "a"))
	b := copyTree(t, filepath.Join("testdata", "repos", "go_sample"), filepath.Join(base, "b"))
	newEngine := func() *bootstrap.Engine {
		eng, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml")})
		if err != nil {
			t.Fatal(err)
		}
		return eng
	}
	ctx := context.Background()
	cluster, alone := newEngine(), newEngine()
	if _, err := cluster.GenerateCluster(ctx, []string{a, b}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := alone.GenerateSnapshot(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	if err := cluster.WriteArtifacts(a); err != nil {
		t.Fatal(err)
	}
	union := map[string][]byte{}
	for _, name := range unionArtifacts {
		union[name], _ = os.ReadFile(filepath.Join(cluster.OutputDir(a), name))
	}
	if err := alone.WriteArtifacts(a); err != nil {
		t.Fatal(err)
	}
	if err := cluster.WriteArtifacts(b); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"facts.jsonl", "insights.json"} {
		got, _ := os.ReadFile(filepath.Join(cluster.OutputDir(b), name))
		if string(got) != string(union[name]) {
			t.Errorf("%s of b is not the cluster's: it linked the file a alone wrote", name)
		}
	}
}

// A restore reads one write's facts, insights and meta, even while another graph
// holding the same repository writes its dir: a pool serving a repository alone and in
// a cluster at once. Each restore must be one graph or the other, never a mix.
func TestRestore_ReadsOneWriteWhileTheDirIsRewritten(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := t.TempDir()
	a := copyTree(t, filepath.Join("testdata", "repos", "go_sample"), filepath.Join(base, "a"))
	b := copyTree(t, filepath.Join("testdata", "repos", "go_sample"), filepath.Join(base, "b"))
	newEngine := func() *bootstrap.Engine {
		eng, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml")})
		if err != nil {
			t.Fatal(err)
		}
		return eng
	}
	ctx := context.Background()
	cluster, alone := newEngine(), newEngine()
	if _, err := cluster.GenerateCluster(ctx, []string{a, b}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := alone.GenerateSnapshot(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	type graph struct {
		id              string
		facts, insights int
	}
	of := func(e *bootstrap.Engine) graph {
		s := e.Snapshot()
		return graph{s.Meta.SnapshotID, e.Store().Count(), len(s.Insights)}
	}
	want := map[graph]bool{of(cluster): true, of(alone): true}
	if of(cluster).facts == of(alone).facts {
		t.Fatalf("precondition: the two graphs should differ in facts: %v", want)
	}
	if err := alone.WriteArtifacts(a); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 40; i++ {
			w := cluster
			if i%2 == 1 {
				w = alone
			}
			if err := w.WriteArtifacts(a); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	reads := 0
	for running := true; running; reads++ {
		select {
		case <-done:
			running = false
		default:
		}
		r := newEngine()
		if err := bootstrap.RestoreSnapshot(r, []string{a}); err != nil {
			t.Fatal(err)
		}
		if got := of(r); !want[got] {
			t.Fatalf("restore %d read a mix of two writes: %+v, want one of %v", reads, got, want)
		}
	}
	t.Logf("%d restores", reads)
}
