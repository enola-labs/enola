package command

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/install"
)

// A skill of the user's under one of enola's names is reported once, with what install
// does about it, not also as a bare row of its own.
func TestDoctorNamesTheUsersSkillOnce(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if _, err := install.Install(install.Options{Scope: install.ScopeLocal, RepoDir: repo, HomeDir: home, Targets: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(repo, ".claude", "skills", "enola-review", "SKILL.md")
	if err := os.WriteFile(mine, []byte("---\nname: enola-review\ndescription: mine\n---\nmine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	testRunner().printSkills(repo)
	os.Stdout = prev
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if n := strings.Count(string(out), "enola-review"); n != 1 {
		t.Errorf("the user's enola-review is named %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(string(out), "install leaves it alone") {
		t.Errorf("no word on what install does with it:\n%s", out)
	}
}
