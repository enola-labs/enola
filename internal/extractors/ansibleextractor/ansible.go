// Package ansibleextractor extracts architectural facts from Ansible content.
package ansibleextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/workspace"
)

// Ansible structure is by-name and literal: a playbook lists the roles it
// applies, a role's tasks import other roles by name, and a role's templates
// live under its own tree. All of it reads deterministically from YAML — and
// none of it requires rendering a Jinja template, which this extractor never
// does.
//
// Detection is the whole false-positive risk (arbitrary YAML must not read as
// Ansible), so it demands an unambiguous marker: an ansible.cfg, or a role, which is
// a roles/<name>/ directory holding one of the directories only an Ansible role has
// (tasks, handlers, meta, defaults, vars). A directory merely named roles is not
// one: Rails views, RBAC manifests and user-permission modules all use the name.

// Extractor extracts Ansible facts.
type Extractor struct{}

// New creates the extractor.
func New() *Extractor { return &Extractor{} }

// Name returns the extractor name.
func (e *Extractor) Name() string { return "ansible" }

// Detect walks the tree and answers from its names.
func (e *Extractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector: an ansible.cfg, or a file inside
// a role's own directory structure, at any depth.
func (e *Extractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if _, ok := roleDirOf(rel); ok {
			return true, nil
		}
		if detectnames.Base(rel) == "ansible.cfg" {
			return true, nil
		}
	}
	return false, nil
}

// roleSubdirs are the directories only an Ansible role has. templates/ and files/
// are role directories too, but plenty of trees that are not Ansible have them.
var roleSubdirs = map[string]bool{
	"tasks": true, "handlers": true, "meta": true, "defaults": true, "vars": true,
}

// roleDirOf returns the role directory (".../roles/<name>") a path lies in, when the
// path is inside one of that role's own subdirectories.
func roleDirOf(rel string) (string, bool) {
	segs := strings.Split(filepath.ToSlash(rel), "/")
	for i := 0; i+3 < len(segs); i++ {
		if segs[i] == "roles" && segs[i+1] != "" && roleSubdirs[segs[i+2]] {
			return strings.Join(segs[:i+2], "/"), true
		}
	}
	return "", false
}

type play struct {
	Name        string `yaml:"name"`
	Hosts       any    `yaml:"hosts"`
	Roles       []any  `yaml:"roles"`
	ImportPlays string `yaml:"import_playbook"`
}

type task struct {
	IncludeRole map[string]any `yaml:"include_role"`
	ImportRole  map[string]any `yaml:"import_role"`
}

