package pythonextractor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
	"gopkg.in/yaml.v3"
)

var configuredPythonPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z_0-9]*(\.[A-Za-z_][A-Za-z_0-9]*)*$`)

func isPythonConfigFile(file string) bool {
	switch strings.ToLower(filepath.Ext(file)) {
	case ".yaml", ".yml", ".ini", ".cfg":
		return true
	}
	base := filepath.Base(file)
	return base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.")
}

// Configuration may belong to another extractor, but still affects Python refs.
func (e *PythonExtractor) AffectsKey(file string) bool {
	return isPythonConfigFile(file) || filepath.Base(file) == "pyproject.toml"
}

// configuredPythonSymbol accepts a whole import path, not prose containing one.
// Factory notation (pkg.module:factory) denotes the same symbol as dotted notation.
func configuredPythonSymbol(value string) string {
	parts := strings.Split(value, ":")
	if len(parts) == 2 && configuredPythonPath.MatchString(parts[0]) && configuredPythonPath.MatchString(parts[1]) {
		return parts[0] + "." + parts[1]
	}
	if len(parts) == 1 && strings.Contains(value, ".") && configuredPythonPath.MatchString(value) {
		return value
	}
	return ""
}

// Configuration references carry no production symbols or coupling. Require an
// actual symbol in the snapshot: arbitrary dotted values, prose and external
// packages must not hide unrelated dead functions through short-name matching.
func extractConfiguredPythonRefs(ctx context.Context, repoPath string, files, pyFiles []string, ff []facts.Fact) []facts.Fact {
	modules := make(map[string]bool, len(pyFiles))
	for _, file := range pyFiles {
		modules[strings.TrimSuffix(file, ".py")] = true
	}
	pkgDirs := packageDirs(pyFiles)
	fileIdx := buildSuffixIndex(modules, pkgDirs)
	roots := importableRoots(modules, pkgDirs)
	reexports := buildReexportIndex(ff, pkgDirs)
	symbols := make(map[string]bool)
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			symbols[f.Name] = true
		}
	}
	var configFiles []string
	for _, file := range files {
		if isPythonConfigFile(file) {
			configFiles = append(configFiles, file)
		}
	}
	perFile := parallel.MapFiles(ctx, configFiles, func(file string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, file))
		if err != nil {
			return nil
		}
		seen := make(map[string]bool)
		var rels []facts.Relation
		for _, value := range configuredPythonValues(file, src) {
			path := configuredPythonSymbol(value)
			if path == "" {
				continue
			}
			target, keep := resolveDottedTarget(path, fileIdx, roots, fileDir(file), reexports, symbols)
			if !keep || !symbols[target] {
				// A Lambda Docker CMD commonly uses a sibling module: app.handler.
				dot := strings.LastIndexByte(path, '.')
				localModule := filepath.Join(fileDir(file), strings.ReplaceAll(path[:dot], ".", "/"))
				local := filepath.ToSlash(localModule) + path[dot:]
				if !symbols[local] {
					continue
				}
				target = local
			}
			if !seen[target] {
				seen[target] = true
				rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: target})
			}
		}
		if len(rels) == 0 {
			return nil
		}
		return []facts.Fact{{Kind: facts.KindFileRef, Name: file, File: file, Line: 1,
			Props: map[string]any{"language": "python", "reference_source": "configuration"}, Relations: rels}}
	})
	var out []facts.Fact
	for _, refs := range perFile {
		out = append(out, refs...)
	}
	return out
}

func configuredPythonValues(file string, src []byte) []string {
	var values []string
	switch strings.ToLower(filepath.Ext(file)) {
	case ".yaml", ".yml":
		var root yaml.Node
		if yaml.Unmarshal(src, &root) != nil {
			return nil
		}
		var walk func(*yaml.Node)
		walk = func(n *yaml.Node) {
			if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
				values = append(values, n.Value)
			}
			for i, child := range n.Content {
				if n.Kind == yaml.MappingNode && i%2 == 0 {
					continue // keys describe settings; only values name entry points
				}
				walk(child)
			}
		}
		walk(&root)
	case ".ini", ".cfg":
		for _, line := range strings.Split(string(src), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			if _, value, ok := strings.Cut(line, "="); ok {
				values = append(values, strings.TrimSpace(value))
			}
		}
	default:
		for _, line := range strings.Split(string(src), "\n") {
			instruction, value, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok || (strings.ToUpper(instruction) != "CMD" && strings.ToUpper(instruction) != "ENTRYPOINT") {
				continue
			}
			var args []string
			if json.Unmarshal([]byte(strings.TrimSpace(value)), &args) == nil && len(args) == 1 {
				values = append(values, args[0])
			}
		}
	}
	return values
}
