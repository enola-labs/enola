package swiftextractor

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// swiftLeafRole returns the role of a module no target describes: build-definition
// code (Tuist helpers, Xcode file templates) is tooling, the rest is read from its path.
func swiftLeafRole(dir string) string {
	slash := filepath.ToSlash(dir) + "/"
	if strings.HasPrefix(slash, "Tuist/") || strings.Contains(slash, "/Tuist/") || strings.Contains(slash, ".xctemplate/") {
		return facts.ModuleRoleTooling
	}
	return facts.ModuleRoleForPath(dir)
}

// --- targets laid out by convention ---
//
// A Tuist project is described in Swift (Project.swift), usually through helpers the
// project writes itself, so its targets cannot be read the way an XcodeGen project.yml
// is. What every such project shares is the layout Xcode itself encourages: each target
// keeps its sources in a folder named after the target, beside the project's manifest,
// and other code names it by that name in `import`. So a folder beside a Project.swift
// is a target when its name is a module some Swift file imports, or when it is a test
// bundle (named …Tests). Every folder of a project that is itself a test bundle is test
// code. Anything else under the project (resources, generated specs) stays in the
// project's own folder.

// swiftImportLine matches an import statement, capturing the module name.
var swiftImportLine = regexp.MustCompile(`(?m)^[ \t]*(?:@\w+[ \t]+)*import[ \t]+(?:(?:class|struct|enum|protocol|func|typealias|var|let)[ \t]+)?([A-Za-z_]\w*)`)

// conventionTarget is a target found by layout: its module name, source folder and role.
type conventionTarget struct {
	name, dir, role string
	manifest        string
}

// swiftPrescan is what one read of every Swift source yields for the passes that need
// the whole repository before the per-file walk: request base types and the modules
// the sources import.
type swiftPrescan struct {
	requestBases map[string]string
	imported     map[string]bool
}

// prescanSwift reads every Swift source once.
func prescanSwift(repoPath string, files []string) swiftPrescan {
	out := swiftPrescan{requestBases: map[string]string{}, imported: map[string]bool{}}
	for _, relFile := range files {
		if !isSwiftFile(relFile) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			continue
		}
		for _, m := range swiftImportLine.FindAllSubmatch(src, -1) {
			out.imported[string(m[1])] = true
		}
		if isSwiftTestFile(relFile) || !bytes.Contains(src, []byte("endpoint")) {
			continue
		}
		for _, b := range routeBases(string(src)) {
			if prev, ok := out.requestBases[b.name]; !ok || b.defaultVerb < prev {
				out.requestBases[b.name] = b.defaultVerb
			}
		}
	}
	return out
}

// isSwiftManifest reports a build description written in Swift, and the helpers a
// Tuist project keeps for its manifests: project definition, not a target's source.
func isSwiftManifest(f string) bool {
	slash := filepath.ToSlash(f)
	switch path.Base(slash) {
	case "Project.swift", "Package.swift", "Workspace.swift", "Tuist.swift", "Config.swift":
		return true
	}
	return strings.HasPrefix(slash, "Tuist/") || strings.Contains(slash, "/Tuist/")
}

// conventionTargets finds the targets beside each Project.swift.
func conventionTargets(files []string, imported map[string]bool) []conventionTarget {
	projects := map[string]bool{}
	for _, f := range files {
		if path.Base(filepath.ToSlash(f)) == "Project.swift" {
			projects[path.Dir(filepath.ToSlash(f))] = true
		}
	}
	if len(projects) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []conventionTarget
	for _, f := range files {
		if !isSwiftFile(f) || isSwiftManifest(f) {
			continue
		}
		// The innermost project owns the file.
		parts := strings.Split(filepath.ToSlash(f), "/")
		for i := len(parts) - 2; i >= 0; i-- {
			project := strings.Join(parts[:i], "/")
			if i == 0 {
				project = "."
			}
			if !projects[project] {
				continue
			}
			dir := strings.Join(parts[:i+1], "/")
			if seen[dir] {
				break
			}
			seen[dir] = true
			name := parts[i]
			manifest := path.Join(project, "Project.swift")
			switch {
			case strings.HasSuffix(path.Base(project), "Tests"):
				out = append(out, conventionTarget{name: name, dir: dir, role: facts.ModuleRoleTest, manifest: manifest})
			case strings.HasSuffix(name, "Tests"):
				out = append(out, conventionTarget{name: name, dir: dir, role: facts.ModuleRoleTest, manifest: manifest})
			case strings.HasSuffix(name, "Demo") || strings.HasSuffix(name, "DemoApp") || strings.HasSuffix(name, "DemoApplication"):
				// A demo app runs the feature on its own: code that ships nowhere.
				out = append(out, conventionTarget{name: name, dir: dir, role: facts.ModuleRoleTooling, manifest: manifest})
			case imported[name] && !swiftSystemFramework[name]:
				out = append(out, conventionTarget{name: name, dir: dir, role: facts.ModuleRoleProduction, manifest: manifest})
			}
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out
}

// addConventionTargets seeds the resolver with the targets found by layout. A target an
// XcodeGen project or a Package.swift already describes keeps that description: a
// convention target only claims a folder no declared target owns.
func (r *moduleResolver) addConventionTargets(targets []conventionTarget) {
	for _, t := range targets {
		if r.identities[t.dir] {
			continue
		}
		prio := 2
		if t.role == facts.ModuleRoleTest {
			prio = 1
		}
		r.identities[t.dir] = true
		r.entries = append(r.entries, moduleEntry{prefix: t.dir, identity: t.dir, priority: prio})
	}
}

// conventionTargetFacts emits a module fact per target found by layout. A production
// target carries its importable name as xcode_target, which is how resolveImports maps
// `import Name` to it; a name two folders share is left out, so an import of it stays
// unresolved rather than picking one.
func conventionTargetFacts(targets []conventionTarget, declared map[string]bool) []facts.Fact {
	count := map[string]int{}
	for _, t := range targets {
		if t.role == facts.ModuleRoleProduction {
			count[t.name]++
		}
	}
	var out []facts.Fact
	for _, t := range targets {
		if declared[t.dir] {
			continue
		}
		props := map[string]any{
			"language":         "swift",
			"module_role":      t.role,
			"project_manifest": t.manifest,
		}
		if t.role == facts.ModuleRoleProduction && count[t.name] == 1 {
			props["xcode_target"] = t.name
		}
		out = append(out, facts.Fact{Kind: facts.KindModule, Name: t.dir, File: t.dir, Props: props})
	}
	return out
}
