package rubyextractor

import (
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"gopkg.in/yaml.v3"
)

// packwerkConfig represents the root packwerk.yml configuration.
type packwerkConfig struct {
	PackagePaths []string `yaml:"package_paths"`
	Exclude      []string `yaml:"exclude"`
}

// packageConfig represents a single package.yml file.
type packageConfig struct {
	EnforceDependencies bool           `yaml:"enforce_dependencies"`
	EnforcePrivacy      bool           `yaml:"enforce_privacy"`
	Dependencies        []string       `yaml:"dependencies"`
	Metadata            map[string]any `yaml:"metadata"`
}

// packwerkPackage holds parsed package info used during extraction.
type packwerkPackage struct {
	path                string
	enforceDependencies bool
	enforcePrivacy      bool
	dependencies        []string
}

// packwerkInfo holds all packwerk metadata for the repository.
type packwerkInfo struct {
	detected bool
	packages map[string]*packwerkPackage // keyed by package path relative to repo root
	facts    []facts.Fact
}

// ownerPackage returns the packwerk package path that owns the given file, or "".
func (p *packwerkInfo) ownerPackage(relFile string) string {
	if p == nil || len(p.packages) == 0 {
		return ""
	}
	// Find the longest matching package path (most specific).
	best := ""
	for pkgPath := range p.packages {
		if pkgPath == "." {
			if best == "" {
				best = "."
			}
			continue
		}
		prefix := pkgPath + "/"
		if strings.HasPrefix(relFile, prefix) {
			if len(pkgPath) > len(best) {
				best = pkgPath
			}
		}
	}
	return best
}

// isPackage returns true if the directory is a packwerk package root.
func (p *packwerkInfo) isPackage(dir string) bool {
	if p == nil || len(p.packages) == 0 {
		return false
	}
	_, ok := p.packages[dir]
	return ok
}

// parsePackwerk detects packwerk.yml and parses all package.yml files.
// Returns a packwerkInfo with module facts and the privacy boundary map.
func parsePackwerk(repoPath string) *packwerkInfo {
	info := &packwerkInfo{
		packages: make(map[string]*packwerkPackage),
	}

	// Check for packwerk.yml.
	packwerkPath := filepath.Join(repoPath, "packwerk.yml")
	data, err := os.ReadFile(packwerkPath)
	if err != nil {
		return info
	}

	info.detected = true
	log.Printf("[ruby-extractor] packwerk.yml detected")

	var cfg packwerkConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Printf("[ruby-extractor] error parsing packwerk.yml: %v", err)
		return info
	}

	// Default package paths if not specified.
	packagePaths := cfg.PackagePaths
	if len(packagePaths) == 0 {
		packagePaths = []string{".", "packages/*"}
	}

	// Find all package.yml files.
	var packageDirs []string
	for _, pattern := range packagePaths {
		if pattern == "." {
			// Root package.
			if _, err := os.Stat(filepath.Join(repoPath, "package.yml")); err == nil {
				packageDirs = append(packageDirs, ".")
			}
			continue
		}

		// Expand glob patterns (e.g. "packages/*").
		matches, err := filepath.Glob(filepath.Join(repoPath, pattern))
		if err != nil {
			log.Printf("[ruby-extractor] error globbing %s: %v", pattern, err)
			continue
		}
		for _, match := range matches {
			pkgYml := filepath.Join(match, "package.yml")
			if _, err := os.Stat(pkgYml); err == nil {
				rel, err := filepath.Rel(repoPath, match)
				if err != nil {
					continue
				}
				packageDirs = append(packageDirs, factpath.Slash(rel))
			}
		}
	}

	log.Printf("[ruby-extractor] found %d packwerk packages", len(packageDirs))

	// Parse each package.yml and emit module facts.
	for _, pkgDir := range packageDirs {
		pkgYmlPath := filepath.Join(repoPath, pkgDir, "package.yml")
		pkgData, err := os.ReadFile(pkgYmlPath)
		if err != nil {
			log.Printf("[ruby-extractor] error reading %s: %v", pkgYmlPath, err)
			continue
		}

		var pkgCfg packageConfig
		if err := yaml.Unmarshal(pkgData, &pkgCfg); err != nil {
			log.Printf("[ruby-extractor] error parsing %s: %v", pkgYmlPath, err)
			continue
		}

		pkg := &packwerkPackage{
			path:                pkgDir,
			enforceDependencies: pkgCfg.EnforceDependencies,
			enforcePrivacy:      pkgCfg.EnforcePrivacy,
			dependencies:        pkgCfg.Dependencies,
		}
		info.packages[pkgDir] = pkg

		// Build module fact.
		props := map[string]any{
			"language":             "ruby",
			"framework":            "rails",
			"packwerk":             true,
			"module_role":          facts.ModuleRoleProduction,
			"enforce_dependencies": pkgCfg.EnforceDependencies,
			"enforce_privacy":      pkgCfg.EnforcePrivacy,
		}

		if pkgCfg.Metadata != nil {
			if ncd, ok := pkgCfg.Metadata["no_circular_dependencies"]; ok {
				props["no_circular_dependencies"] = ncd
			}
		}
		if todo := readPackageTodo(filepath.Join(repoPath, pkgDir, "package_todo.yml")); len(todo) > 0 {
			props[facts.PropPackwerkTodo] = todo
		}

		var rels []facts.Relation
		for _, dep := range pkgCfg.Dependencies {
			target := dep
			if target == "." {
				target = "root"
			}
			rels = append(rels, facts.Relation{
				Kind:   facts.RelDependsOn,
				Target: target,
			})
		}

		moduleName := pkgDir
		if moduleName == "." {
			moduleName = "root"
		}

		info.facts = append(info.facts, facts.Fact{
			Kind:      facts.KindModule,
			Name:      moduleName,
			File:      filepath.Join(pkgDir, "package.yml"),
			Props:     props,
			Relations: rels,
		})
	}

	return info
}

// packageTodo is package_todo.yml: per referenced package, per constant, the
// violations packwerk recorded and the files they occur in.
type packageTodo map[string]map[string]struct {
	Violations []string `yaml:"violations"`
}

// readPackageTodo returns the packages a package_todo.yml records a dependency
// violation against, sorted, in module-fact naming ("root" for "."). A missing or
// unreadable file records none: the file is optional, and a package without it
// has acknowledged nothing.
func readPackageTodo(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var todo packageTodo
	if err := yaml.Unmarshal(data, &todo); err != nil {
		log.Printf("[ruby-extractor] error parsing %s: %v", path, err)
		return nil
	}
	var out []string
	for pkg, constants := range todo {
		for _, c := range constants {
			if slices.Contains(c.Violations, "dependency") {
				if pkg == "." {
					pkg = "root"
				}
				out = append(out, pkg)
				break
			}
		}
	}
	slices.Sort(out)
	return out
}
