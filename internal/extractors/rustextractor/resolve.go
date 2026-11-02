package rustextractor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// crateInfo maps one Cargo.toml's declared package name to the directory
// containing it.
type crateInfo struct {
	name string // underscore-normalized identifier, as it appears in `use` paths
	dir  string // slash-normalized dir containing the manifest ("." for repo root)
	// publishable: the manifest states what crates.io requires to publish (a
	// description and a license) and does not opt out with `publish = false`.
	publishable bool
	// renames maps a dependency's local name to the crate it is, for
	// `alias = { package = "real-name" }`.
	renames map[string]string
	// deps holds the local names of every dependency the manifest declares.
	deps map[string]bool
}

var (
	tomlSectionRe       = regexp.MustCompile(`^\s*\[([^\]]+)\]`)
	tomlNameRe          = regexp.MustCompile(`^\s*name\s*=\s*"([^"]*)"`)
	tomlDependencyKeyRe = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)(?:\.workspace)?\s*=`)
	tomlRenameRe        = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)\s*=\s*\{[^}]*\bpackage\s*=\s*"([^"]+)"`)
	tomlPublishRe       = regexp.MustCompile(`^\s*(publish|description|license|license-file)(\.workspace)?\s*=\s*(.*)$`)
)

// buildCrateIndex reads every Cargo.toml in the repo and returns each crate's
// [package] name mapped to its manifest directory. Cargo auto-converts
// hyphens to underscores for the identifier used in `use` paths, so the name
// is normalized the same way.
func buildCrateIndex(repoPath string, cargoFiles []string) []crateInfo {
	var crates []crateInfo
	for _, rel := range cargoFiles {
		data, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		name := parseCargoPackageName(string(data))
		if name == "" {
			continue
		}
		dir := factpath.Dir(rel)
		pkg := strings.ReplaceAll(name, "-", "_")
		publishable := parseCargoPublishable(string(data))
		crates = append(crates, crateInfo{
			name:        pkg,
			dir:         dir,
			publishable: publishable,
		})
		crates[len(crates)-1].renames, crates[len(crates)-1].deps = parseCargoRenames(string(data))
		// `use` paths name the library target, which `[lib] name` can set apart from
		// the package.
		if lib := strings.ReplaceAll(parseCargoLibName(string(data)), "-", "_"); lib != "" && lib != pkg {
			crates = append(crates, crateInfo{name: lib, dir: dir, publishable: publishable})
		}
	}
	return crates
}

