package cli

import (
	"strings"
	"testing"
)

func testBinary() Binary {
	return Binary{
		Name:       "enola",
		CmdPackage: "./cmd/enola",
		VersionVar: "github.com/enola-labs/enola/internal/version.Version",
	}
}

func renderDefault(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	RenderHelp(&b, DefaultHelp(testBinary()))
	return b.String()
}

func TestDefaultHelp_MentionsEveryFlag(t *testing.T) {
	out := renderDefault(t)
	for _, flag := range []string{"--generate", "--explain", "--list", "--status", "--status --all", "--version", "--help, -h"} {
		if !strings.Contains(out, flag) {
			t.Errorf("flag %q missing from default help", flag)
		}
	}
	for _, title := range []string{"USAGE", "FLAGS", "CONFIG_PATH", "EXAMPLES", "MCP CONFIGURATION", "BUILD"} {
		if !strings.Contains(out, title) {
			t.Errorf("section %q missing from default help", title)
		}
	}
}

// The shared help is what the OSS binary prints verbatim, so it must never
// describe anything only a wrapper provides. (The dashboard is not in this list:
// both binaries serve one, so DefaultHelp documents it.)
func TestDefaultHelp_NoWrapperConcepts(t *testing.T) {
	out := strings.ToLower(renderDefault(t))
	for _, term := range []string{"license", "activate", "enterprise"} {
		if strings.Contains(out, term) {
			t.Errorf("default help must not mention %q:\n%s", term, out)
		}
	}
}

// The claim under test is about the RENDERER — an empty block is omitted rather than
// printed as a bare heading. It used to assert this against DefaultHelp, which happened
// to declare no commands; DefaultHelp now documents `check` and `baseline`, so the spec
// is emptied explicitly instead. Otherwise the test would be pinning the command list
// rather than the rendering rule, and any new engine subcommand would "fail" it.
func TestRenderHelp_NoCommandsBlockWhenEmpty(t *testing.T) {
	spec := DefaultHelp(testBinary())
	spec.Commands = nil

	var b strings.Builder
	RenderHelp(&b, spec)

	if strings.Contains(b.String(), "COMMANDS") {
		t.Error("COMMANDS block should be omitted when no commands are declared")
	}
}

// The gate's subcommands are engine features, not wrapper features, so the shared help
// must describe them — every binary built on this engine serves them.
func TestDefaultHelp_DocumentsGateCommands(t *testing.T) {
	out := renderDefault(t)
	for _, want := range []string{"COMMANDS", "check", "baseline"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing from default help:\n%s", want, out)
		}
	}
	// The exit codes are the CLI's contract with CI; the help is where a reader looks
	// for them before wiring up a pipeline.
	for _, code := range []string{"0 clean", "1 regression", "2 error", "3 declined"} {
		if !strings.Contains(out, code) {
			t.Errorf("exit code %q not documented in help:\n%s", code, out)
		}
	}
}

func TestDefaultHelp_PromotesDashboardLifecycle(t *testing.T) {
	out := renderDefault(t)
	for _, want := range []string{
		"dashboard [--open]",
		"dashboard --open",
		"stop with Ctrl-C",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard help %q missing:\n%s", want, out)
		}
	}
	for _, removed := range []string{"dashboard status", "dashboard stop", "--foreground"} {
		if strings.Contains(out, removed) {
			t.Errorf("dashboard help still advertises removed command %q:\n%s", removed, out)
		}
	}

	dashboard := strings.Index(out, "\nDASHBOARD\n")
	gate := strings.Index(out, "\nTHE GATE")
	if dashboard < 0 || gate < 0 || dashboard > gate {
		t.Errorf("dashboard detail should appear before the gate documentation")
	}
}

func TestRenderHelp_MultiLineDescriptionsAlign(t *testing.T) {
	var b strings.Builder
	spec := DefaultHelp(testBinary())
	spec.Flags = []FlagDoc{{Flag: "--status", Desc: "first line\nsecond line"}}
	RenderHelp(&b, spec)

	var found bool
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.HasSuffix(line, "second line") {
			found = true
			if got := len(line) - len("second line"); got != descColumn {
				t.Errorf("continuation indent = %d, want %d", got, descColumn)
			}
		}
	}
	if !found {
		t.Error("continuation line not rendered")
	}
}
