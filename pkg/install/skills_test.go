package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/plugins/enola/skills"
)

// installedSkills is every skill install writes, by installed name.
func installedSkills(t *testing.T) []string {
	t.Helper()
	all, err := skills.Installable()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range all {
		out = append(out, skills.InstalledName(s.Dir))
	}
	return out
}

// Both directories get every installable skill, renamed and marked; Claude Code reads
// only the first, every other agent the second. Claude Code's copy leaves out what its
// rule file already says.
func TestInstall_SkillsGoToBothDirectories(t *testing.T) {
	o := opts(t, false)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, ".claude", "skills", "enola")); err == nil {
		t.Error(".claude/skills/enola: the rule file says the same, so Claude Code should not get it as a skill")
	}
	for _, root := range []string{".claude/skills", ".agents/skills"} {
		for _, name := range installedSkills(t) {
			if root == ".claude/skills" && name == "enola" {
				continue
			}
			md := []byte(read(t, filepath.Join(o.RepoDir, root, name, "SKILL.md")))
			if !strings.Contains(string(md), "\nname: "+name+"\n") {
				t.Errorf("%s/%s: frontmatter does not name it %s", root, name, name)
			}
			if !strings.HasSuffix(string(md), skillMarker) {
				t.Errorf("%s/%s: no marker, so uninstall could not tell it is enola's", root, name)
			}
		}
		if _, err := os.Stat(filepath.Join(o.RepoDir, root, "enola-setup")); err == nil {
			t.Errorf("%s: setup is plugin-only", root)
		}
	}
}

// A run narrowed to Claude Code writes no shared directory, and one narrowed to an
// agent that reads the shared directory does not write Claude's.
func TestInstall_SkillsFollowTheTargets(t *testing.T) {
	o := opts(t, false)
	o.Targets = []string{"claude"}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, ".agents")); err == nil {
		t.Error("--targets=claude wrote .agents/skills")
	}

	o = opts(t, false)
	o.Targets = []string{"codex", "pi"}
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, ".claude", "skills")); err == nil {
		t.Error("--targets=codex,pi wrote .claude/skills")
	}
	seen := 0
	for _, r := range rs {
		if strings.HasSuffix(r.Path, filepath.Join("enola", "SKILL.md")) {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the shared directory was written %d times, want once however many readers were selected", seen)
	}
}

// A directory with enola's name and a skill enola did not write is the user's: install
// leaves it as it is and says so, and uninstall does not touch it.
func TestInstall_SkillLeavesTheUsersOwnAlone(t *testing.T) {
	o := opts(t, false)
	o.Targets = []string{"claude"}
	name := installedSkills(t)[1] // [0] is the routing skill, which Claude Code does not get
	mine := filepath.Join(o.RepoDir, ".claude", "skills", name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	const own = "---\nname: " + "x" + "\ndescription: mine\n---\nmine\n"
	if err := os.WriteFile(mine, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}

	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionFor(rs, filepath.Join(name)); a != ActionSkipped {
		t.Errorf("action for the user's %s: %q, want skipped", name, a)
	}
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if got := read(t, mine); got != own {
		t.Errorf("the user's skill was changed:\n%s", got)
	}
}

// A skill a later release dropped is removed on the next install, found by its marker.
func TestInstall_RemovesASkillNoLongerShipped(t *testing.T) {
	o := opts(t, false)
	o.Targets = []string{"claude"}
	stale := filepath.Join(o.RepoDir, ".claude", "skills", "enola-retired")
	if err := os.MkdirAll(filepath.Join(stale, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{"SKILL.md": "---\nname: enola-retired\n---\n\n" + skillMarker, "references/a.md": "a"} {
		if err := os.WriteFile(filepath.Join(stale, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a retired skill enola wrote survived the install: %v", err)
	}
}

// Globally, the shared directory is created only for someone who uses an agent that
// reads it.
func TestInstall_GlobalSharedSkillsNeedAReader(t *testing.T) {
	o := opts(t, false)
	o.Scope = ScopeGlobal
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionFor(rs, filepath.Join(".agents", "skills")); a != ActionSkipped {
		t.Errorf("~/.agents/skills: %q with no reader installed, want skipped", a)
	}
	if _, err := os.Stat(filepath.Join(o.HomeDir, ".agents")); err == nil {
		t.Error("created ~/.agents for nobody")
	}

	if err := os.MkdirAll(filepath.Join(o.HomeDir, ".config", "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	read(t, filepath.Join(o.HomeDir, ".agents", "skills", "enola", "SKILL.md"))
}

// With enola's Claude Code plugin installed, the plugin serves the skills; a copy in
// .claude/skills would list each twice. One an earlier install wrote is taken away.
func TestInstall_ClaudeSkillsDeferToThePlugin(t *testing.T) {
	o := opts(t, false)
	o.Targets = []string{"claude"}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	reg := filepath.Join(o.HomeDir, ".claude", "plugins", "installed_plugins.json")
	if err := os.MkdirAll(filepath.Dir(reg), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"version":2,"plugins":{"enola@enola":[{"scope":"project","projectPath":"` + filepath.ToSlash(o.RepoDir) + `"}]}}`
	if err := os.WriteFile(reg, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionFor(rs, filepath.Join(".claude", "skills")); a != ActionSkipped {
		t.Errorf(".claude/skills: %q with the plugin installed, want skipped", a)
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, ".claude", "skills", "enola-review")); !os.IsNotExist(err) {
		t.Errorf("the copy an earlier install wrote survived: %v", err)
	}
	read(t, filepath.Join(o.RepoDir, ".claude", "rules", "enola.md"))

	// Another repository's plugin install is not this one's.
	other := opts(t, false)
	other.HomeDir, other.Targets = o.HomeDir, o.Targets
	if _, err := Install(other); err != nil {
		t.Fatal(err)
	}
	read(t, filepath.Join(other.RepoDir, ".claude", "skills", "enola-review", "SKILL.md"))
}

// An install from before the routing skill was left out of Claude Code's directory wrote
// it there; the next install takes it away.
func TestInstall_RemovesTheRoutingSkillFromClaude(t *testing.T) {
	o := opts(t, false)
	o.Targets = []string{"claude"}
	old := filepath.Join(o.RepoDir, ".claude", "skills", "enola", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("---\nname: enola\n---\n\n"+skillMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the routing skill an earlier install wrote survived: %v", err)
	}
}