// parseCargoPackageName scans a Cargo.toml's [package] section for its `name`
// key using a minimal line-based TOML scan (mirroring the Python extractor's
// pyproject.toml handling) rather than a full TOML parser.
func parseCargoPackageName(data string) string {
	inPackage := false
	for _, line := range strings.Split(data, "\n") {
		if m := tomlSectionRe.FindStringSubmatch(line); m != nil {
			inPackage = strings.TrimSpace(m[1]) == "package"
			continue
		}
		if !inPackage {
			continue
		}
		if m := tomlNameRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// parseCargoLibName returns the `name` of a Cargo.toml's [lib] section, "" when it has none.
func parseCargoLibName(data string) string {
	inLib := false
	for _, line := range strings.Split(data, "\n") {
		if m := tomlSectionRe.FindStringSubmatch(line); m != nil {
			inLib = strings.TrimSpace(m[1]) == "lib"
			continue
		}
		if !inLib {
			continue
		}
		if m := tomlNameRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// tomlDepTableRe matches a dependency written as its own table, `[dependencies.short]`
// or `[target.'cfg(unix)'.dev-dependencies.short]`.
var tomlDepTableRe = regexp.MustCompile(`(?:^|\.)(?:dev-|build-)?dependencies\.([A-Za-z0-9_-]+)$`)

// tomlPackageRe matches the `package = "real-name"` line of a dependency table.
var tomlPackageRe = regexp.MustCompile(`^\s*package\s*=\s*"([^"]+)"`)

// parseCargoRenames returns the dependencies a Cargo.toml renames, local name (with
// hyphens as underscores, as `use` paths spell it) to the real crate name, and the
// local names of all its dependencies.
func parseCargoRenames(data string) (renames map[string]string, deps map[string]bool) {
	norm := func(n string) string { return strings.ReplaceAll(n, "-", "_") }
	addDep := func(name string) {
		if deps == nil {
			deps = map[string]bool{}
		}
		deps[norm(name)] = true
	}
	inDeps, table := false, ""
	for _, line := range strings.Split(data, "\n") {
		if m := tomlSectionRe.FindStringSubmatch(line); m != nil {
			sec := strings.TrimSpace(m[1])
			table = ""
			if t := tomlDepTableRe.FindStringSubmatch(sec); t != nil {
				table = t[1]
				addDep(table)
			}
			inDeps = table == "" && strings.HasSuffix(sec, "dependencies")
			continue
		}
		switch {
		case table != "":
			if m := tomlPackageRe.FindStringSubmatch(line); m != nil {
				if renames == nil {
					renames = map[string]string{}
				}
				renames[norm(table)] = norm(m[1])
			}
		case inDeps:
			if m := tomlDependencyKeyRe.FindStringSubmatch(line); m != nil {
				addDep(m[1])
			}
			if m := tomlRenameRe.FindStringSubmatch(line); m != nil {
				if renames == nil {
					renames = map[string]string{}
				}
				renames[norm(m[1])] = norm(m[2])
			}
		}
	}
	return renames, deps
}

// parseCargoPublishable reports whether a Cargo.toml's [package] section can go to
// crates.io: it has a description and a license (or license-file), inherited from
// the workspace or not, and no `publish = false` or `publish = []`. Cargo makes every
// crate publishable by default, so the default says nothing: an application's own
// workspace crates rarely carry the metadata a registry demands.
func parseCargoPublishable(data string) bool {
	inPackage := false
	var description, license bool
	for _, line := range strings.Split(data, "\n") {
		if m := tomlSectionRe.FindStringSubmatch(line); m != nil {
			inPackage = strings.TrimSpace(m[1]) == "package"
			continue
		}
		if !inPackage {
			continue
		}
		m := tomlPublishRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch m[1] {
		case "publish":
			if v := strings.TrimSpace(m[3]); m[2] == "" && (strings.HasPrefix(v, "false") || strings.HasPrefix(v, "[]")) {
				return false
			}
		case "description":
			description = true
		case "license", "license-file":
			license = true
		}
	}
	return description && license
}

// nearestCrateDir returns the manifest directory of the crate that owns dir:
// the crate whose directory is the longest matching ancestor of dir.
func nearestCrateDir(dir string, crates []crateInfo) string {
	best, bestLen := "", -1
	for _, c := range crates {
		if c.dir == "." {
			if bestLen < 0 {
				best, bestLen = c.dir, 0
			}
			continue
		}
		if dir == c.dir || strings.HasPrefix(dir, c.dir+"/") {
			if len(c.dir) > bestLen {
				best, bestLen = c.dir, len(c.dir)
			}
		}
	}
	return best
}

// isReexportingFacade reports whether the crate a dependency is renamed to depends on a
// crate of the alias's own name. That is how a workspace facade (`core` mapped to
// `rustc-std-workspace-core`) re-exports the real crate, so the alias keeps naming it.
func isReexportingFacade(real, alias string, crates []crateInfo) bool {
	for _, c := range crates {
		if c.name == real && c.deps[alias] {
			if _, ok := crateDirByName(alias, crates); ok {
				return true
			}
		}
	}
	return false
}

func crateDirByName(name string, crates []crateInfo) (string, bool) {
	for _, c := range crates {
		if c.name == name {
			return c.dir, true
		}
	}
	return "", false
}

func crateNameByDir(dir string, crates []crateInfo) (string, bool) {
	for _, c := range crates {
		if c.dir == dir {
			return c.name, true
		}
	}
	return "", false
}

// rustStdlibCrates are the standard/core library crates always available
// without a Cargo dependency.
var rustStdlibCrates = map[string]bool{
	"std": true, "core": true, "alloc": true, "proc_macro": true, "test": true,
}

// classifyUsePath resolves a `use` path (split on "::", with a leading
// "crate"/"self"/"super" segment when present) into a dependency target and a
// source classification, from the perspective of a file in directory dir
// belonging to the crate rooted at crateDir.
func classifyUsePath(segs []string, dir, crateDir string, crates []crateInfo, moduleDirs map[string]bool) (target, source string) {
	if len(segs) == 0 {
		return "", "external"
	}
	switch segs[0] {
	case "self":
		return joinRustPath(dir, segs[1:], moduleDirs), "internal"
	case "super":
		base, rest := dir, segs
		for len(rest) > 0 && rest[0] == "super" {
			base = parentRustDir(base)
			rest = rest[1:]
		}
		return joinRustPath(base, rest, moduleDirs), "internal"
	case "crate":
		return joinRustPath(crateSourceRoot(crateDir, moduleDirs), segs[1:], moduleDirs), "internal"
	}
	name := segs[0]
	for _, c := range crates {
		if c.dir != crateDir {
			continue
		}
		if real := c.renames[name]; real != "" && !isReexportingFacade(real, name, crates) {
			name = real
		}
		break
	}
	if d, ok := crateDirByName(name, crates); ok {
		return joinRustPath(crateSourceRoot(d, moduleDirs), segs[1:], moduleDirs), "internal"
	}
	if rustStdlibCrates[segs[0]] {
		return strings.Join(segs, "::"), "stdlib"
	}
	return strings.Join(segs, "::"), "external"
}

// crateSourceRoot returns the effective module-tree root for a crate whose
// Cargo.toml lives at crateDir. The manifest directory itself is almost never
// where source files live — by convention they sit in a "src" subdirectory —
// so when crateDir isn't itself a known module directory but "crateDir/src"
// is, resolution should start there instead of at the manifest directory.
func crateSourceRoot(crateDir string, moduleDirs map[string]bool) string {
	if moduleDirs[crateDir] {
		return crateDir
	}
	if src := crateDir + "/src"; moduleDirs[src] {
		return src
	}
	if crateDir == "." && moduleDirs["src"] {
		return "src"
	}
	return crateDir
}

// joinRustPath resolves a module-relative path under base to a known module
// directory. Rust's directory-per-module convention is not exact: a `mod foo;`
// submodule can live either in a sibling file (base/foo.rs, same directory as
// its parent) or in its own subdirectory (base/foo/mod.rs or base/foo/*.rs).
// Trying progressively shorter suffixes against the known module-directory set
// (mirrors the Python extractor's suffix-index resolution) picks the real
// submodule directory when one exists, and falls back to base — the sibling-
// file case — otherwise.
func joinRustPath(base string, rest []string, moduleDirs map[string]bool) string {
	for end := len(rest); end >= 1; end-- {
		cand := strings.Join(rest[:end], "/")
		full := cand
		if base != "" && base != "." {
			full = base + "/" + cand
		}
		if moduleDirs[full] {
			return full
		}
	}
	return base
}

func parentRustDir(dir string) string {
	if dir == "" || dir == "." {
		return "."
	}
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		return dir[:i]
	}
	return "."
}

// implPair records an `impl Trait for Type` observation so applyImplements can
// attach the relation to Type's own symbol fact once all files are merged —
// impl blocks are frequently declared in a different file than the type they
// extend, so the edge cannot be attached at walk time.
type implPair struct {
	typeName  string
	traitName string
	members   []int // indexes of the impl's own items in the fact list
}

// applyImplements mutates, in place, the KindSymbol fact matching each impl
// pair's typeName to add a RelImplements relation. Attaching the edge to the
// type's own existing fact (rather than emitting a new one) avoids
// double-counting the type in symbol-kind stats, and avoids any ordering
// dependency on whether the type or the impl block was parsed first.
func applyImplements(allFacts []facts.Fact, impls []implPair) {
	if len(impls) == 0 {
		return
	}
	byName := make(map[string]int, len(allFacts))
	declaredTraits := map[string]bool{}
	for i := range allFacts {
		if allFacts[i].Kind != facts.KindSymbol {
			continue
		}
		if _, exists := byName[allFacts[i].Name]; !exists {
			byName[allFacts[i].Name] = i
		}
		if allFacts[i].PropAny("symbol_kind") == facts.SymbolInterface {
			declaredTraits[allFacts[i].Name[strings.LastIndex(allFacts[i].Name, ".")+1:]] = true
		}
	}
	for _, p := range impls {
		if !declaredTraits[p.traitName] {
			for _, idx := range p.members {
				allFacts[idx].SetProp("override", true)
			}
		}
		idx, ok := byName[p.typeName]
		if !ok {
			continue
		}
		allFacts[idx].Relations = append(allFacts[idx].Relations, facts.Relation{
			Kind:   facts.RelImplements,
			Target: p.traitName,
		})
	}
}

// dropUndeclaredPreludeNames removes the references to Vec, Option and the other
// prelude types from the facts that name them, unless the repository declares a
// type of that name: only then can the reference be to something in it.
func dropUndeclaredPreludeNames(allFacts []facts.Fact) {
	declared := map[string]bool{}
	for i := range allFacts {
		if allFacts[i].Kind != facts.KindSymbol {
			continue
		}
		n := allFacts[i].Name
		if short := n[strings.LastIndex(n, ".")+1:]; rustPreludeTypes[short] {
			declared[short] = true
		}
	}
	for i := range allFacts {
		f := &allFacts[i]
		kind := facts.RelNames
		if f.Kind == facts.KindTestRef {
			kind = facts.RelCalls
		}
		kept := f.Relations[:0]
		for _, r := range f.Relations {
			if r.Kind == kind && rustPreludeTypes[r.Target] && !declared[r.Target] {
				continue
			}
			kept = append(kept, r)
		}
		f.Relations = kept
	}
}
