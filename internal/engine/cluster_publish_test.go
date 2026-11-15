package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/engine"
)

// A reader never sees a cluster half built. While the second repository is being
// extracted, the published graph is still the one from before the cluster started:
// no member of the cluster in it, no store being appended to.
func TestGenerateCluster_ReadersSeeOnlyTheGraphBefore(t *testing.T) {
	before, repoA, repoB := freezeTestRepo(t), freezeTestRepo(t), freezeTestRepo(t)
	eng := freezeTestEngine(t)
	if _, err := eng.GenerateSnapshot(context.Background(), before, false); err != nil {
		t.Fatal(err)
	}
	published := eng.Store()

	sampled := false
	ctx := engine.WithProgress(context.Background(), func(p engine.Progress) {
		if p.Repo != repoB {
			return
		}
		sampled = true
		if eng.Store() != published {
			t.Errorf("while %s was extracted, readers saw a store other than the one published before", p.Repo)
		}
		if eng.Snapshot().Meta.RepoPath != before {
			t.Errorf("while %s was extracted, the published snapshot names %s", p.Repo, eng.Snapshot().Meta.RepoPath)
		}
	})
	if _, err := eng.GenerateCluster(ctx, []string{repoA, repoB}, false); err != nil {
		t.Fatal(err)
	}
	if !sampled {
		t.Fatal("no progress was reported for the second repository, so nothing was checked")
	}
	if eng.Store() == published || len(eng.RepoPaths()) != 2 {
		t.Fatalf("after the cluster, the published graph should be its union of 2 repositories, got %d", len(eng.RepoPaths()))
	}
}

// A repository that fails partway leaves the graph published before the cluster in
// place, and the error names it.
func TestGenerateCluster_FailureKeepsThePublishedGraph(t *testing.T) {
	before, repoA := freezeTestRepo(t), freezeTestRepo(t)
	missing := filepath.Join(t.TempDir(), "no-such-repo")
	eng := freezeTestEngine(t)
	if _, err := eng.GenerateSnapshot(context.Background(), before, false); err != nil {
		t.Fatal(err)
	}
	published, snap := eng.Store(), eng.Snapshot()

	_, err := eng.GenerateCluster(context.Background(), []string{repoA, missing}, false)
	var re *engine.RepoError
	if !errors.As(err, &re) || re.Repo != missing {
		t.Fatalf("want a RepoError naming %s, got %v", missing, err)
	}
	if eng.Store() != published || eng.Snapshot() != snap {
		t.Fatal("a failed cluster replaced the published graph")
	}

	// The engine is usable afterwards, and does not resume the abandoned build.
	if _, err := eng.GenerateCluster(context.Background(), []string{repoA}, false); err != nil {
		t.Fatal(err)
	}
	if got := len(eng.RepoPaths()); got > 1 {
		t.Fatalf("the next cluster carried %d repositories from the failed one", got)
	}
}

// A repository listed twice is replaced by its second turn, not doubled: the union
// holds what a cluster listing it once holds.
func TestGenerateCluster_RepeatedMemberIsReplaced(t *testing.T) {
	repoA, repoB := freezeTestRepo(t), freezeTestRepo(t)
	facts := func(repos ...string) []string {
		eng := freezeTestEngine(t)
		if _, err := eng.GenerateCluster(context.Background(), repos, false); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range eng.Store().FactsRef() {
			out = append(out, f.Repo+"|"+f.Kind+"|"+f.Name+"|"+f.File)
		}
		sort.Strings(out)
		return out
	}
	once, twice := facts(repoA, repoB), facts(repoA, repoB, repoA)
	if len(once) != len(twice) {
		t.Fatalf("listing a repository twice changed the union: %d facts vs %d", len(once), len(twice))
	}
	for i := range once {
		if once[i] != twice[i] {
			t.Fatalf("fact %d differs: %q vs %q", i, once[i], twice[i])
		}
	}
}

// Only the union's snapshot carries facts. An earlier turn's would be a view of the
// store's slice as it stood then, and the caller holding the returned snapshots would
// keep every array the union outgrew alive until it let go of them.
func TestGenerateCluster_OnlyTheUnionCarriesFacts(t *testing.T) {
	repoA, repoB, repoC := freezeTestRepo(t), freezeTestRepo(t), freezeTestRepo(t)
	eng := freezeTestEngine(t)
	snaps, err := eng.GenerateCluster(context.Background(), []string{repoA, repoB, repoC}, false)
	if err != nil {
		t.Fatal(err)
	}
	for i, snap := range snaps[:len(snaps)-1] {
		if snap.Facts != nil {
			t.Errorf("turn %d's snapshot holds %d facts of a store later turns grew", i, len(snap.Facts))
		}
	}
	if union := snaps[len(snaps)-1]; len(union.Facts) != eng.Store().Count() {
		t.Fatalf("the union's snapshot holds %d facts, the published store %d", len(union.Facts), eng.Store().Count())
	}
}
