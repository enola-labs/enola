package engine_test

import (
	"context"
	"testing"
)

// A cluster's published meta is the last turn's; the meta written into a member's
// dir is that member's. A consumer grading against a baseline pinned from the
// anchor's dir must read the anchor's meta, and both share the union's snapshot id.
func TestMetaFor_ReturnsTheMembersOwnProvenanceOverTheUnion(t *testing.T) {
	repoA, repoB := freezeTestRepo(t), freezeTestRepo(t)
	eng := freezeTestEngine(t)
	if _, err := eng.GenerateCluster(context.Background(), []string{repoA, repoB}, false); err != nil {
		t.Fatal(err)
	}
	published := eng.Snapshot().Meta
	anchor := eng.MetaFor(repoA)
	if anchor.RepoPath != repoA {
		t.Fatalf("MetaFor(anchor) names %q, want %q", anchor.RepoPath, repoA)
	}
	if published.RepoPath != repoB {
		t.Fatalf("published meta names %q, want the last turn %q", published.RepoPath, repoB)
	}
	if anchor.SnapshotID == "" || anchor.SnapshotID != published.SnapshotID {
		t.Fatalf("member and union disagree on the snapshot id: %q vs %q", anchor.SnapshotID, published.SnapshotID)
	}
	if stranger := eng.MetaFor(t.TempDir()); stranger.RepoPath != published.RepoPath {
		t.Fatalf("an unknown path must fall back to the published meta, got %q", stranger.RepoPath)
	}
}
