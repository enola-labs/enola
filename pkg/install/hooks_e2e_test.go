package install

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This is the check that would have caught the Stop hook never firing, and the only
// kind that could have.
//
// The failure mode is a configuration that parses, reports success, and does
// nothing. Every cheaper check passed while it was broken: `enola hook stop`
// produced the right verdict when invoked by hand, the installer wrote the file it
// meant to write, and a unit test asserted the shape against the same belief that
// produced it. What settled it was ending a real session and looking at whether the
// hook ran. So this test installs the hooks the way a user does, ends a session with
// a real regression present, and asserts the verdict came out — the same discipline
// TestPublishedExample_StillDemonstratesWhatItClaims applies to examples/cross-repo.
//
// It is opt-in because it spawns a real agent session. ENOLA_E2E=1 runs it. The skip
// names its reason rather than passing silently: a test that quietly does nothing is
// the thing being tested here.
func TestStopHook_FiresInARealSession(t *testing.T) {
	if os.Getenv("ENOLA_E2E") != "1" {
		t.Skip("set ENOLA_E2E=1 to run: this spawns a real Claude Code session")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the hook wrapper is a POSIX shell script")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not on PATH: the Stop hook cannot be exercised end to end")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	work := t.TempDir()
	enola := buildEnola(ctx, t, work)
	repo := writeCyclePendingRepo(t, work)

	// Pin the baseline BEFORE the regression exists — this is the "before" the Stop
	// hook grades against. Pinned deliberately (no auto-pin marker), so the
	// SessionStart hook leaves it alone and the session cannot re-baseline the
	// regression away.
	runCLI(ctx, t, repo, enola, "baseline", "pin", repo)

	// Close the cycle. b already imports a; now a imports b.
	writeFile(t, filepath.Join(repo, "a", "a.go"), `package a

import "example.com/e2e/b"

// A now calls back into b, closing a cycle.
func A() string { return "a" + b.B() }
`)

	// The hook command is a wrapper around the real binary: it records that it ran
	// at all, and tees the hook's stdout. Those are two different questions — the
	// defect was that the hook never ran, not that it answered wrongly — and only
	// the first is visible from outside the session.
	log := filepath.Join(work, "hooks")
	if err := os.MkdirAll(log, 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(work, "enola-hook-wrapper")
	writeFile(t, wrapper, "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> '"+filepath.Join(log, "fired.log")+"'\n"+
		"'"+enola+"' \"$@\" 2>> '"+filepath.Join(log, "stderr.log")+"'"+
		" | tee -a '"+filepath.Join(log, "stdout.log")+"'\n")
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(Options{
		Scope:       ScopeLocal,
		RepoDir:     repo,
		HomeDir:     t.TempDir(),
		Hooks:       true,
		HookCommand: wrapper,
		Targets:     []string{"claude"},
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	settings := filepath.Join(repo, ".claude", "settings.json")
	session := exec.CommandContext(ctx, claude, "-p", "Say OK", "--settings", settings)
	session.Dir = repo
	if out, err := session.CombinedOutput(); err != nil {
		t.Fatalf("claude session failed: %v\n%s", err, out)
	}

	fired := readIfPresent(filepath.Join(log, "fired.log"))
	stdout := readIfPresent(filepath.Join(log, "stdout.log"))

	if !strings.Contains(fired, "hook stop") {
		t.Errorf("the Stop hook never ran.\nhooks that did run:\n%s\nsettings.json:\n%s",
			fired, readIfPresent(settings))
	}
	// Firing is necessary but not sufficient: a hook that runs and stays silent
	// leaves the session ungraded just as completely. The repo declares no policy, so
	// the cycle is REPORTED rather than failed: the finding still has to reach the
	// agent, which is the whole point of the hook.
	if !strings.Contains(stdout, "Cyclic dependency detected") {
		t.Errorf("the Stop hook produced no verdict for a repo with a real cycle.\n"+
			"stdout:\n%s\nstderr:\n%s", stdout, readIfPresent(filepath.Join(log, "stderr.log")))
	} else if n := strings.Count(stdout, "Cyclic dependency detected"); n != 1 {
		// Speaking ONCE is as much a part of the contract as speaking at all. A Stop
		// hook's output does not annotate a finished turn, it prevents the turn from
		// ending: the harness feeds the report back to the model and stops again. So a
		// hook that emits the same report twice over an unchanged tree is looping, and
		// the session ends on the harness's block-cap warning instead of on the report.
		//
		// That was issue #288, and this test was already reproducing it on every run
		// while unable to see it, because it asked whether the report appeared at all
		// rather than how many times. Counting is the entire difference.
		t.Errorf("the Stop hook emitted its report %d times in one session, want exactly 1: "+
			"it is re-grading an unchanged baseline at every Stop and will run until the "+
			"harness overrides it.\nstdout:\n%s", n, stdout)
	}
	// The second run is expected and must stay silent: the report, then one replay
	// carrying stop_hook_active. More than that means the suppression is not holding.
	if n := strings.Count(fired, "hook stop"); n > 2 {
		t.Errorf("the Stop hook ran %d times in one session, want at most 2 (one report and "+
			"one suppressed replay).\nhooks that ran:\n%s", n, fired)
	}
	// SessionStart was already correct, and is the control: if neither fired, the
	// session itself did not run the way this test assumes.
	if !strings.Contains(fired, "hook session-start") {
		t.Errorf("SessionStart did not fire either — the session did not run as assumed:\n%s", fired)
	}
}

// buildEnola compiles the binary under test. The hook invokes enola as a
// subprocess, so an e2e run has to exercise a real build rather than this package.
func buildEnola(ctx context.Context, t *testing.T, work string) string {
	t.Helper()
	bin := filepath.Join(work, "enola")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/enola")
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building enola: %v\n%s", err, out)
	}
	return bin
}

// writeCyclePendingRepo creates a two-package Go module with a single edge b -> a.
// Adding the opposite edge closes a cycle — a finding enola computes exactly, which is
// what the Stop hook reports even though no policy here enforces it (nothing fails by
// default; see check.DefaultFailExplainers).
func writeCyclePendingRepo(t *testing.T, work string) string {
	t.Helper()
	repo := filepath.Join(work, "repo")
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/e2e\n\ngo 1.25\n")
	writeFile(t, filepath.Join(repo, "a", "a.go"), `package a

// A is the leaf.
func A() string { return "a" }
`)
	writeFile(t, filepath.Join(repo, "b", "b.go"), `package b

import "example.com/e2e/a"

// B depends on a.
func B() string { return a.A() }
`)
	return repo
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runCLI(ctx context.Context, t *testing.T, dir, bin string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, out)
	}
}

func readIfPresent(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "<file not written>"
	}
	return string(b)
}
