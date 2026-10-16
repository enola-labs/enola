package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelpRequested(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"pin"}, false},
		{[]string{"pin", "--help"}, true},
		{[]string{"pin", "-help"}, true},
		{[]string{"-h"}, true},
		{[]string{"lint", ".", "--help"}, true},
		{[]string{"--", "--help"}, false},
		{[]string{"--helper"}, false},
	}
	for _, tc := range cases {
		if got := HelpRequested(tc.args); got != tc.want {
			t.Errorf("HelpRequested(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// helpPaths are the command paths below the top-level subcommands that take --help.
// The top-level ones come from Subcommands(), so a new command is covered without
// being listed here.
var helpPaths = [][]string{
	{"upgrade"},
	{"baseline", "pin"}, {"baseline", "show"}, {"baseline", "clear"},
	{"constraints", "lint"}, {"constraints", "mine"}, {"constraints", "init"},
	{"constraints", "explain"}, {"constraints", "ledger"},
	{"cluster", "init"},
	{"history", "push"}, {"history", "pull"}, {"history", "verify"}, {"history", "gc"},
	{"providers", "list"}, {"providers", "fetch"},
	{"hook", "stop"}, {"hook", "session-start"},
}

// --help describes a command and does nothing else. Before this test, `baseline pin
// --help` pinned a baseline, `constraints lint --help` linted, `upgrade --help` replaced
// the binary, and `providers` and `hook` had no help at all, because a word before the
// flag stops the flag package from ever seeing it.
func TestHelpRunsNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "enola")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/enola")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	paths := append([][]string{}, helpPaths...)
	for _, c := range Subcommands() {
		paths = append(paths, []string{c})
	}
	for _, p := range paths {
		for _, flag := range []string{"--help", "-h"} {
			args := append(append([]string{}, p...), flag)
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				repo, home := t.TempDir(), t.TempDir()
				cmd := exec.Command(bin, args...)
				cmd.Dir = repo
				cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "ENOLA_NO_UPDATE_CHECK=1", "CI=1")
				out, err := cmd.CombinedOutput()
				if err != nil {
					if _, ok := err.(*exec.ExitError); !ok {
						t.Fatalf("run: %v", err)
					}
				}
				if !strings.Contains(string(out), "Usage: ") {
					t.Errorf("no usage block:\n%s", out)
				}
				for _, dir := range []string{repo, home} {
					if entries, _ := os.ReadDir(dir); len(entries) > 0 {
						t.Errorf("--help wrote %s into %s", entries[0].Name(), dir)
					}
				}
			})
		}
	}
}
