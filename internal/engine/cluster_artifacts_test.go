package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// unionArtifacts are the artifacts a graph's directory holds that describe the whole
// graph; memberArtifacts describe the graph they are written with.
var (
	unionArtifacts  = []string{"facts.jsonl", "insights.json", "llm_context.md", "docs.jsonl", "flows.json"}
	memberArtifacts = []string{"snapshot.meta.json", "receipt.json"}
)

// outputFiles reads every file under dir except the extractor cache, which a run
// updates on purpose: it is the repository's own input cache, not output.
func outputFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if strings.HasPrefix(rel, "extractor_cache") {
			return nil
		}
		data, err := os.ReadFile(p)
		out[rel] = string(data)
		return err
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

// A cluster writes its graph once, into its own directory, and leaves every member's
// output dir exactly as it was: each member's own snapshot, previous/ and baseline/.
// The cluster's directory rotates its own previous/ by linking, and a member written
// alone afterwards leaves the cluster's directory as it was.
func TestClusterArtifacts_MembersAreLeftAsTheyWere(t *testing.T) {
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
	alone := newEngine()
	for _, repo := range []string{a, b, a} { // a twice: it has a previous/ of its own
		if _, err := alone.GenerateSnapshot(ctx, repo, false); err != nil {
			t.Fatal(err)
		}
		if err := alone.WriteGraph(); err != nil {
			t.Fatal(err)
		}
	}
	if err := alone.SetBaseline(a); err != nil {
		t.Fatal(err)
	}
	before := map[string]map[string]string{a: outputFiles(t, alone.OutputDir(a)), b: outputFiles(t, alone.OutputDir(b))}
	if len(before[a]) == 0 || before[a][filepath.Join(engine.BaselineSubdir, "facts.jsonl")] == "" {
		t.Fatalf("precondition: a should hold its own snapshot, previous/ and baseline/: %d files", len(before[a]))
	}

	cluster := newEngine()
	write := func() {
		t.Helper()
		if _, err := cluster.GenerateCluster(ctx, []string{a, b}, false); err != nil {
			t.Fatal(err)
		}
		if err := cluster.WriteGraph(); err != nil {
			t.Fatal(err)
		}
	}
	write()
	dir := cluster.GraphDir([]string{a, b})
	for _, name := range append(unionArtifacts, memberArtifacts...) {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("the cluster's directory has no %s: %v", name, err)
		}
	}
	first := map[string]os.FileInfo{}
	for _, name := range []string{"facts.jsonl", "insights.json", "snapshot.meta.json", "receipt.json", engine.MembersMetaFile, engine.RunMarkFile} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("the cluster's directory has no %s: %v", name, err)
		}
		first[name] = st
	}
	write() // rotates the first write into the cluster's previous/

	for _, repo := range []string{a, b} {
		after := outputFiles(t, cluster.OutputDir(repo))
		if len(after) != len(before[repo]) {
			t.Errorf("%s: %d output files before the cluster ran, %d after", filepath.Base(filepath.Dir(repo)), len(before[repo]), len(after))
		}
		for name, want := range before[repo] {
			if after[name] != want {
				t.Errorf("%s: the cluster changed %s", filepath.Base(filepath.Dir(repo)), name)
			}
		}
	}
	for name, st := range first {
		got, err := os.Stat(filepath.Join(dir, engine.PreviousSubdir, name))
		if err != nil || !os.SameFile(st, got) {
			t.Errorf("previous/%s of the cluster is not the first write, linked", name)
		}
	}

	// a alone again: the cluster's directory is not a's to write.
	keep := outputFiles(t, dir)
	if _, err := alone.GenerateSnapshot(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	if err := alone.WriteGraph(); err != nil {
		t.Fatal(err)
	}
	for name, want := range keep {
		if got := outputFiles(t, dir)[name]; got != want && !strings.HasPrefix(name, "history") {
			t.Errorf("writing a alone changed the cluster's %s", name)
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
