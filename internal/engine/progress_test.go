package engine_test

import (
	"context"
	"path/filepath"
	"reflect"
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
	var goReport *engine.Progress
	for i := range got {
		if got[i].Extractor == "go" {
			goReport = &got[i]
		}
	}
	if goReport == nil || goReport.Extracted != 3 || goReport.Found != 4 {
		t.Errorf("the go extractor's report = %+v, want the 3 it reads (two sources and go.mod) of 4", goReport)
	}

	last := got[len(got)-1]
	if last.Extracted != last.Found || last.Done != last.Stages || last.Stages == 0 {
		t.Errorf("the last report = %+v, want every file extracted and every announced stage done", last)
	}
	var stages []string
	for _, p := range got {
		if p.Stages != walk.Stages {
			t.Errorf("report %+v announces %d stages, the walk announced %d: the total must not move", p, p.Stages, walk.Stages)
		}
		if p.Stage != "" {
			stages = append(stages, p.Stage)
		}
	}
	if want := []string{"providers", "linking", "explainers", "renderers"}; !reflect.DeepEqual(stages, want) {
		t.Errorf("stages reported %v, want %v", stages, want)
	}
}

// A repository linked later in a cluster stops after its providers, and says
// so from the walk, so its share of the total is complete when it is done.
func TestGenerateSnapshotDeferredLinkingAnnouncesOneStage(t *testing.T) {
	repo := freezeTestRepo(t)
	var got []engine.Progress
	ctx := engine.WithProgress(context.Background(), func(p engine.Progress) { got = append(got, p) })
	eng := freezeTestEngine(t)
	eng.SetDeferLinking(true)
	if _, err := eng.GenerateSnapshot(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	last := got[len(got)-1]
	if last.Stages != 1 || last.Done != 1 || last.Stage != "providers" || last.Extracted != last.Found {
		t.Errorf("the last report = %+v, want the one announced stage done after every file", last)
	}
}
