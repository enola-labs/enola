package command

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A subcommand path that names nothing on disk is rejected, as the top level rejects it.
// It used to be handed on as a config path; the lookup fell back to built-in defaults,
// whose `repo: .` is the working directory, so `touched` or `check` with a mistyped path
// reported on whichever repository the caller stood in and exited as though it had worked.
//
// resolveTarget exits the process, so the call runs in a re-executed copy of this test
// binary, standing in a directory that would be analysed if the path fell through.
func TestResolveTargetRejectsAPathThatNamesNothing(t *testing.T) {
	if os.Getenv("ENOLA_RESOLVE_TARGET_CHILD") == "1" {
		tgt := testRunner().resolveTarget(os.Getenv("ENOLA_RESOLVE_TARGET_ARG"))
		// Reaching here is the bug: report what was analysed instead.
		_, _ = os.Stdout.WriteString("RESOLVED " + strings.Join(tgt.repoPaths, ",") + "\n")
		os.Exit(0)
	}

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "a.ts"), []byte("export function alpha() { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "no", "such", "path")

	cmd := exec.Command(os.Args[0], "-test.run=^TestResolveTargetRejectsAPathThatNamesNothing$")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "ENOLA_RESOLVE_TARGET_CHILD=1", "ENOLA_RESOLVE_TARGET_ARG="+missing)
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("a path that names nothing was accepted (err=%v); the working directory was analysed instead:\n%s", err, out)
	}
	if exit.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2 (the command did not run):\n%s", exit.ExitCode(), out)
	}
	if strings.Contains(string(out), "RESOLVED") {
		t.Errorf("the target resolved despite the rejection:\n%s", out)
	}
	if !strings.Contains(string(out), "is neither a directory (a repository) nor an existing file (a config)") {
		t.Errorf("the rejection does not say what was wrong with the path:\n%s", out)
	}
	if !strings.Contains(string(out), missing) {
		t.Errorf("the rejection does not name the path:\n%s", out)
	}
}
