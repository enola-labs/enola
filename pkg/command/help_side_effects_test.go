package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/cli"
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

// --help describes a command and does nothing else. Before this test, `baseline pin
// --help` pinned a baseline, `constraints lint --help` linted, `upgrade --help` replaced
// the binary, and `providers` and `hook` had no help at all, because a word before the
// flag stops the flag package from ever seeing it.
func TestHelpRunsNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildEnola(t)
	for _, p := range allHelpPaths() {
		for _, tail := range [][]string{{"--help"}, {"-h"}, {".", "--help"}} {
			args := append(slices.Clone(p), tail...)
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				repo, home := t.TempDir(), t.TempDir()
				res := runEnola(t, bin, repo, home, args...)
				if !strings.Contains(res.stderr+res.stdout, "Usage: ") {
					t.Errorf("no usage block:\n%s%s", res.stdout, res.stderr)
				}
				if !strings.Contains(res.stderr+res.stdout, "\nExamples:\n") {
					t.Errorf("no examples:\n%s%s", res.stdout, res.stderr)
				}
				if n := strings.Count(res.stderr+res.stdout, "Usage: "); n != 1 {
					t.Errorf("the page is printed %d times", n)
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

// allHelpPaths is every command path: upgrade, the top-level subcommands, and theirs.
func allHelpPaths() [][]string {
	paths := [][]string{{"upgrade"}}
	for _, c := range Subcommands() {
		paths = append(paths, []string{c})
		for _, sub := range subcommandsOf[c] {
			paths = append(paths, []string{c, sub})
		}
	}
	return paths
}

func buildEnola(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "enola")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/enola")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

type enolaRun struct {
	stdout, stderr string
	code           int
}

// runEnola runs the binary in dir under its own HOME, with the update check off so
// nothing it prints depends on the network or the clock.
func runEnola(t *testing.T, bin, dir, home string, args ...string) enolaRun {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "ENOLA_NO_UPDATE_CHECK=1", "CI=1")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	return enolaRun{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

func TestHelpArgs(t *testing.T) {
	r := New(cli.Binary{Name: "enola"}, "upgrade")
	cases := []struct {
		in, want []string
		err      string
	}{
		{in: nil, want: nil},
		{in: []string{"flows"}, want: []string{"flows"}},
		{in: []string{"help"}, want: []string{"--help"}},
		{in: []string{"help", "help"}, want: []string{"--help"}},
		{in: []string{"help", "flows"}, want: []string{"flows", "--help"}},
		{in: []string{"help", "baseline", "pin"}, want: []string{"baseline", "pin", "--help"}},
		{in: []string{"help", "upgrade"}, want: []string{"upgrade", "--help"}},
		{in: []string{"--help", "flows"}, want: []string{"flows", "--help"}},
		{in: []string{"-h", "constraints", "mine"}, want: []string{"constraints", "mine", "--help"}},
		// --help already meant the top-level page whatever followed; it still does.
		{in: []string{"--help"}, want: []string{"--help"}},
		{in: []string{"--help", "."}, want: []string{"--help", "."}},
		{in: []string{"--help", "--json"}, want: []string{"--help", "--json"}},
		{in: []string{"--help", "flwos"}, want: []string{"--help", "flwos"}},
		{in: []string{"help", "flwos"}, err: `did you mean "flows"`},
		{in: []string{"help", "zzzzzzzz"}, err: "no help for"},
	}
	for _, tc := range cases {
		got, err := r.HelpArgs(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("HelpArgs(%q) error = %v, want %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("HelpArgs(%q) = %q, %v, want %q", tc.in, got, err, tc.want)
		}
	}
}

// Every spelling of help reaches the same page: same stdout, same stderr, same exit
// code, byte for byte, for every command path.
func TestHelpSpellingsAgree(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildEnola(t)
	dir, home := t.TempDir(), t.TempDir()

	top := runEnola(t, bin, dir, home, "--help")
	for _, args := range [][]string{{"help"}, {"-h"}, {"help", "help"}, {"--help", dir}} {
		if got := runEnola(t, bin, dir, home, args...); got != top {
			t.Errorf("%q differs from --help: exit %d vs %d", args, got.code, top.code)
		}
	}

	for _, p := range allHelpPaths() {
		want := runEnola(t, bin, dir, home, append(slices.Clone(p), "--help")...)
		for _, lead := range []string{"help", "--help", "-h"} {
			args := append([]string{lead}, p...)
			if got := runEnola(t, bin, dir, home, args...); got != want {
				t.Errorf("%q differs from %q --help: exit %d vs %d\n--- got\n%s%s\n--- want\n%s%s",
					args, p, got.code, want.code, got.stdout, got.stderr, want.stdout, want.stderr)
			}
		}
	}

	if got := runEnola(t, bin, dir, home, "help", "flwos"); got.code == 0 || !strings.Contains(got.stderr, `did you mean "flows"`) {
		t.Errorf("help flwos: exit %d, stderr:\n%s", got.code, got.stderr)
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Errorf("help wrote %s into the working directory", entries[0].Name())
	}
}

// Every command has a page, and every page a See also line points at exists, so the
// links between pages lead somewhere.
func TestHelpPagesAreComplete(t *testing.T) {
	for _, c := range append(Subcommands(), "upgrade") {
		if len(helpPages[c].examples) == 0 {
			t.Errorf("%s has no examples", c)
		}
	}
	for path, page := range helpPages {
		if len(page.examples) == 0 {
			t.Errorf("%s has no examples", path)
		}
		for _, ref := range page.seeAlso {
			if _, ok := helpPages[ref]; !ok {
				t.Errorf("%s: see also %q, which has no page", path, ref)
			}
		}
	}
}

// The footer belongs to help. A bad flag or a missing argument prints the same usage
// block, and there the error stays as short as it was.
func TestHelpFooterOnlyOnHelp(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildEnola(t)
	dir, home := t.TempDir(), t.TempDir()
	for _, args := range [][]string{{"check", "--no-such-flag"}, {"baseline"}, {"history"}, {"cluster"}, {"constraints", "explain"}} {
		if got := runEnola(t, bin, dir, home, args...); strings.Contains(got.stdout+got.stderr, "Examples:") {
			t.Errorf("%q printed the help footer:\n%s", args, got.stderr)
		}
	}
}

// Every example names flags its command accepts. Each runs with --help appended: the
// flag package reads flags left to right, so an unknown one fails before --help stops
// the run, and nothing executes either way. Examples of the top-level flags are left
// out, since --generate and friends do not stop at --help.
func TestHelpExamplesParse(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildEnola(t)
	dir, home := t.TempDir(), t.TempDir()
	for path, page := range helpPages {
		for _, ex := range page.examples {
			cmd, _, _ := strings.Cut(ex.Command, " > ")
			args := splitExample(strings.TrimPrefix(cmd, "enola "))
			if strings.HasPrefix(args[0], "-") {
				continue
			}
			got := runEnola(t, bin, dir, home, append(args, "--help")...)
			if strings.Contains(got.stderr, "not defined") || !strings.Contains(got.stderr+got.stdout, "Usage: ") {
				t.Errorf("%s: example %q does not parse:\n%s", path, ex.Command, got.stderr)
			}
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Errorf("an example wrote %s into the working directory", entries[0].Name())
	}
}

// splitExample splits a command line on spaces, keeping a single-quoted argument whole.
func splitExample(s string) []string {
	var args []string
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		if rest, ok := strings.CutPrefix(s, "'"); ok {
			arg, after, _ := strings.Cut(rest, "'")
			args, s = append(args, arg), after
			continue
		}
		arg, after, _ := strings.Cut(s, " ")
		args, s = append(args, arg), after
	}
	return args
}
