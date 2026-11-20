package command

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
	"github.com/enola-labs/enola/pkg/cli"
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

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = prev }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// `baseline show` is the cheap question before the expensive one. It printed the
// baseline's provenance and exited 0 for a baseline `check` then declined, because it
// never asked whether the baseline could be graded against.
func TestBaselineShow_ReportsTheVerdictCheckWouldReach(t *testing.T) {
	repo := writeGoRepo(t)
	pinRepo(t, repo) // pinned under the repo's own config: no extra ignore globs
	r := New(cli.Binary{Name: "enola"}, "upgrade")

	var code int
	out := captureStdout(t, func() { code = r.baselineShow(filepath.Join(repo, "mcp-arch.yaml")) })
	if code != 0 || !strings.Contains(out, "comparable — check can grade against it") {
		t.Errorf("a baseline pinned under the same config: exit %d, output:\n%s", code, out)
	}

	other := filepath.Join(t.TempDir(), "other.yaml")
	if err := os.WriteFile(other, []byte("repo: "+repo+"\nignore: ['vendor/**', 'node_modules/**']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() { code = r.baselineShow(other) })
	if code != check.StatusIncomparable.ExitCode() {
		t.Errorf("exit %d under another ignore set, want %d as check gives", code, check.StatusIncomparable.ExitCode())
	}
	if !strings.Contains(out, "NOT COMPARABLE") || !strings.Contains(out, "ignore_globs") {
		t.Errorf("show did not say why check will decline:\n%s", out)
	}
	if !strings.Contains(out, "Facts:") {
		t.Errorf("the provenance must stay:\n%s", out)
	}
}
