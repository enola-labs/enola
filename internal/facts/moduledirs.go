package facts

import "strings"

// ModuleDirs maps a directory to the module that lives there, for modules not named by
// their directory: a Go package is named by its import path ("example.com/app/
// internal/config") and its directory is its File ("internal/config"). Every other
// module is named by its directory and needs no entry. A nil ModuleDirs is empty.
//
// Keyed by the module's File as stored, which a multi-repo snapshot repo-prefixes, so
// two repositories' internal/config stay two keys; and by repository and the
// repo-stripped directory, the second form a fact's file is resolved by.
type ModuleDirs map[string]string

// NewModuleDirs indexes the Go packages among ff, the modules named otherwise than by
// their directory.
func NewModuleDirs(ff []Fact) ModuleDirs {
	dirs := ModuleDirs{}
	for _, m := range ff {
		if !IsGoPackage(m) || m.File == "" || m.File == m.Name {
			continue
		}
		dirs[m.File] = m.Name
		if m.Repo != "" {
			dirs[m.Repo+"\x00"+strings.TrimPrefix(m.File, m.Repo+"/")] = m.Name
		}
	}
	return dirs
}

// Name returns the module living in dir of repo's tree, or "".
func (d ModuleDirs) Name(repo, dir string) string {
	if len(d) == 0 {
		return ""
	}
	if n, ok := d[dir]; ok {
		return n
	}
	if repo != "" {
		return d[repo+"\x00"+dir]
	}
	return ""
}

// IsGoPackage reports a Go package's module fact: named by its import path, filed
// under its directory. Other languages' modules may also be named otherwise than
// their File, for reasons of their own (a Swift target, a packwerk pack), and are
// resolved as they always were.
func IsGoPackage(m Fact) bool {
	return m.Kind == KindModule && m.PropString("language") == "go"
}

// ModulePath is the path a module's location heuristics read: whether it is test code,
// generated, a fixture. For every module it is its name, except a Go package, named
// by its import path, whose directory is the path: "example.com/e2e/a" is the
// package in directory a, and the e2e in its module path says nothing about it.
func ModulePath(m Fact) string {
	if IsGoPackage(m) && m.File != "" && m.File != m.Name {
		if m.Repo != "" {
			return strings.TrimPrefix(m.File, m.Repo+"/")
		}
		return m.File
	}
	return m.Name
}

// ModulePaths maps each Go package's name to its ModulePath, for a caller holding
// module names rather than module facts. A name not in it is its own path.
type ModulePaths map[string]string

// NewModulePaths indexes the Go packages among ff.
func NewModulePaths(ff []Fact) ModulePaths {
	paths := ModulePaths{}
	for _, m := range ff {
		if IsGoPackage(m) && m.File != "" && m.File != m.Name {
			paths[m.Name] = ModulePath(m)
		}
	}
	return paths
}

// Of returns the path of the module named name.
func (p ModulePaths) Of(name string) string {
	if path, ok := p[name]; ok {
		return path
	}
	return name
}
