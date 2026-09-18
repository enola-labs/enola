package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/version"
	"github.com/enola-labs/enola/pkg/cli"
)

// Runner does not hardcode enola's own identity: New takes a cli.Binary, and the name,
// version and self-dispatched subcommands all come from it. ossRunner is what cmd/enola
// passes; otherBinary is one configured differently, which is what the negative branches
// of buildVersion and selfUpgrades exist for.
func ossRunner() *Runner {
	return New(cli.Binary{Name: "enola", Version: version.Version}, "upgrade")
}

func otherBinary() *Runner {
	return New(cli.Binary{Name: "acme-arch", Version: "1.4.2"}, "activate")
}

// A binary stamped through its own -X target reports that version, not enola's. Reading
// internal/version unconditionally is what once made `doctor` print "enola dev", the
// SARIF driver claim to be enola dev, and the standalone dashboard register its instance
// as dev: three surfaces, one cause.
func TestBuildVersion_IsTheBinarysOwnNotEnolas(t *testing.T) {
	if got := otherBinary().buildVersion(); got != "1.4.2" {
		t.Errorf("buildVersion() = %q, want the binary's own version", got)
	}
	// Unset stays enola's, which is what keeps cli.Binary.Version optional.
	if got := New(cli.Binary{Name: "enola"}).buildVersion(); got != version.Version {
		t.Errorf("buildVersion() = %q with no Version set, want enola's %q", got, version.Version)
	}
}

// The update notice reads ENOLA's release manifest, so it must only be printed by a
// binary that manifest describes. In one shipping on another release schedule it would
// not merely name a command that does not exist — it would compare two unrelated version
// streams and advertise the difference as an upgrade.
func TestUpdateNotice_IsSilentInABinaryTheManifestDoesNotDescribe(t *testing.T) {
	var buf bytes.Buffer
	otherBinary().updateNotice(&buf)
	if buf.Len() != 0 {
		t.Errorf("a binary that does not self-upgrade printed an enola update notice:\n%s", buf.String())
	}
	if otherBinary().selfUpgrades() {
		t.Error("a binary that does not dispatch `upgrade` must not claim to self-upgrade")
	}
	if !ossRunner().selfUpgrades() {
		t.Error("cmd/enola passes `upgrade` to New; the Runner must recognise it")
	}
}

// `install` must write the shared instruction body to disk, not merely compose it. The
// composition is unit-tested in pkg/install; what this covers is that runInstall's
// Options actually reach the file.
func TestInstall_WritesTheSharedInstructions(t *testing.T) {
	repo := t.TempDir()
	agents := filepath.Join(repo, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# Agents\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --yes: the confirmation prompt reads a terminal this test does not have.
	// Output is discarded; what is being asserted is what landed on disk.
	stdout := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	ossRunner().Install([]string{"--yes", repo}, false)
	os.Stdout = stdout
	_ = devnull.Close()

	got, err := os.ReadFile(agents)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "impact_analysis") {
		t.Errorf("the shared instruction body was not written:\n%s", got)
	}
}
