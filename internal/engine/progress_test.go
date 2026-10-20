package engine_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/engine"
)

// A snapshot asked with a reporter hears the walk's count and then each
// extractor's end, as files extracted of files found; the snapshot itself is
// the one it would have been with nobody listening.
func TestGenerateSnapshotReportsProgressAndProducesTheSameSnapshot(t *testing.T) {
	repo := freezeTestRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "# testmod\n")

	var got []engine.Progress
	ctx := engine.WithProgress(context.Background(), func(p engine.Progress) { got = append(got, p) })
	heard, err := freezeTestEngine(t).GenerateSnapshot(ctx, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	quiet, err := freezeTestEngine(t).GenerateSnapshot(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if heard.Meta.SnapshotID != quiet.Meta.SnapshotID || heard.Meta.FactCount != quiet.Meta.FactCount {
		t.Errorf("a reporter changed the snapshot: %s (%d facts) against %s (%d facts)",
			heard.Meta.SnapshotID, heard.Meta.FactCount, quiet.Meta.SnapshotID, quiet.Meta.FactCount)
	}

	if len(got) < 2 {
		t.Fatalf("heard %d reports, want the walk's and at least one extractor's: %+v", len(got), got)
	}
	walk := got[0]
	if walk.Extractor != "" || walk.Extracted != 0 || walk.Found != 4 || walk.Repo == "" {
		t.Errorf("the walk's report = %+v, want 0 of the 4 files found in the repository", walk)
	}
	last := got[len(got)-1]
	if last.Extractor != "go" || last.Extracted != 3 || last.Found != 4 {
		t.Errorf("the go extractor's report = %+v, want the 3 it reads (two sources and go.mod) of 4", last)
	}
}
