package skills

import (
	"strings"
	"testing"
)

// Every skill is one both consumers can load: frontmatter whose name is its directory,
// as the Agent Skills format and the plugin loader require, and a description, which is
// all an agent sees of a skill until it decides to load it.
func TestSkillsAreWellFormed(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no skills embedded")
	}
	for _, s := range all {
		var md []byte
		for _, f := range s.Files {
			if f.Path == "SKILL.md" {
				md = f.Content
			}
		}
		if md == nil {
			t.Errorf("%s: no SKILL.md", s.Dir)
			continue
		}
		if name, _ := frontmatter(md, "name"); name != s.Dir {
			t.Errorf("%s: frontmatter name %q, want the directory name", s.Dir, name)
		}
		if d, _ := frontmatter(md, "description"); len(d) < 40 {
			t.Errorf("%s: description %q is too short to tell an agent when to load it", s.Dir, d)
		}
		renamed, err := Rename(md, InstalledName(s.Dir))
		if err != nil {
			t.Errorf("%s: %v", s.Dir, err)
			continue
		}
		if name, _ := frontmatter(renamed, "name"); name != InstalledName(s.Dir) {
			t.Errorf("%s: renamed to %q", s.Dir, name)
		}
		_, body, _ := splitFrontmatter(string(md))
		if !strings.HasSuffix(string(renamed), body) {
			t.Errorf("%s: rename changed the body", s.Dir)
		}
	}
}

func TestInstallableLeavesOutSetup(t *testing.T) {
	inst, err := Installable()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range inst {
		if s.Dir == "setup" {
			t.Error("setup installs the binary; a binary-written copy has nothing to install")
		}
	}
}

func TestInstalledName(t *testing.T) {
	for in, want := range map[string]string{"enola": "enola", "review": "enola-review", "enola-x": "enola-x"} {
		if got := InstalledName(in); got != want {
			t.Errorf("InstalledName(%q) = %q, want %q", in, got, want)
		}
	}
}

// frontmatter returns the value of key in a SKILL.md's YAML frontmatter. The skills
// enola ships keep it to single-line `key: value` pairs, so that is all it reads.
func frontmatter(skillMD []byte, key string) (string, bool) {
	fm, _, ok := splitFrontmatter(string(skillMD))
	if !ok {
		return "", false
	}
	for _, line := range strings.Split(fm, "\n") {
		k, v, found := strings.Cut(line, ":")
		if found && strings.TrimSpace(k) == key && !strings.HasPrefix(line, " ") {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}
