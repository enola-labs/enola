package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func piOpts(t *testing.T, hooks bool) Options {
	t.Helper()
	o := opts(t, hooks)
	o.Targets = []string{"pi"}
	o.HookCommand = "/opt/enola/enola"
	return o
}

// TestPi_TemplateHasEachPlaceholderOnce — the installer substitutes by string replace, so
// a placeholder that went missing or got duplicated in an edit to the template would
// ship an extension that does not parse, or one that ignores --hooks.
func TestPi_TemplateHasEachPlaceholderOnce(t *testing.T) {
	for _, p := range []string{"__ENOLA_COMMAND__", "__ENOLA_HOOKS__"} {
		if n := strings.Count(piExtension, p); n != 1 {
			t.Errorf("%s appears %d times in pi_extension.js, want 1", p, n)
		}
	}
}

// TestPi_LocalWritesTheExtensionWithoutHooks — the extension is what gives Pi the tools
// at all, so it is written by a plain install, with the hooks switched off.
func TestPi_LocalWritesTheExtensionWithoutHooks(t *testing.T) {
	o := piOpts(t, false)
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(o.RepoDir, ".pi", "extensions", "enola.js")
	if a, _ := actionFor(rs, filepath.Join(".pi", "extensions", "enola.js")); a != ActionCreated {
		t.Fatalf("extension action = %q, want created; results: %v", a, rs)
	}
	src := read(t, path)
	if !strings.Contains(src, `const ENOLA = "/opt/enola/enola"`) {
		t.Error("the extension does not invoke the binary that installed it")
	}
	if !strings.Contains(src, "const HOOKS = false") || strings.Contains(src, PiHooksMarker) {
		t.Error("hooks are on in an install that did not ask for them")
	}
	if strings.Contains(src, "__ENOLA_") {
		t.Error("a placeholder survived substitution")
	}
}

func TestPi_HooksSwitchFollowsTheFlag(t *testing.T) {
	o := piOpts(t, true)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, filepath.Join(o.RepoDir, ".pi", "extensions", "enola.js")), PiHooksMarker) {
		t.Error("--hooks did not switch the extension's hooks on")
	}
	if !InstallsSessionHooks(o) {
		t.Error("a Pi install with --hooks runs the session hooks and must say so")
	}
}

// TestPi_QuotedBinaryPathIsUnquoted — hookBinary quotes a path with spaces for a shell.
// The extension spawns without one, so the quotes would become part of the filename.
func TestPi_QuotedBinaryPathIsUnquoted(t *testing.T) {
	o := piOpts(t, false)
	o.HookCommand = `"/Applications/My Tools/enola"`
	if src := piExtensionSource(o); !strings.Contains(src, `const ENOLA = "/Applications/My Tools/enola"`) {
		t.Errorf("binary path not unquoted: %s", src[strings.Index(src, "const ENOLA"):][:60])
	}
}

// TestPi_UninstallIsAByteForByteReversalLocally — enola created `.pi/`, so it goes with
// the extension, and nothing else in the repository is touched.
func TestPi_UninstallIsAByteForByteReversalLocally(t *testing.T) {
	o := piOpts(t, true)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if left := residue(t, o.RepoDir); len(left) != 0 {
		t.Errorf("uninstall left %v behind", left)
	}
}

// TestPi_UninstallKeepsTheUsersOtherExtensions — `.pi/extensions/` is shared with
// whatever else the user loads; only enola's file goes.
func TestPi_UninstallKeepsTheUsersOtherExtensions(t *testing.T) {
	o := piOpts(t, false)
	mine := filepath.Join(o.RepoDir, ".pi", "extensions", "mine.js")
	writeFile(t, mine, "export default function () {}\n")
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("uninstall removed the user's own extension: %v", err)
	}
}

func TestPi_GlobalSkipsWithoutPi(t *testing.T) {
	o := piOpts(t, false)
	o.Scope = ScopeGlobal
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.HomeDir, ".pi")); !os.IsNotExist(err) {
		t.Errorf("~/.pi was created for a user who does not have Pi (err=%v)", err)
	}
}

func TestPi_GlobalWritesBlockAndExtension(t *testing.T) {
	o := piOpts(t, false)
	o.Scope = ScopeGlobal
	if err := os.MkdirAll(filepath.Join(o.HomeDir, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"AGENTS.md", filepath.Join("extensions", "enola.js")} {
		if _, err := os.Stat(filepath.Join(o.HomeDir, ".pi", "agent", rel)); err != nil {
			t.Errorf("global install did not write ~/.pi/agent/%s: %v", rel, err)
		}
	}
}

// TestPi_TrustNoticeIsLocalOnly — a global extension loads without project trust, so
// telling that user to trust something would send them looking for a prompt that never
// comes.
func TestPi_TrustNoticeIsLocalOnly(t *testing.T) {
	o := piOpts(t, false)
	if !InstallsPiExtension(o) {
		t.Error("a local Pi install writes a trust-gated extension")
	}
	o.Scope = ScopeGlobal
	if InstallsPiExtension(o) {
		t.Error("a global Pi install is not trust-gated")
	}
	o.Scope = ScopeLocal
	o.Targets = []string{"claude"}
	if InstallsPiExtension(o) {
		t.Error("a run without the pi target writes no extension")
	}
}
