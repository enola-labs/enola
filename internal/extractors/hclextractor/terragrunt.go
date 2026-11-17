package hclextractor

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// Terragrunt wraps Terraform: a directory holding a terragrunt.hcl is a unit,
// deployed on its own, whose code is a module named by a terraform { source }
// block, often inherited from a configuration it includes, and which may read the
// outputs of other units:
//
//	include "module" { path = find_in_parent_folders("module.hcl") }
//	terraform { source = "../../modules/vpc" }
//	dependency "network" { config_path = "../network" }
//
// A unit's file usually declares no Terraform block of its own, so without this
// pass a repository of three thousand units was a hundred modules. Each unit is
// now a module, with a dependency on the configuration it includes, the local
// module it deploys and the units it reads; a remote module source is recorded on
// the unit as module_source.

const terragruntFile = "terragrunt.hcl"

var (
	tgInclude      = regexp.MustCompile(`(?ms)^\s*include(?:\s+"[^"]*")?\s*\{(.*?)^\s*\}`)
	tgParentLookup = regexp.MustCompile(`find_in_parent_folders\(\s*(?:"([^"]*)")?\s*\)`)
	tgPath         = regexp.MustCompile(`(?m)^\s*path\s*=\s*"([^"$]+)"`)
	tgTerraform    = regexp.MustCompile(`(?ms)^\s*terraform\s*\{(.*?)^\}`)
	tgSource       = regexp.MustCompile(`(?m)^\s*source\s*=\s*"([^"]+)"`)
	tgDependency   = regexp.MustCompile(`(?ms)^\s*dependency\s+"([^"]+)"\s*\{(.*?)^\}`)
	tgConfigPath   = regexp.MustCompile(`(?m)^\s*config_path\s*=\s*"([^"$]+)"`)
	tgDependencies = regexp.MustCompile(`(?ms)^\s*dependencies\s*\{.*?paths\s*=\s*\[([^\]]*)\]`)
	tgQuoted       = regexp.MustCompile(`"([^"$]+)"`)
)

// terragruntFacts reads every unit's terragrunt.hcl. hasModule names the
// directories the Terraform pass already made modules.
func terragruntFacts(repoPath string, files []string, hasModule map[string]bool) []facts.Fact {
	hcl := map[string]bool{}
	var units []string
	for _, rel := range files {
		if !isHCLFile(rel) {
			continue
		}
		hcl[rel] = true
		if path.Base(rel) == terragruntFile {
			units = append(units, rel)
		}
	}
	if len(units) == 0 {
		return nil
	}
	sort.Strings(units)
	read := map[string]string{}
	load := func(rel string) string {
		if s, ok := read[rel]; ok {
			return s
		}
		b, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(rel)))
		s := ""
		if err == nil {
			s = string(b)
		}
		read[rel] = s
		return s
	}

	var out []facts.Fact
	edge := func(from, to, file, kind string) {
		out = append(out, facts.Fact{
			Kind: facts.KindDependency, Name: from + " -> " + to, File: file, Line: 1,
			Props:     map[string]any{"language": "hcl", "source": "internal", "terragrunt": kind},
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: to}},
		})
	}
	for _, rel := range units {
		dir := factpath.Dir(rel)
		text := stripHCLComments(load(rel))
		props := map[string]any{"language": "hcl", "terragrunt_unit": true}
		seen := map[string]bool{}
		link := func(to, kind string) {
			if to == "" || to == dir || seen[to] {
				return
			}
			seen[to] = true
			edge(dir, to, rel, kind)
		}
		// What the unit deploys: its own terraform { source }, else that of a
		// configuration it includes.
		sources := []string{terraformSource(text)}
		for _, m := range tgInclude.FindAllStringSubmatch(text, -1) {
			inc := includedFile(m[1], dir, hcl)
			if inc == "" {
				continue
			}
			link(factpath.Dir(inc), "include")
			sources = append(sources, terraformSource(stripHCLComments(load(inc))))
		}
		for _, src := range sources {
			if src == "" {
				continue
			}
			if local := localModule(src, dir); local != "" {
				link(local, "source")
			} else if _, set := props["module_source"]; !set {
				props["module_source"] = src
			}
			break
		}
		for _, m := range tgDependency.FindAllStringSubmatch(text, -1) {
			if cp := tgConfigPath.FindStringSubmatch(m[2]); cp != nil {
				link(relDir(dir, cp[1]), "dependency")
			}
		}
		for _, m := range tgDependencies.FindAllStringSubmatch(text, -1) {
			for _, q := range tgQuoted.FindAllStringSubmatch(m[1], -1) {
				link(relDir(dir, q[1]), "dependency")
			}
		}
		if hasModule[dir] {
			// The Terraform pass already made the directory a module; the unit
			// marker and source ride on a module fact of their own only when it
			// did not.
			continue
		}
		hasModule[dir] = true
		out = append(out, facts.Fact{Kind: facts.KindModule, Name: dir, File: dir, Props: props})
	}
	return out
}

// terraformSource returns the source a terraform { } block names, or "".
func terraformSource(text string) string {
	if m := tgTerraform.FindStringSubmatch(text); m != nil {
		if s := tgSource.FindStringSubmatch(m[1]); s != nil {
			return s[1]
		}
	}
	return ""
}

// includedFile resolves an include block's path: find_in_parent_folders("x.hcl")
// (or with no argument, the parent terragrunt.hcl or root.hcl) is the nearest
// ancestor directory's file of that name; a literal path is relative to the unit.
func includedFile(body, dir string, hcl map[string]bool) string {
	if m := tgParentLookup.FindStringSubmatch(body); m != nil {
		names := []string{m[1]}
		if m[1] == "" {
			names = []string{terragruntFile, "root.hcl"}
		}
		for d := factpath.Dir(dir); ; d = factpath.Dir(d) {
			for _, n := range names {
				if cand := factpath.Join(d, n); hcl[cand] {
					return cand
				}
			}
			if d == "." || d == "/" || d == "" {
				return ""
			}
		}
	}
	if m := tgPath.FindStringSubmatch(body); m != nil {
		if cand := factpath.Clean(factpath.Join(dir, m[1])); hcl[cand] {
			return cand
		}
	}
	return ""
}

// localModule resolves a relative module source to the module's directory:
// "../../modules/vpc//sub?ref=x" is modules/vpc/sub. A remote source is "".
func localModule(src, dir string) string {
	if !strings.HasPrefix(src, "./") && !strings.HasPrefix(src, "../") {
		return ""
	}
	if i := strings.IndexByte(src, '?'); i >= 0 {
		src = src[:i]
	}
	src = strings.Replace(src, "//", "/", 1)
	return factpath.Clean(factpath.Join(dir, src))
}

func relDir(dir, p string) string {
	if strings.HasPrefix(p, "/") {
		return ""
	}
	return factpath.Clean(factpath.Join(dir, p))
}

// stripHCLComments removes # and // line comments, so a commented-out block does
// not count.
func stripHCLComments(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}
