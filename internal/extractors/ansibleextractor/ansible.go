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
)

// Ansible structure is by-name and literal: a playbook lists the roles it
// applies, a role's tasks import other roles by name, and a role's templates
// live under its own tree. All of it reads deterministically from YAML — and
// none of it requires rendering a Jinja template, which this extractor never
// does.
//
// Detection is the whole false-positive risk (arbitrary YAML must not read as
// Ansible), so it demands an unambiguous marker: an ansible.cfg, or a roles/
// directory beside playbooks whose plays declare `hosts`.

// Extractor extracts Ansible facts.
type Extractor struct{}

// New creates the extractor.
func New() *Extractor { return &Extractor{} }

// Name returns the extractor name.
func (e *Extractor) Name() string { return "ansible" }

// Detect requires an ansible.cfg or a roles/ directory, within three
// directory levels — infrastructure repos commonly nest their Ansible under
// <area>/ansible/.
func (e *Extractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector: an ansible.cfg or a roles/
// directory at any depth, where the old walk stopped at three. Both signals now
// read off the names — a roles/ segment anywhere in a path is the directory the
// walk used to have to reach, and ansible.cfg is a name rather than a stat.
func (e *Extractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if detectnames.HasSegment(rel, "roles") {
			return true, nil
		}
		if detectnames.Base(rel) == "ansible.cfg" {
			return true, nil
		}
	}
	return false, nil
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
		slashed := filepath.ToSlash(relFile)
		if idx := strings.Index(slashed, "roles/"); idx >= 0 && (idx == 0 || slashed[idx-1] == '/') {
			rest := slashed[idx+len("roles/"):]
			if slash := strings.IndexByte(rest, '/'); slash > 0 {
				name := rest[:slash]
				roleDirs[name] = slashed[:idx] + "roles/" + name
			}
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
			if path != root && (strings.HasPrefix(name, ".") || ansibleSkipDirs[name]) {
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
