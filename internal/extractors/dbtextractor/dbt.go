// Package dbtextractor reads dbt projects: the models a project builds, the
// models and sources each one selects from, and the sources it declares.
//
// A dbt model is a SELECT in a .sql file under one of the project's model paths;
// dbt names it by the file's stem and builds the DAG from the Jinja calls in it:
//
//	select * from {{ ref("stg_orders") }}                  -- another model
//	select * from {{ ref("shared", "dim_dates") }}          -- a model of a package
//	select * from {{ source("cdc_changelog", "orders") }}   -- a declared source table
//
// Each model and snapshot becomes a symbol named "<project>.<model>", with a
// depends_on edge to every model it refs and every source it selects from. Each
// table a sources: block declares becomes a storage fact named "<source>.<table>",
// the pair source() is called with. A model's description from its properties
// file is its doc.
//
// Every model is a root: dbt builds all of them, and a mart nothing refs is read
// by a dashboard or an export, not dead.
package dbtextractor

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/facts"
)

// Extractor reads dbt projects.
type Extractor struct{}

// New creates the extractor.
func New() *Extractor { return &Extractor{} }

// Name returns the extractor name.
func (e *Extractor) Name() string { return "dbt" }

const projectFile = "dbt_project.yml"

// OwnsFile scopes caching to what a dbt project is made of: its project file, its
// SQL and its YAML property files.
func (e *Extractor) OwnsFile(relFile string) bool {
	if path.Base(relFile) == projectFile {
		return true
	}
	switch strings.ToLower(path.Ext(relFile)) {
	case ".sql", ".yml", ".yaml", ".csv":
		return !installedCopy(relFile)
	}
	return false
}

// Detect reports a dbt project anywhere in the repository.
func (e *Extractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector.
func (e *Extractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if path.Base(rel) == projectFile && !installedCopy(rel) {
			return true, nil
		}
	}
	return false, nil
}

// installedCopy reports a file dbt writes or installs rather than one the project
// holds: installed packages and compiled output.
func installedCopy(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case "dbt_packages", "dbt_modules", "target", "logs":
			return true
		}
	}
	return false
}

type project struct {
	dir   string // repository-relative, "." at the root
	name  string
	paths []string // model and snapshot paths, relative to dir
	seeds []string // seed paths, relative to dir
}

type projectFileDoc struct {
	Name          string   `yaml:"name"`
	ModelPaths    []string `yaml:"model-paths"`
	SourcePaths   []string `yaml:"source-paths"` // the pre-1.0 spelling
	SnapshotPaths []string `yaml:"snapshot-paths"`
	SeedPaths     []string `yaml:"seed-paths"`
	DataPaths     []string `yaml:"data-paths"` // the pre-1.0 spelling
}

type propertiesDoc struct {
	Models []struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	} `yaml:"models"`
	Snapshots []struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	} `yaml:"snapshots"`
	Sources []struct {
		Name     string `yaml:"name"`
		Database string `yaml:"database"`
		Schema   string `yaml:"schema"`
		Tables   []struct {
			Name        string `yaml:"name"`
			Identifier  string `yaml:"identifier"`
			Description string `yaml:"description"`
		} `yaml:"tables"`
	} `yaml:"sources"`
}

// refCall matches ref('model') and ref('package', 'model'); sourceCall matches
// source('source', 'table'). Only literal arguments name a node; a ref built from
// a variable is left out.
var (
	refCall    = regexp.MustCompile(`\bref\(\s*['"]([^'"]+)['"]\s*(?:,\s*['"]([^'"]+)['"]\s*)?(?:,\s*v(?:ersion)?\s*=\s*[^)]*)?\)`)
	sourceCall = regexp.MustCompile(`\bsource\(\s*['"]([^'"]+)['"]\s*,\s*['"]([^'"]+)['"]\s*\)`)
)

type model struct {
	project *project
	name    string
	file    string
	refs    [][2]string // {package or "", model}
	sources []string    // "source.table"
	seed    bool
}

