package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveTarget_DirectoryArgumentIsTheRepo guards a silent-wrong-answer bug.
//
// `enola check /path/to/other/repo` used to fall through to config.Load, which cannot
// read a directory: it warned and used built-in defaults, whose `repo: "."` is the WORKING
// DIRECTORY. So the gate snapshotted whichever repo you happened to be standing in,
// compared it against THAT repo's baseline, and printed a confident verdict about a
// repository the caller never named.
func TestResolveTarget_DirectoryArgumentIsTheRepo(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module t\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stand somewhere else entirely, so a fallback to "." would be detectable.
	cwd := t.TempDir()
	restore := chdir(t, cwd)
	defer restore()

	tgt := testRunner().resolveTarget(repo)

	if len(tgt.repoPaths) != 1 {
		t.Fatalf("repoPaths = %v, want exactly one", tgt.repoPaths)
	}
	got, err := filepath.EvalSymlinks(tgt.repoPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("resolved repo = %q, want %q (a directory argument must name the repo, not the cwd)", got, want)
	}
	if !strings.Contains(tgt.configNote, repo) {
		t.Errorf("configNote = %q, must name the repo it resolved so the target is never a guess", tgt.configNote)
	}
}

// TestResolveTarget_PicksUpConfigInsideTheRepo — a config inside the target directory is
// used, so `baseline pin <repo>` and `check <repo>` resolve identically. If they differed,
// their ignore globs would differ, the ignore-glob hash would differ, and every diff would
// decline as incomparable.
func TestResolveTarget_PicksUpConfigInsideTheRepo(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module t\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(repo, "mcp-arch.yaml")
	if err := os.WriteFile(inner, []byte("ignore:\n  - \"vendor/**\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, t.TempDir())
	defer restore()

	tgt := testRunner().resolveTarget(repo)

	if !strings.Contains(tgt.configNote, inner) {
		t.Errorf("configNote = %q, want it to name the in-repo config %q", tgt.configNote, inner)
	}
	// The repo override must still win over anything the config says.
	got, _ := filepath.EvalSymlinks(tgt.repoPaths[0])
	want, _ := filepath.EvalSymlinks(repo)
	if got != want {
		t.Errorf("resolved repo = %q, want %q", got, want)
	}
}

// TestResolveTarget_FileArgumentIsAConfig — the other half of the disambiguation: a file
// is a config, so `enola check cluster.yaml` still works.
func TestResolveTarget_FileArgumentIsAConfig(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "svc")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(cfg, []byte("repos:\n  - svc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, t.TempDir())
	defer restore()

	tgt := testRunner().resolveTarget(cfg)

	if !strings.Contains(tgt.configNote, cfg) {
		t.Errorf("configNote = %q, want it to name the config", tgt.configNote)
	}
	// `repos:` entries resolve against the config's own directory, so the cluster member
	// must land next to the config rather than under the working directory.
	got, _ := filepath.EvalSymlinks(tgt.repoPaths[0])
	want, _ := filepath.EvalSymlinks(repo)
	if got != want {
		t.Errorf("resolved repo = %q, want %q", got, want)
	}
}

// TestUnknownArgHelp_DistinguishesTypoFromBadPath guards the message, not just the
// failure. `enola check <repo>` on a build without the subcommand was swallowed as a
// config path, fell back to defaults, and started an MCP server — it looked like it
// worked. Failing is necessary but not sufficient: telling someone who mistyped a command
// "no such file or directory" sends them to inspect their filesystem instead of their
// spelling.
func TestUnknownArgHelp_DistinguishesTypoFromBadPath(t *testing.T) {
	cases := []struct {
		name     string
		arg      string
		contains []string
	}{
		{"near-miss subcommand suggests it", "chekc", []string{"unknown command", "did you mean", "check"}},
		{"another near miss", "baselien", []string{"did you mean", "baseline"}},
		{"unrelated bare word lists the commands", "frobnicate", []string{"unknown command", "check", "baseline", "upgrade"}},
		{"unknown flag is named as a flag", "--genrate", []string{"unknown flag", "--help"}},
		{"a path explains the directory/file rule", "/no/such/path.yaml", []string{"neither a directory", "config"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := testRunner().UnknownArgHelp(tc.arg)
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("testRunner().UnknownArgHelp(%q) = %q, want it to mention %q", tc.arg, got, want)
				}
			}
		})
	}
}

// TestClosestSubcommand_DoesNotGuessWildly — a suggestion is only useful if it is
// plausible. Offering "check" for an unrelated word would be worse than no suggestion.
func TestClosestSubcommand_DoesNotGuessWildly(t *testing.T) {
	for _, arg := range []string{"frobnicate", "quux", "xyzzy"} {
		if got := testRunner().closestSubcommand(arg); got != "" {
			t.Errorf("testRunner().closestSubcommand(%q) = %q, want no suggestion", arg, got)
		}
	}
	for _, tc := range []struct{ arg, want string }{
		{"chekc", "check"},
		{"chek", "check"},
		{"baselien", "baseline"},
		{"upgrad", "upgrade"},
	} {
		if got := testRunner().closestSubcommand(tc.arg); got != tc.want {
			t.Errorf("testRunner().closestSubcommand(%q) = %q, want %q", tc.arg, got, tc.want)
		}
	}
}

func chdir(t *testing.T, dir string) func() {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chdir(prev) }
}
