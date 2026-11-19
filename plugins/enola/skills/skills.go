// Package skills holds the agent skills enola ships, as the one copy two consumers read.
//
// The Claude Code plugin loads them from this directory as it is: skills/<name>/SKILL.md
// inside plugins/enola. `enola install` embeds the same files and writes them into the
// skills directories the other agents read. A second copy under pkg/install would be one
// more thing to drift, and go:embed cannot reach a directory above its own package, so the
// package lives here, beside the files.
//
// The plugin loader ignores this file: it reads skill directories, not loose files.
package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// files is every file inside a skill directory. The pattern needs a slash, so this file
// and its test, which sit beside the directories, are not part of any skill.
//
//go:embed */*
var files embed.FS

// File is one file of a skill, by its path inside the skill's directory.
type File struct {
	Path    string
	Content []byte
}

// Skill is one skill directory.
type Skill struct {
	// Dir is the directory name in the plugin, which is also the name the plugin serves
	// it under, namespaced: /enola:<Dir>.
	Dir   string
	Files []File
}

// pluginOnly are skills that make sense only inside the plugin. `setup` installs the
// binary the plugin needs; a skill the binary itself wrote has no binary to install.
var pluginOnly = map[string]bool{"setup": true}

// All returns every skill, sorted by directory, with its files sorted by path.
func All() ([]Skill, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s := Skill{Dir: e.Name()}
		err := fs.WalkDir(files, e.Name(), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := files.ReadFile(p)
			if err != nil {
				return err
			}
			s.Files = append(s.Files, File{Path: strings.TrimPrefix(p, e.Name()+"/"), Content: b})
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Slice(s.Files, func(i, j int) bool { return s.Files[i].Path < s.Files[j].Path })
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// Installable returns the skills `enola install` writes: All, less the plugin-only ones.
func Installable() ([]Skill, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, s := range all {
		if !pluginOnly[s.Dir] {
			out = append(out, s)
		}
	}
	return out, nil
}

// InstalledName is the name a skill gets outside the plugin. Skills in a skills
// directory share one flat namespace with the user's own and every other tool's, where
// "review" or "intent" would collide, so each carries enola's name. The plugin namespaces
// them itself (/enola:review), so the source keeps the short name.
func InstalledName(dir string) string {
	if dir == "enola" || strings.HasPrefix(dir, "enola-") {
		return dir
	}
	return "enola-" + dir
}

// Rename returns a SKILL.md whose frontmatter `name` is name. The Agent Skills format
// requires the name to match the directory, so a skill installed as enola-review must
// say so.
func Rename(skillMD []byte, name string) ([]byte, error) {
	fm, body, ok := splitFrontmatter(string(skillMD))
	if !ok {
		return nil, fmt.Errorf("no frontmatter")
	}
	lines := strings.Split(fm, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "name:") {
			lines[i] = "name: " + name
			return []byte("---\n" + strings.Join(lines, "\n") + "\n---" + body), nil
		}
	}
	return nil, fmt.Errorf("frontmatter has no name")
}

// splitFrontmatter splits "---\n<fm>\n---<body>" into fm and body.
func splitFrontmatter(s string) (fm, body string, ok bool) {
	rest, found := strings.CutPrefix(s, "---\n")
	if !found {
		return "", "", false
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", false
	}
	return rest[:end], rest[end+len("\n---"):], true
}
