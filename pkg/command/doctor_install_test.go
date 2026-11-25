package command

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/install"
)

// doctor reports every file a plain re-install would change, not only skills: here a
// hook command pointing at another binary, which is what an upgrade or a move leaves.
func TestDoctorReportsInstallDrift(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	o := install.Options{Scope: install.ScopeLocal, RepoDir: repo, HomeDir: home, Hooks: true,
		Targets: []string{"claude"}, HookCommand: hookBinary()}
	if _, err := install.Install(o); err != nil {
		t.Fatal(err)
	}
	show := func() string {
		local := installStatusFor(install.ScopeLocal, repo, home)
		global := installStatusFor(install.ScopeGlobal, repo, home)
		return captureStdout(t, func() { testRunner().printInstall(repo, home, local, global) })
	}

	if out := show(); !strings.Contains(out, "current with this binary") || strings.Contains(out, "~ (all projects)") {
		t.Errorf("a fresh install is not reported current, or an empty home is listed:\n%s", out)
	}

	o.HookCommand = "/elsewhere/enola"
	if _, err := install.Install(o); err != nil {
		t.Fatal(err)
	}
	out := show()
	if !strings.Contains(out, "updated "+filepath.Join(".claude", "settings.json")) {
		t.Errorf("hooks invoking another binary are not reported:\n%s", out)
	}
	if !strings.Contains(out, "Run `enola install` to bring them in line") {
		t.Errorf("no command to fix it:\n%s", out)
	}
}

// Nothing installed is said as such, with the command that installs it.
func TestDoctorSaysWhenNothingIsInstalled(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	local := installStatusFor(install.ScopeLocal, repo, home)
	out := captureStdout(t, func() { testRunner().printInstall(repo, home, local, installStatus{}) })
	if !strings.Contains(out, "nothing installed. `enola install --hooks`") {
		t.Errorf("an empty repository is not reported as having nothing installed:\n%s", out)
	}
}
