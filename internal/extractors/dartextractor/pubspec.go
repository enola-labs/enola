package dartextractor

import (
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/linescan"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pubPackage is one pub package: the unit Dart actually compiles and publishes, and
// the unit inside which circular imports are legal.
type pubPackage struct {
	// Name is the `name:` from pubspec.yaml — the identifier a `package:` URI names.
	Name string
	// Dir is the package root, repo-relative and slash-separated ("" for the repo root).
	Dir string
	// Deps are the direct dependency names (dependencies + dev_dependencies), used
	// only to describe the package; per-file framework gating keys on the file's own
	// imports instead, which is strictly stronger. See gatingNote in dart.go.
	Deps map[string]bool
	// IsFlutter is true when the package depends on the flutter SDK.
	IsFlutter bool
}

// packageIndex resolves a repo's pub packages: which one owns a given file, and which
// directory a `package:<name>/...` URI points at.
//
// A Dart repository is frequently a WORKSPACE of many packages (melos, or simply a
// packages/ directory) — flutter/packages holds over a hundred — so "the package" is
// never assumed to be the repo. Every pubspec.yaml is read up front, which is cheap
// (a line scan, no YAML parser and no tree-sitter), so each file's walk can resolve its
// own imports synchronously instead of needing a second pass.
type packageIndex struct {
	byName map[string]*pubPackage
	// dirs is every package directory, longest first, so ownerOf can attribute a file
	// to the NEAREST enclosing package rather than to whichever matched first. A
	// workspace root frequently has its own pubspec beside the packages it contains.
	dirs []*pubPackage
}

// scanPubspecs finds every pubspec.yaml in the repository, reading the tree directly
// rather than taking the walker's file list.
//
// It has to bypass the walker because `**/*.yaml` is in the default ignore globs, so a
// pubspec.yaml NEVER reaches an extractor — measured on appflowy: 0 of 4,114 walked
// files. The globs exist to suppress config and data noise, and a pubspec is neither:
// it is the definition of the compilation unit. Without it the package index is empty,
// which silently breaks three things at once — modules carry no `pub_package` (so the
// cycles explainer calls a legal Dart cycle a build defect), the repo's own
// `package:` imports classify as external instead of internal, and Flutter is never
// detected from the manifest.
//
// This is the same deliberate bypass the OpenAPI extractor and PHP's Symfony route
// config already make, for the same stated reason.
func scanPubspecs(repoPath string) []string {
	var out []string
	_ = filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDetectDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "pubspec.yaml" {
			if rel, rerr := filepath.Rel(repoPath, path); rerr == nil {
				out = append(out, factpath.Slash(rel))
			}
		}
		return nil
	})
	// Sorted so the index — and therefore which package wins a duplicate name — does
	// not depend on directory iteration order.
	sort.Strings(out)
	return out
}

// buildPackageIndex reads every pubspec.yaml in the file list.
func buildPackageIndex(repoPath string, pubspecs []string) *packageIndex {
	idx := &packageIndex{byName: map[string]*pubPackage{}}
	for _, rel := range pubspecs {
		p := parsePubspec(filepath.Join(repoPath, rel))
		if p == nil || p.Name == "" {
			continue
		}
		p.Dir = factpath.Dir(rel)
		if p.Dir == "." {
			p.Dir = ""
		}
		// A duplicate package name across a repo is real (a package and its example
		// app, or two vendored copies). The shallower one wins, which keeps the
		// canonical package ahead of a nested example.
		if prev, ok := idx.byName[p.Name]; !ok || len(p.Dir) < len(prev.Dir) {
			idx.byName[p.Name] = p
		}
		idx.dirs = append(idx.dirs, p)
	}
	sort.Slice(idx.dirs, func(i, j int) bool {
		if len(idx.dirs[i].Dir) != len(idx.dirs[j].Dir) {
			return len(idx.dirs[i].Dir) > len(idx.dirs[j].Dir)
		}
		return idx.dirs[i].Dir < idx.dirs[j].Dir
	})
	return idx
}

// ownerOf returns the package owning a repo-relative file, or nil.
func (idx *packageIndex) ownerOf(relFile string) *pubPackage {
	f := filepath.ToSlash(relFile)
	for _, p := range idx.dirs {
		if p.Dir == "" || strings.HasPrefix(f, p.Dir+"/") {
			return p
		}
	}
	return nil
}

// resolvePackageURI maps a `package:<name>/<path>` URI to the repo-relative file it
// names, and reports whether the package is one this repo declares. A package the repo
// does not declare is a third-party dependency, which is the common case and is exactly
// what makes the dependency `external` rather than `internal`.
//
// Dart's convention is that `package:foo/bar.dart` resolves to `<foo's dir>/lib/bar.dart`;
// the `lib/` is implicit in the URI and has to be put back.
func (idx *packageIndex) resolvePackageURI(uri string) (relFile string, internal bool) {
	rest, ok := strings.CutPrefix(uri, "package:")
	if !ok {
		return "", false
	}
	name, path, ok := strings.Cut(rest, "/")
	if !ok {
		return "", false
	}
	p := idx.byName[name]
	if p == nil {
		return "", false
	}
	if p.Dir == "" {
		return "lib/" + path, true
	}
	return p.Dir + "/lib/" + path, true
}

// parsePubspec reads the fields this extractor needs from a pubspec.yaml.
//
// Deliberately a line scan rather than a YAML parse, for the same reason the Rust
// extractor scans Cargo.toml that way: the three things needed here (the package name,
// whether flutter is a dependency, and the direct dependency names) are all top-level
// or one level in, and a real parser would add a dependency and a failure mode to read
// what a dozen lines of scanning reads exactly.
func parsePubspec(path string) *pubPackage {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	p := &pubPackage{Deps: map[string]bool{}}
	section := ""
	sc := linescan.New(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		if indent == 0 {
			key, val, _ := strings.Cut(trimmed, ":")
			key = strings.TrimSpace(key)
			switch key {
			case "name":
				p.Name = strings.Trim(strings.TrimSpace(val), `"'`)
				section = ""
			case "dependencies", "dev_dependencies", "dependency_overrides":
				section = "deps"
			default:
				section = ""
			}
			continue
		}
		// One level in, under a dependency block: `  <name>:` is a dependency.
		if section == "deps" && indent <= 2 {
			name, _, ok := strings.Cut(trimmed, ":")
			if !ok {
				continue
			}
			name = strings.TrimSpace(name)
			if name == "" || strings.HasPrefix(name, "-") {
				continue
			}
			p.Deps[name] = true
			if name == "flutter" {
				p.IsFlutter = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		log.Printf("[dart-extractor] error reading %s: %v", path, err)
	}
	return p
}
