package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/version"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/cli"
	"github.com/enola-labs/enola/pkg/dashboard"
)

// ossRunner and wrapperRunner are the two binaries these commands serve: the OSS one,
// which dispatches `upgrade` itself and is stamped through internal/version, and a
// wrapper, which does neither.
func ossRunner() *Runner {
	return New(cli.Binary{Name: "enola", Version: version.Version}, "upgrade")
}

func wrapperRunner() *Runner {
	return New(cli.Binary{Name: "acme-arch", Version: "1.4.2"}, "activate")
}

// A wrapper is stamped through its own -X target, so anything reading enola's
// internal/version inside one reports "dev" forever. That is what made `doctor` print
// "enola dev", the SARIF driver claim to be enola dev, and the standalone dashboard
// register its instance as dev — three surfaces, one cause.
func TestBuildVersion_IsTheBinarysOwnNotEnolas(t *testing.T) {
	if got := wrapperRunner().buildVersion(); got != "1.4.2" {
		t.Errorf("buildVersion() = %q, want the wrapper's own version", got)
	}
	// Unset stays enola's, which is what keeps cli.Binary.Version optional.
	if got := New(cli.Binary{Name: "enola"}).buildVersion(); got != version.Version {
		t.Errorf("buildVersion() = %q with no Version set, want enola's %q", got, version.Version)
	}
}

// The update notice reads ENOLA's release manifest. In a binary that ships on another
// release schedule it would not merely name a command that does not exist — it would
// compare two unrelated version streams and advertise the difference as an upgrade.
//
// It is suppressed there today only because internal/version is never stamped in a
// wrapper, so updatecheck.Suppressed sees "dev". That is an accident, and this test is
// what stops it from being the only thing holding the line.
func TestUpdateNotice_IsSilentInABinaryTheManifestDoesNotDescribe(t *testing.T) {
	var buf bytes.Buffer
	wrapperRunner().updateNotice(&buf)
	if buf.Len() != 0 {
		t.Errorf("a wrapper printed an enola update notice:\n%s", buf.String())
	}
	if wrapperRunner().selfUpgrades() {
		t.Error("a binary that does not dispatch `upgrade` must not claim to self-upgrade")
	}
	if !ossRunner().selfUpgrades() {
		t.Error("cmd/enola passes `upgrade` to New; the Runner must recognise it")
	}
}

// `dashboard` starts a dashboard of its OWN, so a wrapper that passed its options only
// to bootstrap's MCP server got the plain OSS page here. InsightLabels is the part that
// matters: it is the page's admission list, so the wrapper's own explainers' findings
// were computed and then filtered out of its own Insights modal.
func TestDashboardOptions_ComeFromTheBinaryWhenItRegistersThem(t *testing.T) {
	if got := ossRunner().dashboardOptions(nil); got.Title != "" || got.InsightLabels != nil {
		t.Errorf("a binary that registers nothing must get the OSS defaults, got %+v", got)
	}

	r := wrapperRunner().WithDashboard(func(*bootstrap.Engine) dashboard.Options {
		return dashboard.Options{
			Title:         "enola enterprise",
			InsightLabels: map[string]string{"dead-code": "Dead code"},
		}
	})
	got := r.dashboardOptions(nil)
	if got.Title != "enola enterprise" {
		t.Errorf("Title = %q, want the wrapper's", got.Title)
	}
	if got.InsightLabels["dead-code"] == "" {
		t.Error("the wrapper's explainers are not in the page's admission list, so its own findings would be filtered out")
	}
}

// install.Options has carried ExtraInstructions/ExtraHooksNote since it was written, and
// nothing reached them: `install` built its Options here and left both empty, so a
// licensed binary wrote instruction files naming only the OSS tools.
func TestInstructionSeam_ReachesTheInstallOptions(t *testing.T) {
	r := wrapperRunner().WithInstructions("- `find_orphans` — unreferenced symbols.", "It also grades dead code.")
	if !strings.Contains(r.extraInstructions, "find_orphans") {
		t.Errorf("WithInstructions did not record the body: %q", r.extraInstructions)
	}
	if !strings.Contains(r.extraHooksNote, "dead code") {
		t.Errorf("WithInstructions did not record the hooks note: %q", r.extraHooksNote)
	}
	if got := ossRunner(); got.extraInstructions != "" || got.extraHooksNote != "" {
		t.Error("the OSS binary must add nothing, so its instruction files stay byte-identical")
	}
}

// The link the seam actually needed: runInstall must copy the Runner's text into the
// install.Options it builds. install.Options has carried those fields since it was
// written and this is the line that had been missing, so a unit test on
// WithInstructions alone would have passed while `install` still wrote OSS-only
// instructions to disk.
func TestInstall_WritesTheBinarysOwnInstructions(t *testing.T) {
	repo := t.TempDir()
	agents := filepath.Join(repo, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# Agents\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const marker = "- `find_orphans` — symbols nothing references."
	r := wrapperRunner().WithInstructions(marker, "")

	// --yes: the confirmation prompt reads a terminal this test does not have.
	// Output is discarded; what is being asserted is what landed on disk.
	stdout := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	r.Install([]string{"--yes", repo}, false)
	os.Stdout = stdout
	_ = devnull.Close()

	got, err := os.ReadFile(agents)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), marker) {
		t.Errorf("AGENTS.md does not name this binary's tools:\n%s", got)
	}
	// The shared body must still be there — the wrapper adds, it does not replace.
	if !strings.Contains(string(got), "impact_analysis") {
		t.Errorf("the shared instruction body was lost:\n%s", got)
	}
}