// Extract parses playbooks and role trees. It walks the repository itself
// rather than consuming the engine's file list: YAML is ignore-globbed by
// default (the same reason the OpenAPI extractor self-walks), so the excluded
// directories are skipped here explicitly.
func (e *Extractor) Extract(ctx context.Context, repoPath string, _ []string) ([]facts.Fact, error) {
	files := walkAnsibleFiles(repoPath)
	var out []facts.Fact
	roleDirs := map[string]string{}
	sort.Strings(files)

	for _, relFile := range files {
		if dir, ok := roleDirOf(relFile); ok {
			roleDirs[factpath.Base(dir)] = dir
		}
	}
	roleNames := make([]string, 0, len(roleDirs))
	for n := range roleDirs {
		roleNames = append(roleNames, n)
	}
	sort.Strings(roleNames)
	roleSymbol := func(name string) string { return roleDirs[name] + "." + name }
	templateCounts := map[string]int{}
	taskRefs := map[string]map[string]bool{}

	for _, relFile := range files {
		slashed := filepath.ToSlash(relFile)
		for _, name := range roleNames {
			dir := roleDirs[name] + "/"
			if !strings.HasPrefix(slashed, dir) {
				continue
			}
			if strings.HasSuffix(slashed, ".j2") {
				templateCounts[name]++
			}
			if strings.Contains(slashed, "/tasks/") && isYAMLFile(slashed) {
				for _, ref := range roleRefsInTasks(filepath.Join(repoPath, relFile)) {
					if _, known := roleDirs[ref]; known && ref != name {
						if taskRefs[name] == nil {
							taskRefs[name] = map[string]bool{}
						}
						taskRefs[name][ref] = true
					}
				}
			}
		}
	}

	for _, name := range roleNames {
		f := facts.Fact{
			Kind: facts.KindSymbol,
			Name: roleSymbol(name),
			File: roleDirs[name],
			Line: 1,
			Props: map[string]any{
				"language":     "ansible",
				"symbol_kind":  "type",
				"ansible_kind": "role",
				"exported":     true,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: roleDirs[name]}},
		}
		if n := templateCounts[name]; n > 0 {
			f.SetProp("template_count", n)
		}
		refs := make([]string, 0, len(taskRefs[name]))
		for r := range taskRefs[name] {
			refs = append(refs, r)
		}
		sort.Strings(refs)
		for _, r := range refs {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: roleSymbol(r)})
		}
		out = append(out, facts.Fact{
			Kind:  facts.KindModule,
			Name:  roleDirs[name],
			File:  roleDirs[name],
			Props: map[string]any{"language": "ansible"},
		})
		out = append(out, f)
	}

	for _, relFile := range files {
		slashed := filepath.ToSlash(relFile)
		if !isYAMLFile(slashed) || strings.Contains(slashed, "roles/") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			continue
		}
		var plays []play
		if yaml.Unmarshal(data, &plays) != nil {
			continue
		}
		dir := factpath.Dir(relFile)
		prefix := dir + "."
		if dir == "." {
			prefix = ""
		}
		for i, p := range plays {
			if p.Hosts == nil {
				continue
			}
			name := p.Name
			if name == "" {
				name = strings.TrimSuffix(filepath.Base(slashed), filepath.Ext(slashed))
			}
			f := facts.Fact{
				Kind: facts.KindSymbol,
				Name: prefix + name,
				File: relFile,
				Line: 1,
				Props: map[string]any{
					"language":     "ansible",
					"symbol_kind":  "type",
					"ansible_kind": "play",
					"exported":     true,
				},
			}
			if dir != "." {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDeclares, Target: dir})
			}
			for _, r := range p.Roles {
				roleName := ""
				switch v := r.(type) {
				case string:
					roleName = v
				case map[string]any:
					if s, ok := v["role"].(string); ok {
						roleName = s
					}
				}
				if roleName != "" {
					if _, known := roleDirs[roleName]; known {
						f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: roleSymbol(roleName)})
					}
				}
			}
			_ = i
			out = append(out, f)
		}
	}
	return out, nil
}

func isYAMLFile(path string) bool {
	return strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml")
}

// roleRefsInTasks reads include_role/import_role literal names from a tasks
// file.
func roleRefsInTasks(absPath string) []string {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}
	var tasks []task
	if yaml.Unmarshal(data, &tasks) != nil {
		return nil
	}
	var refs []string
	for _, t := range tasks {
		for _, m := range []map[string]any{t.IncludeRole, t.ImportRole} {
			if m == nil {
				continue
			}
			if s, ok := m["name"].(string); ok && s != "" {
				refs = append(refs, s)
			}
		}
	}
	return refs
}

var ansibleSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "testdata": true, "dist": true,
	"build": true, "tmp": true, "log": true, "coverage": true,
}

// walkAnsibleFiles enumerates YAML and .j2 files, skipping the directories the
// ignore globs would have — a self-walking extractor must not read what the
// engine's walk excludes.
func walkAnsibleFiles(repoPath string) []string {
	var files []string
	root := filepath.Clean(repoPath) //factpath:host
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || ansibleSkipDirs[name] || workspace.IsLinkedWorktree(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if isYAMLFile(path) || strings.HasSuffix(path, ".j2") {
			if rel, relErr := filepath.Rel(root, path); relErr == nil {
				files = append(files, factpath.Slash(rel))
			}
		}
		return nil
	})
	return files
}
