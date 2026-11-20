package command

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/bootstrap"
)

// pinRepo snapshots repo under the config doctor will resolve for it (the repo's own
// mcp-arch.yaml) and pins that snapshot as the baseline.
func pinRepo(t *testing.T, repo string) {
	t.Helper()
	cfgPath := filepath.Join(repo, "mcp-arch.yaml")
	if err := os.WriteFile(cfgPath, []byte("repo: "+repo+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.GenerateCluster(context.Background(), []string{repo}, false); err != nil {
		t.Fatal(err)
	}
	if err := eng.WriteGraph(); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetBaseline(repo); err != nil {
		t.Fatal(err)
	}
}

func doctorBaselineJSON(t *testing.T, repo string) (state, unusable string) {
	t.Helper()
	var out struct {
		State    string `json:"baseline_state"`
		Unusable string `json:"baseline_unusable"`
	}
	if err := json.Unmarshal([]byte(runDoctorCapturing(t, "--json", repo)), &out); err != nil {
		t.Fatal(err)
	}
	return out.State, out.Unusable
}

// A repository nobody ever pinned has nothing for the hooks to grade against. doctor
// said "comparable — the gate can grade against it" and --json "baseline_unusable": "",
// the same answer it gives a baseline that really is comparable.
func TestDoctor_AMissingBaselineIsNotReportedComparable(t *testing.T) {
	repo := writeGoRepo(t)

	text := runDoctorCapturing(t, repo)
	if strings.Contains(text, "comparable — the gate can grade against it") {
		t.Errorf("doctor called a missing baseline comparable:\n%s", text)
	}
	if !strings.Contains(text, "none pinned") {
		t.Errorf("doctor did not say no baseline is pinned:\n%s", text)
	}
	if state, unusable := doctorBaselineJSON(t, repo); state != string(baselineAbsent) || unusable == "" {
		t.Errorf("--json baseline_state=%q baseline_unusable=%q, want %q and a reason", state, unusable, baselineAbsent)
	}

	// The affirmative stays the affirmative.
	pinRepo(t, repo)
	text = runDoctorCapturing(t, repo)
	if !strings.Contains(text, "comparable — the gate can grade against it") {
		t.Errorf("doctor did not call a fresh pin comparable:\n%s", text)
	}
	if state, unusable := doctorBaselineJSON(t, repo); state != string(baselineComparable) || unusable != "" {
		t.Errorf("--json baseline_state=%q baseline_unusable=%q, want %q and no reason", state, unusable, baselineComparable)
	}
}
