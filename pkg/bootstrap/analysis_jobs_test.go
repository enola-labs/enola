package bootstrap_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// Explainers and annotators running several at a time produce the graph and the
// findings they produce one after another, in the same order.
func TestAnalysisJobsDoNotChangeTheSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := filepath.Join("..", "..", "internal", "engine", "testdata", "repos")
	repos := []string{
		copyFixture(t, filepath.Join(src, "go_httpclient_multirepo", "api")),
		copyFixture(t, filepath.Join(src, "go_httpclient_multirepo", "consumer")),
		copyFixture(t, filepath.Join(src, "python_sample")),
	}
	type out struct{ id, insights string }
	run := func(jobs int) out {
		eng := newEngine(t)
		eng.Config().AnalysisJobs = jobs
		snaps, err := eng.GenerateCluster(context.Background(), repos, false)
		if err != nil {
			t.Fatalf("jobs=%d: %v", jobs, err)
		}
		snap := snaps[len(snaps)-1]
		ins, err := json.Marshal(snap.Insights)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Insights) == 0 {
			t.Fatalf("jobs=%d: no insights, nothing compared", jobs)
		}
		return out{snap.Meta.SnapshotID, string(ins)}
	}
	one, four := run(1), run(4)
	if one.id != four.id {
		t.Errorf("snapshot id differs: one at a time %s, four at a time %s", one.id, four.id)
	}
	if one.insights != four.insights {
		t.Error("insights differ between one and four analysis jobs")
	}
}