// Extract emits the projects' models, snapshots and sources.
func (e *Extractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var projects []*project
	for _, rel := range files {
		if path.Base(rel) != projectFile || installedCopy(rel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		var doc projectFileDoc
		if yaml.Unmarshal(data, &doc) != nil || doc.Name == "" {
			continue
		}
		p := &project{dir: path.Dir(rel), name: doc.Name}
		p.paths = append(append(append(p.paths, doc.ModelPaths...), doc.SourcePaths...), doc.SnapshotPaths...)
		if len(doc.ModelPaths) == 0 && len(doc.SourcePaths) == 0 {
			p.paths = append(p.paths, "models")
		}
		if len(doc.SnapshotPaths) == 0 {
			p.paths = append(p.paths, "snapshots")
		}
		p.seeds = append(append(p.seeds, doc.SeedPaths...), doc.DataPaths...)
		if len(p.seeds) == 0 {
			p.seeds = []string{"seeds"}
		}
		projects = append(projects, p)
	}
	if len(projects) == 0 {
		return nil, nil
	}
	// Deepest first, so a file goes to the nearest enclosing project.
	sort.Slice(projects, func(i, j int) bool { return len(projects[i].dir) > len(projects[j].dir) })

	var models []*model
	descriptions := map[string]string{} // "<project>.<model>" -> description
	var out []facts.Fact
	seenSource := map[string]bool{}
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if installedCopy(rel) {
			continue
		}
		p, inPath := owningProject(projects, rel)
		if p == nil {
			continue
		}
		ext := strings.ToLower(path.Ext(rel))
		switch {
		case ext == ".csv" && underAny(p, p.seeds, rel):
			// A seed is a table dbt loads from the CSV; ref() names it like a model.
			models = append(models, &model{project: p, name: strings.TrimSuffix(path.Base(rel), path.Ext(rel)), file: rel, seed: true})
		case ext == ".sql" && inPath:
			src, err := os.ReadFile(filepath.Join(repoPath, rel))
			if err != nil {
				continue
			}
			m := &model{project: p, name: strings.TrimSuffix(path.Base(rel), path.Ext(rel)), file: rel}
			for _, r := range refCall.FindAllSubmatch(src, -1) {
				if len(r[2]) > 0 {
					m.refs = append(m.refs, [2]string{string(r[1]), string(r[2])})
				} else {
					m.refs = append(m.refs, [2]string{"", string(r[1])})
				}
			}
			for _, s := range sourceCall.FindAllSubmatch(src, -1) {
				m.sources = append(m.sources, string(s[1])+"."+string(s[2]))
			}
			models = append(models, m)
		case (ext == ".yml" || ext == ".yaml") && inPath:
			data, err := os.ReadFile(filepath.Join(repoPath, rel))
			if err != nil {
				continue
			}
			var doc propertiesDoc
			if yaml.Unmarshal(data, &doc) != nil {
				continue
			}
			for _, m := range doc.Models {
				if m.Name != "" && m.Description != "" {
					descriptions[p.name+"."+m.Name] = m.Description
				}
			}
			for _, m := range doc.Snapshots {
				if m.Name != "" && m.Description != "" {
					descriptions[p.name+"."+m.Name] = m.Description
				}
			}
			for _, src := range doc.Sources {
				for _, t := range src.Tables {
					if src.Name == "" || t.Name == "" {
						continue
					}
					name := src.Name + "." + t.Name
					if seenSource[name] {
						continue
					}
					seenSource[name] = true
					props := map[string]any{"storage_kind": "table", "engine": "dbt_source", "language": "dbt", "dbt_project": p.name}
					if src.Database != "" {
						props["database"] = src.Database
					}
					if src.Schema != "" {
						props["schema"] = src.Schema
					}
					if t.Identifier != "" {
						props["identifier"] = t.Identifier
					}
					if t.Description != "" {
						facts.SetDocProp(props, t.Description)
					}
					out = append(out, facts.Fact{Kind: facts.KindStorage, Name: name, File: rel, Line: 1, Props: props})
				}
			}
		}
	}

	// A ref names a model of the same project first, then the one other project in
	// the repository that declares it (an installed shared package checked in
	// beside it).
	byName := map[string][]*model{}
	for _, m := range models {
		byName[m.name] = append(byName[m.name], m)
	}
	resolve := func(from *model, pkg, name string) string {
		var other []*model
		for _, c := range byName[name] {
			switch {
			case pkg != "" && c.project.name == pkg:
				return c.project.name + "." + name
			case pkg == "" && c.project == from.project:
				return c.project.name + "." + name
			default:
				other = append(other, c)
			}
		}
		if pkg == "" && len(other) == 1 {
			return other[0].project.name + "." + name
		}
		if pkg != "" {
			return pkg + "." + name
		}
		return name
	}

	sort.Slice(models, func(i, j int) bool { return models[i].file < models[j].file })
	for _, m := range models {
		name := m.project.name + "." + m.name
		// No exported prop: dbt has no visibility, and every model is reachable by
		// name from any other.
		props := map[string]any{
			"symbol_kind":           facts.SymbolType,
			"language":              "dbt",
			"dbt_project":           m.project.name,
			facts.PropFrameworkRoot: "dbt",
		}
		if m.seed {
			props["dbt_resource"] = "seed"
		}
		if d := descriptions[name]; d != "" {
			facts.SetDocProp(props, d)
		}
		f := facts.Fact{
			Kind: facts.KindSymbol, Name: name, File: m.file, Line: 1, Props: props,
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: path.Dir(m.file)}},
		}
		seen := map[string]bool{}
		for _, r := range m.refs {
			if t := resolve(m, r[0], r[1]); t != name && !seen[t] {
				seen[t] = true
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: t})
			}
		}
		for _, s := range m.sources {
			if !seen[s] {
				seen[s] = true
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: s})
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// underAny reports rel inside one of a project's directories.
func underAny(p *project, dirs []string, rel string) bool {
	within := rel
	if p.dir != "." {
		within = strings.TrimPrefix(rel, p.dir+"/")
	}
	for _, d := range dirs {
		d = strings.Trim(path.Clean(d), "/")
		if d != "" && d != "." && strings.HasPrefix(within, d+"/") {
			return true
		}
	}
	return false
}

// owningProject returns the nearest project enclosing rel, and whether rel lies in
// one of that project's model or snapshot paths.
func owningProject(projects []*project, rel string) (*project, bool) {
	for _, p := range projects {
		within := rel
		if p.dir != "." {
			if !strings.HasPrefix(rel, p.dir+"/") {
				continue
			}
			within = strings.TrimPrefix(rel, p.dir+"/")
		}
		for _, mp := range p.paths {
			mp = strings.Trim(path.Clean(mp), "/")
			if mp != "" && mp != "." && strings.HasPrefix(within, mp+"/") {
				return p, true
			}
		}
		return p, false
	}
	return nil, false
}
