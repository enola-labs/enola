package goextractor

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
)

// goModule is one Go module of the repository: the path its go.mod declares
// and the repository-relative directory that go.mod sits in ("." at the root).
type goModule struct {
	path, dir string
}

// goModules is every Go module in the repository, longest module path first,
// so a module nested under another's path ("example.com/app/tools" beside
// "example.com/app") claims its own imports, and the modules their go.mod files
// require.
//
// A repository is not one module in general. A monorepo keeps one go.mod per
// service, often with none at the root, and a go.work names directories that
// each have their own. Read as one module from the root go.mod alone, every
// import between them was external and none resolved to a package directory,
// so the import edges between those packages did not exist.
//
// The requirements are what decide an import that a repository module's path is
// a prefix of. Go gives an import to the longest module path that matches it in
// the build list, so "gitea.dev/actions-proto-go/runner/v1" belongs to the
// required module gitea.dev/actions-proto-go, not to the repository's module
// gitea.dev. Matched on the repository's path alone, it was resolved to a
// directory "actions-proto-go/runner/v1" the repository does not have.
type goModules struct {
	list     []goModule
	required []string
}

// readModules finds the go.mod files among the files the extractor was given,
// which the ignore globs have already filtered, so a module under testdata/ or
// vendor/ is not one of the repository's. A go.work needs no reading of its
// own: the directories it uses have their own go.mod.
//
// A root go.mod the file list does not carry is still read, as it always was.
func readModules(repoPath string, files []string) goModules {
	var mods goModules
	add := func(goMod, dir string) bool {
		p, req := readGoMod(goMod)
		if p == "" {
			return false
		}
		mods.list = append(mods.list, goModule{path: p, dir: dir})
		mods.required = append(mods.required, req...)
		return true
	}
	root := false
	for _, f := range files {
		if factpath.Base(f) != "go.mod" {
			continue
		}
		dir := factpath.Dir(f)
		if add(filepath.Join(repoPath, filepath.FromSlash(f)), dir) && dir == "." {
			root = true
		}
	}
	if !root {
		add(filepath.Join(repoPath, "go.mod"), ".")
	}
	sort.Slice(mods.list, func(i, j int) bool {
		a, b := mods.list[i], mods.list[j]
		if len(a.path) != len(b.path) {
			return len(a.path) > len(b.path)
		}
		return a.dir < b.dir
	})
	sort.Strings(mods.required)
	return mods
}

// rootPath is the module path of the go.mod at the repository root, or "".
func (m goModules) rootPath() string {
	for _, mod := range m.list {
		if mod.dir == "." {
			return mod.path
		}
	}
	return ""
}

// resolve maps an import path to the repository-relative directory of the
// package it names, when one of the repository's modules owns it: the longest
// repository module path that matches, unless a required module's path matches
// it for longer.
func (m goModules) resolve(importPath string) (string, bool) {
	for _, mod := range m.list {
		if !underModule(importPath, mod.path) {
			continue
		}
		for _, r := range m.required {
			if len(r) > len(mod.path) && underModule(importPath, r) {
				return "", false
			}
		}
		if importPath == mod.path {
			return mod.dir, true
		}
		rest := importPath[len(mod.path)+1:]
		if mod.dir == "." {
			return rest, true
		}
		return path.Join(mod.dir, rest), true
	}
	return "", false
}

// underModule reports whether importPath is modulePath or a package inside it.
func underModule(importPath, modulePath string) bool {
	return importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/")
}

// readGoMod reads the module path a go.mod file declares and the module paths
// it requires, in both the one-line and the block form. Versions, comments and
// the other directives are not needed and not read.
func readGoMod(goMod string) (string, []string) {
	data, err := os.ReadFile(goMod)
	if err != nil {
		return "", nil
	}
	var module string
	var required []string
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		switch {
		case inBlock && line == ")":
			inBlock = false
		case inBlock:
			if f := strings.Fields(line); len(f) > 0 {
				required = append(required, strings.Trim(f[0], `"`))
			}
		case line == "require (":
			inBlock = true
		case strings.HasPrefix(line, "require "):
			if f := strings.Fields(strings.TrimPrefix(line, "require ")); len(f) > 0 {
				required = append(required, strings.Trim(f[0], `"`))
			}
		case strings.HasPrefix(line, "module ") && module == "":
			module = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`)
		}
	}
	return module, required
}
