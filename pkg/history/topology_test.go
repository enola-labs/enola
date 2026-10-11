package history

import (
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func rev(id, commit string) Entry {
	return Entry{ID: "sha256:" + id, At: "2026-08-03T10:00:00Z", Git: &facts.GitInfo{Commit: commit, Ref: "main"}}
}

func dirtyRev(id, commit string) Entry {
	e := rev(id, commit)
	e.Git.Dirty = true
	return e
}

// topoOf builds a topology from an explicit parent map, bypassing git — which is what makes
// every shape below testable without a repository.
func topoOf(entries []Entry, parents map[string][]string) Topology {
	rows := make([]Row, len(entries))
	for i, e := range entries {
		rows[i] = Row{Entry: e}
	}
	byCommit := map[string][]int{}
	for i, e := range entries {
		if c := e.Commit(); c != "" {
			byCommit[c] = append(byCommit[c], i)
		}
	}
	for i, e := range entries {
		commit := e.Commit()
		if siblings := byCommit[commit]; len(siblings) > 0 && siblings[0] != i {
			for k, idx := range siblings {
				if idx == i {
					rows[i].Parents = []int{siblings[k-1]}
					break
				}
			}
			continue
		}
		for _, a := range nearestObserved(commit, parents, byCommit) {
			idxs := byCommit[a]
			rows[i].Parents = append(rows[i].Parents, idxs[len(idxs)-1])
		}
		rows[i].Parents = reduce(rows[i].Parents, rows)
	}
	return Topology{Rows: rows, Source: SourceGit}
}

// parentsOf renders a topology's edges compactly, for assertions that read as the shape.
func parentsOf(t Topology) string {
	var parts []string
	for i, r := range t.Rows {
		parts = append(parts, fmt.Sprintf("%d<-%v", i, r.Parents))
	}
	return strings.Join(parts, " ")
}

// The property the whole file exists for: enola sees a SPARSE subset of commits, so the
// unobserved stretch between two observed ones must collapse to a single edge. Anything
// simpler — drawing recorded parent commits, or joining consecutive log entries — gets this
// wrong precisely where the graph is worth having.
func TestTopology_CollapsesUnobservedCommits(t *testing.T) {
	// c1 -> c2 -> c3 -> c4, with only c1 and c4 ever snapshotted.
	entries := []Entry{rev("aaa", "c1"), rev("bbb", "c4")}
	parents := map[string][]string{"c4": {"c3"}, "c3": {"c2"}, "c2": {"c1"}}

	got := topoOf(entries, parents)
	if want := "0<-[] 1<-[0]"; parentsOf(got) != want {
		t.Errorf("got %q, want %q — the gap between c1 and c4 must become one edge", parentsOf(got), want)
	}
}

// A branch and its base, both observed, then a merge: the merge descends from both sides.
func TestTopology_Merge(t *testing.T) {
	//      c2 (branch)
	//     /  \
	//   c1    c4 (merge)
	//     \  /
	//      c3 (main)
	entries := []Entry{rev("aaa", "c1"), rev("bbb", "c2"), rev("ccc", "c3"), rev("ddd", "c4")}
	parents := map[string][]string{
		"c2": {"c1"},
		"c3": {"c1"},
		"c4": {"c2", "c3"},
	}
	got := topoOf(entries, parents)
	if want := "0<-[] 1<-[0] 2<-[0] 3<-[1 2]"; parentsOf(got) != want {
		t.Errorf("got %q, want %q", parentsOf(got), want)
	}
}

// A merge whose sides share an observed ancestor must not ALSO draw a direct edge to it.
// Without the reduction the merge appears to descend from the base twice, which reads as a
// relationship that does not exist.
func TestTopology_ReducesRedundantEdges(t *testing.T) {
	// c1 observed; c2 (from c1) observed; c3 merges c2 and c1 directly.
	entries := []Entry{rev("aaa", "c1"), rev("bbb", "c2"), rev("ccc", "c3")}
	parents := map[string][]string{"c2": {"c1"}, "c3": {"c2", "c1"}}

	got := topoOf(entries, parents)
	if want := "0<-[] 1<-[0] 2<-[1]"; parentsOf(got) != want {
		t.Errorf("got %q, want %q — the edge to c1 is implied through c2", parentsOf(got), want)
	}
}

// Several revisions of ONE commit are a linear run, not a fan. An agent loop snapshotting
// the same working tree repeatedly is a sequence of observations of one state; drawing them
// as siblings off the commit would turn a session into a starburst.
func TestTopology_RevisionsOfOneCommitAreALine(t *testing.T) {
	entries := []Entry{
		rev("aaa", "c1"),
		dirtyRev("w1", "c2"),
		dirtyRev("w2", "c2"),
		rev("ccc", "c2"),
	}
	parents := map[string][]string{"c2": {"c1"}}

	got := topoOf(entries, parents)
	if want := "0<-[] 1<-[0] 2<-[1] 3<-[2]"; parentsOf(got) != want {
		t.Errorf("got %q, want %q", parentsOf(got), want)
	}
}

// A commit the repository no longer has (rewritten branch, dropped stash, shallow clone
// boundary) contributes no edge rather than failing the walk. A history outliving the
// commits it describes is what it is for.
func TestTopology_UnknownCommitYieldsNoEdge(t *testing.T) {
	entries := []Entry{rev("aaa", "gone"), rev("bbb", "c2")}
	parents := map[string][]string{"c2": {"alsogone"}}

	got := topoOf(entries, parents)
	if want := "0<-[] 1<-[]"; parentsOf(got) != want {
		t.Errorf("got %q, want %q — an unreachable ancestor must not invent an edge", parentsOf(got), want)
	}
}

// With no ancestry available at all, revisions are strung together in the order they were
// taken — and the topology says so, so a renderer can disclose that it is showing a
// timeline rather than a history.
func TestBuildTopology_FallsBackToTimeAndSaysSo(t *testing.T) {
	entries := []Entry{{ID: "sha256:a", At: "2026-08-03T10:00:00Z"}, {ID: "sha256:b", At: "2026-08-03T11:00:00Z"}}
	got := BuildTopology(entries, "")
	if got.Source != SourceTime {
		t.Errorf("Source = %q, want %q", got.Source, SourceTime)
	}
	if want := "0<-[] 1<-[0]"; parentsOf(got) != want {
		t.Errorf("got %q, want %q", parentsOf(got), want)
	}
}

// Without a repository, the parents each revision recorded at snapshot time are used —
// enough to join revisions whose parent commit was itself observed, and honest about being
// second best.
func TestBuildTopology_FallsBackToRecordedParents(t *testing.T) {
	a := rev("aaa", "c1")
	b := rev("bbb", "c2")
	b.Parents = []string{"c1"}

	got := BuildTopology([]Entry{a, b}, "/nonexistent-repo-path")
	if got.Source != SourceRecordedParents {
		t.Fatalf("Source = %q, want %q", got.Source, SourceRecordedParents)
	}
	if want := "0<-[] 1<-[0]"; parentsOf(got) != want {
		t.Errorf("got %q, want %q", parentsOf(got), want)
	}
}

func TestBuildTopology_EmptyIsNotAFailure(t *testing.T) {
	if got := BuildTopology(nil, "/repo"); len(got.Rows) != 0 {
		t.Errorf("want no rows, got %d", len(got.Rows))
	}
}
