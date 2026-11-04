package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	pkghistory "github.com/enola-labs/enola/pkg/history"
)

// writeSnapshotDir writes the parts of an output directory the recorder reads: facts and
// the meta carrying the snapshot id.
func writeSnapshotDir(t *testing.T, dir, id string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, n := range names {
		line, _ := json.Marshal(facts.Fact{Kind: facts.KindSymbol, Name: n, File: n + ".go"})
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "facts.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(facts.SnapshotMeta{SnapshotID: id})
	if err := os.WriteFile(filepath.Join(dir, "snapshot.meta.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
}

func recordOf(repo, id string, names ...string) Record {
	snap := &facts.Snapshot{}
	for _, n := range names {
		snap.Facts = append(snap.Facts, facts.Fact{Kind: facts.KindSymbol, Name: n, File: n + ".go"})
	}
	return Record{
		RepoPath:    repo,
		Meta:        facts.SnapshotMeta{SnapshotID: id, GeneratedAt: "2026-11-04T10:00:00Z"},
		Snapshot:    snap,
		PreviousDir: filepath.Join(repo, ".enola", "previous"),
	}
}

func lastSummary(t *testing.T, root string) pkghistory.Summary {
	t.Helper()
	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	return entries[len(entries)-1].Summary
}

// A repository snapshotted alone after its cluster holds the cluster's union in
// previous/. Its delta against that is the step between two scopes, not a change, and
// was logged as tens of thousands of facts vanishing; it is recorded without a delta.
func TestRecord_PreviousOfAnotherScopeIsNoBase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	prev := filepath.Join(repo, ".enola", "previous")
	r := NewRecorder(RecorderOptions{})
	root, _ := pkghistory.Root(repo, "")

	r.Record(recordOf(repo, "r1", "a"))
	if s := lastSummary(t, root); !s.Initial {
		t.Fatalf("first revision: %+v, want initial", s)
	}

	writeSnapshotDir(t, prev, "the-cluster", "a", "b", "c", "d")
	r.Record(recordOf(repo, "r2", "a", "e"))
	if s := lastSummary(t, root); !s.NoBase || s.Initial || s.FactsRemoved != 0 {
		t.Fatalf("after a cluster run: %+v, want no base and no delta", s)
	}

	writeSnapshotDir(t, prev, "r2", "a", "e")
	r.Record(recordOf(repo, "r3", "a", "e", "f"))
	if s := lastSummary(t, root); s.NoBase || s.Initial || s.FactsAdded != 1 {
		t.Fatalf("after its own revision: %+v, want +1 fact", s)
	}
}

// A cluster is recorded once, into its own history, and its predecessor is found in
// whichever member directory holds it: the member written first may have been
// snapshotted alone since, while another still holds the last union.
func TestRecord_ClusterHistoryIsItsOwn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	members := []string{a, b}
	r := NewRecorder(RecorderOptions{})
	cluster := func(id string, names ...string) Record {
		rec := recordOf(a, id, names...)
		rec.Members = members
		rec.PriorDirs = []string{
			filepath.Join(a, ".enola", "previous"),
			filepath.Join(b, ".enola", "previous"), filepath.Join(b, ".enola"),
		}
		return rec
	}

	r.Record(cluster("c1", "a", "b"))
	clusterRoot, _ := pkghistory.ClusterRoot(members, "")
	if s := lastSummary(t, clusterRoot); !s.Initial {
		t.Fatalf("first cluster revision: %+v, want initial", s)
	}
	for _, m := range members {
		root, _ := pkghistory.Root(m, "")
		if _, err := pkghistory.Read(root); err == nil {
			t.Errorf("member %s got the cluster's revision", m)
		}
	}

	writeSnapshotDir(t, filepath.Join(a, ".enola", "previous"), "a-alone", "a")
	writeSnapshotDir(t, filepath.Join(b, ".enola"), "c1", "a", "b")
	r.Record(cluster("c2", "a", "b", "c"))
	if s := lastSummary(t, clusterRoot); s.NoBase || s.FactsAdded != 1 || s.FactsRemoved != 0 {
		t.Fatalf("second cluster revision: %+v, want +1 fact against c1", s)
	}
}

// With the revisions' contents stored, a predecessor that is on disk nowhere is rebuilt
// from the history, and the revision after a switch of scope gets its real delta.
func TestRecord_PredecessorRebuiltFromTheHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	r := NewRecorder(RecorderOptions{Blobs: true})
	root, _ := pkghistory.Root(repo, "")
	withFacts := func(id string, names ...string) Record {
		rec := recordOf(repo, id, names...)
		rec.FactsPath = filepath.Join(t.TempDir(), "facts.jsonl")
		writeSnapshotDir(t, filepath.Dir(rec.FactsPath), id, names...)
		return rec
	}

	r.Record(withFacts("r1", "a"))
	writeSnapshotDir(t, filepath.Join(repo, ".enola", "previous"), "the-cluster", "a", "b", "c", "d")
	r.Record(withFacts("r2", "a", "e"))
	if s := lastSummary(t, root); s.NoBase || s.Initial || s.FactsAdded != 1 || s.FactsRemoved != 0 {
		t.Fatalf("after a cluster run: %+v, want +1 fact against the rebuilt r1", s)
	}
}
