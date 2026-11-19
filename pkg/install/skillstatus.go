package install

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"

	"github.com/enola-labs/enola/plugins/enola/skills"
)

// SkillState is how one skill directory compares with what this binary would install.
type SkillState string

const (
	// SkillCurrent is enola's skill, identical to what this binary writes.
	SkillCurrent SkillState = "current"
	// SkillOutdated is enola's skill, written by another version: re-running install
	// replaces it.
	SkillOutdated SkillState = "outdated"
	// SkillMissing is a skill this binary ships that the directory does not have.
	SkillMissing SkillState = "missing"
	// SkillRetired is a skill enola wrote that this binary no longer installs there:
	// re-running install removes it.
	SkillRetired SkillState = "retired"
	// SkillTheirs is a directory with one of enola's skill names holding a skill enola
	// did not write. Install leaves it alone.
	SkillTheirs SkillState = "theirs"
)

// SkillStatus is one skill in one directory.
type SkillStatus struct {
	Name  string
	State SkillState
}

// SkillsLocation is one skills directory enola installs into, and what it holds.
type SkillsLocation struct {
	// Dir is the skills directory, for example <repo>/.claude/skills.
	Dir string
	// Agents names who reads it.
	Agents string
	// Global is true for a directory under the home directory.
	Global bool
	// ServedByPlugin is true when Claude Code's skills come from enola's plugin here,
	// so the directory is expected to hold none of them.
	ServedByPlugin bool
	Skills         []SkillStatus
}

// Installed reports whether any of enola's skills, in any state but missing, is here.
func (l SkillsLocation) Installed() bool {
	for _, s := range l.Skills {
		if s.State != SkillMissing && s.State != SkillTheirs {
			return true
		}
	}
	return false
}

// SkillsReport compares the skills directories of repoDir and of homeDir with what this
// binary installs into each, the same rendering `enola install` writes, so a skill
// reported current is byte for byte what install would leave. Either directory may be
// empty to leave its two locations out.
func SkillsReport(repoDir, homeDir string) ([]SkillsLocation, error) {
	var out []SkillsLocation
	add := func(o Options, claude bool) error {
		base := o.RepoDir
		if o.Scope == ScopeGlobal {
			base = o.HomeDir
		}
		loc := SkillsLocation{Global: o.Scope == ScopeGlobal}
		var skip map[string]bool
		if claude {
			loc.Dir, loc.Agents, skip = filepath.Join(base, ".claude", "skills"), "Claude Code", coveredByClaudeRule
			loc.ServedByPlugin = pluginServesSkills(o)
		} else {
			loc.Dir, loc.Agents = filepath.Join(base, ".agents", "skills"), "Codex, Cursor, Copilot, Pi, opencode"
		}
		statuses, err := skillStatuses(loc.Dir, skip, loc.ServedByPlugin)
		if err != nil {
			return err
		}
		loc.Skills = statuses
		out = append(out, loc)
		return nil
	}
	if repoDir != "" {
		o := Options{Scope: ScopeLocal, RepoDir: repoDir, HomeDir: homeDir}
		if err := add(o, true); err != nil {
			return nil, err
		}
		if err := add(o, false); err != nil {
			return nil, err
		}
	}
	if homeDir != "" {
		o := Options{Scope: ScopeGlobal, HomeDir: homeDir}
		if err := add(o, true); err != nil {
			return nil, err
		}
		if err := add(o, false); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// skillStatuses classifies root's skills. With none expected (the plugin serves them),
// every one enola wrote there is retired, which is what install would do with it.
func skillStatuses(root string, skip map[string]bool, noneExpected bool) ([]SkillStatus, error) {
	all, err := skills.Installable()
	if err != nil {
		return nil, err
	}
	var out []SkillStatus
	expected := map[string]bool{}
	for _, s := range all {
		if skip[s.Dir] || noneExpected {
			continue
		}
		name := skills.InstalledName(s.Dir)
		expected[name] = true
		dir := filepath.Join(root, name)
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); os.IsNotExist(err) {
			out = append(out, SkillStatus{name, SkillMissing})
			continue
		}
		if claimable(dir) != nil {
			out = append(out, SkillStatus{name, SkillTheirs})
			continue
		}
		files, err := renderSkill(s)
		if err != nil {
			return nil, err
		}
		state := SkillCurrent
		for _, f := range files {
			got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
			if err != nil || !bytes.Equal(got, f.Content) {
				state = SkillOutdated
				break
			}
		}
		out = append(out, SkillStatus{name, state})
	}
	owned, err := ownedSkillDirs(root)
	if err != nil {
		return nil, err
	}
	for _, name := range owned {
		if !expected[name] {
			out = append(out, SkillStatus{name, SkillRetired})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
