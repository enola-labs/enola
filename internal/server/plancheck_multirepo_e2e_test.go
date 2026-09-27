package server_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// A plan is one repository's: its declarations govern it and a patch applies to its
// tree. In a multi-repo snapshot plan_check used to plan every target against the
// primary member, whichever was indexed last, and to measure that member's drift under
// the wrong file set. The target's own repository must be the one planned.
func TestE2E_PlanCheckPlansTheTargetsOwnRepository(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	parent := t.TempDir()
	sample := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), parent)
	gin := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_gin_sample"), parent)
	eng, cfg := newTestEngine(t)
	cfg.Repo = sample
	s := connect(t, eng, cfg)
	s.call(t, "generate_snapshot", map[string]any{"repo_paths": []string{sample, gin}})

	type report struct {
		Repo     string `json:"repo"`
		Snapshot struct {
			Staleness string `json:"staleness"`
		} `json:"snapshot"`
		Targets []struct {
			Target   string `json:"target"`
			Measured bool   `json:"measured"`
		} `json:"targets"`
	}
	plan := func(args map[string]any) (report, string) {
		t.Helper()
		out := text(s.call(t, "plan_check", args))
		var r report
		_ = json.Unmarshal([]byte(out), &r)
		return r, out
	}

	for _, repo := range []string{"go_sample", "go_gin_sample"} {
		r, out := plan(map[string]any{"paths": []string{repo + "/main.go"}})
		if r.Repo != repo {
			t.Errorf("a %s/ path was planned against %q:\n%s", repo, r.Repo, out)
			continue
		}
		if len(r.Targets) != 1 || r.Targets[0].Target != "main.go" || !r.Targets[0].Measured {
			t.Errorf("%s/main.go: targets %+v, want main.go measured in its own repository", repo, r.Targets)
		}
		if r.Snapshot.Staleness != "" {
			t.Errorf("%s: a fresh snapshot reported staleness %q", repo, r.Snapshot.Staleness)
		}
	}

	if r, out := plan(map[string]any{"paths": []string{"main.go"}, "repo": "go_gin_sample"}); r.Repo != "go_gin_sample" {
		t.Errorf("an explicit repo was not the one planned:\n%s", out)
	}
	if _, out := plan(map[string]any{"paths": []string{"main.go"}}); !strings.Contains(out, "names no repository") {
		t.Errorf("an unprefixed path in a multi-repo snapshot was planned without saying which repository:\n%s", out)
	}
	if _, out := plan(map[string]any{"paths": []string{"go_sample/main.go", "go_gin_sample/main.go"}}); !strings.Contains(out, "plan each separately") {
		t.Errorf("targets in two repositories were planned as one:\n%s", out)
	}
}
