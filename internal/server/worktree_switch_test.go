package server_test

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// gitCheckouts turns the session's repository into a git repository with a remote and
// adds a linked worktree of it beside it, returning the worktree's path.
func gitCheckouts(t *testing.T, repo string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", "git@github.com:acme/go_sample.git")
	run("add", "-A")
	run("commit", "-qm", "init")
	wt := filepath.Join(t.TempDir(), "go_sample-feature")
	run("worktree", "add", "-q", wt, "-b", "feature")
	return wt
}

var factsLine = regexp.MustCompile(`Facts: (\d+)`)

func factCount(t *testing.T, summary string) string {
	t.Helper()
	m := factsLine.FindStringSubmatch(summary)
	if m == nil {
		t.Fatalf("no fact count in summary:\n%s", summary)
	}
	return m[1]
}

// An agent that snapshots the main checkout and then moves into a worktree of it is
// switching checkouts. Both carry the remote's label and graph nodes are name-keyed, so
// joining them merged every symbol of one into the other. The worktree replaces the
// main checkout instead, whether the append was automatic or asked for.
func TestE2E_WorktreeReplacesItsMainCheckout(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		s := startInMemory(t)
		wt := gitCheckouts(t, s.repo)
		main := text(s.call(t, "generate_snapshot", map[string]any{"repo_path": s.repo}))

		args := map[string]any{"repo_path": wt}
		if explicit {
			args["append"] = true
		}
		res := s.call(t, "generate_snapshot", args)
		if res.IsError {
			t.Fatalf("append=%v: worktree snapshot errored: %s", explicit, text(res))
		}
		out := text(res)
		if !strings.Contains(out, "Switched checkouts") {
			t.Errorf("append=%v: switching to a worktree must say it replaced the main checkout; got:\n%s", explicit, out)
		}
		for _, banned := range []string{"Auto-appended", "Multi-repo mode active"} {
			if strings.Contains(out, banned) {
				t.Errorf("append=%v: a worktree of the loaded repository was joined to it (%q); got:\n%s", explicit, banned, out)
			}
		}
		if got, want := factCount(t, out), factCount(t, main); got != want {
			t.Errorf("append=%v: the graph holds %s facts after the switch, want the single checkout's %s", explicit, got, want)
		}
	}
}

// In a multi-repo graph the worktree takes over its repository's member, label and
// all, and the other members stay.
func TestE2E_WorktreeReplacesItsMemberInAMultiRepoGraph(t *testing.T) {
	s := startInMemory(t)
	wt := gitCheckouts(t, s.repo)
	s.snapshot(t)
	repoB := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "ts_sample"), t.TempDir())
	both := text(s.call(t, "generate_snapshot", map[string]any{"repo_path": repoB, "append": true}))

	out := text(s.call(t, "generate_snapshot", map[string]any{"repo_path": wt}))
	if !strings.Contains(out, "Switched checkouts") || strings.Contains(out, "Auto-appended") {
		t.Errorf("a worktree of a member must replace that member, not be merged in; got:\n%s", out)
	}
	if got, want := factCount(t, out), factCount(t, both); got != want {
		t.Errorf("the graph holds %s facts after the switch, want the two members' %s", got, want)
	}
	cov := text(s.call(t, "coverage_report", map[string]any{}))
	if !strings.Contains(cov, "ts_sample") || !strings.Contains(cov, "go_sample") {
		t.Errorf("both members must stay in the graph; got:\n%s", cov)
	}
	if strings.Contains(cov, "go_sample-feature") {
		t.Errorf("the worktree joined as a member of its own under its directory name; got:\n%s", cov)
	}
}
