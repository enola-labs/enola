package install

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/plugins/enola/skills"
)

// skillMarker ends every SKILL.md enola writes. It is what makes a skill directory
// enola's to update or remove: a directory named enola-something that lacks it is the
// user's, however it is named. A skill dropped from a later release is found by the same
// marker, so an install never leaves a stale one behind.
const skillMarker = "<!-- Written by `enola install`. `enola uninstall` removes this skill. -->\n"

// agentSkillsReaders are the targets whose agent reads the shared .agents/skills
// directory. Claude Code reads only .claude/skills, which its own target writes.
var agentSkillsReaders = map[string]bool{
	"cursor": true, "agents": true, "codex": true, "copilot": true, "pi": true, "opencode": true,
}

// agentSkillsHomeDirs are the home-directory roots whose presence says one of those
// agents is installed.
var agentSkillsHomeDirs = []string{".codex", ".pi", ".cursor", ".copilot", filepath.Join(".config", "opencode")}

// claudeSkills writes the skills into Claude Code's skills directory, unless enola's
// Claude Code plugin already serves them there, as /enola:<name>. A second copy would
// list every skill twice; any copy an earlier install wrote is removed instead.
func claudeSkills(o Options, remove bool) ([]Result, error) {
	root := filepath.Join(claudeDir(o), "skills")
	if !remove && pluginServesSkills(o) {
		out, err := syncSkills(root, pruneStop(o), o, true)
		if err != nil {
			return nil, err
		}
		return append(out, Result{
			Path:   root,
			Action: ActionSkipped,
			Reason: "the enola Claude Code plugin is installed and serves these skills as /enola:<name>",
		}), nil
	}
	return syncSkills(root, pruneStop(o), o, remove)
}

// pluginServesSkills reports whether Claude Code has enola's plugin installed where this
// install applies: for this user, or for this repository. It reads Claude Code's own
// record of installed plugins, which is not a documented format, so anything it cannot
// read counts as no plugin, and the cost of being wrong is a skill listed twice.
func pluginServesSkills(o Options) bool {
	raw, err := os.ReadFile(filepath.Join(o.HomeDir, ".claude", "plugins", "installed_plugins.json"))
	if err != nil {
		return false
	}
	var doc struct {
		Plugins map[string][]struct {
			Scope       string `json:"scope"`
			ProjectPath string `json:"projectPath"`
		} `json:"plugins"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	for key, installs := range doc.Plugins {
		if !strings.HasPrefix(key, "enola@") {
			continue
		}
		for _, in := range installs {
			if in.Scope == "user" {
				return true
			}
			if o.Scope == ScopeLocal && in.ProjectPath != "" && samePath(in.ProjectPath, o.RepoDir) {
				return true
			}
		}
	}
	return false
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// agentSkills writes the skills into .agents/skills, the directory Codex, Cursor,
// Copilot, Pi and opencode all read. Called once per run, however many of those targets
// were selected, because they share it.
//
// Globally, ~/.agents is created only when one of those agents is installed, the rule
// globalAgentsTarget follows: no directory in someone's home for a tool they do not use.
// It is not itself that evidence, because enola may have created it, and uninstall
// removes it again once it is empty.
func agentSkills(o Options, remove bool) ([]Result, error) {
	base := o.RepoDir
	if o.Scope == ScopeGlobal {
		base = o.HomeDir
	}
	root := filepath.Join(base, ".agents", "skills")
	if o.Scope == ScopeGlobal && !remove && !anyExists(o.HomeDir, agentSkillsHomeDirs) {
		return []Result{{
			Path:   root,
			Action: ActionSkipped,
			Reason: "none of Codex, Cursor, Copilot, Pi or opencode appears to be installed in your home directory",
		}}, nil
	}
	return syncSkills(root, pruneStop(o), o, remove)
}

func anyExists(home string, rels []string) bool {
	for _, r := range rels {
		if _, err := os.Stat(filepath.Join(home, r)); err == nil {
			return true
		}
	}
	return false
}

// syncSkills makes root hold exactly the current installable skills among the ones enola
// wrote: each written or left unchanged, and every other marked directory removed. With
// remove, every marked directory goes.
func syncSkills(root, stop string, o Options, remove bool) ([]Result, error) {
	var out []Result
	keep := map[string]bool{}
	if !remove {
		all, err := skills.Installable()
		if err != nil {
			return nil, err
		}
		for _, s := range all {
			name := skills.InstalledName(s.Dir)
			dir := filepath.Join(root, name)
			if err := claimable(dir); err != nil {
				out = append(out, Result{Path: dir, Action: ActionSkipped, Reason: err.Error()})
				continue
			}
			keep[name] = true
			for _, f := range s.Files {
				content := f.Content
				if f.Path == "SKILL.md" {
					renamed, err := skills.Rename(content, name)
					if err != nil {
						return nil, fmt.Errorf("skill %s: %w", s.Dir, err)
					}
					content = append(renamed, []byte("\n"+skillMarker)...)
				}
				r, err := writeOwnedFile(filepath.Join(dir, filepath.FromSlash(f.Path)), string(content), o.DryRun)
				if err != nil {
					return nil, err
				}
				out = append(out, r)
			}
		}
	}

	owned, err := ownedSkillDirs(root)
	if err != nil {
		return nil, err
	}
	for _, name := range owned {
		if keep[name] {
			continue
		}
		rs, err := removeSkillDir(filepath.Join(root, name), stop, o.DryRun)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// claimable refuses a directory enola would write into but did not create: one holding
// a SKILL.md without the marker. Overwriting it would replace the user's skill.
func claimable(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Contains(b, []byte(skillMarker)) {
		return fmt.Errorf("holds a skill enola did not write; rename or remove it to install enola's")
	}
	return nil
}

// ownedSkillDirs are the directories under root whose SKILL.md carries the marker.
func ownedSkillDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		// A name InstalledName leaves alone is one enola's skills can have.
		if !e.IsDir() || skills.InstalledName(e.Name()) != e.Name() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "SKILL.md"))
		if err == nil && bytes.Contains(b, []byte(skillMarker)) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// removeSkillDir removes every file of a skill enola wrote, and the directories left
// empty, up to stop.
func removeSkillDir(dir, stop string, dryRun bool) ([]Result, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, p := range paths {
		r, err := removeOwnedFile(p, stop, dryRun)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
