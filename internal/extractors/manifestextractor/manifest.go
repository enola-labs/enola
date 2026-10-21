// Package manifestextractor reads a repository's DECLARED direct dependencies
// out of its package manifests and emits one fact per package.
//
// Every other extractor here reads a manifest already — for detection, for
// resolution, for framework gating — and none of them emits anything about what
// it found. So a graph that knows which of its own modules import which has
// nothing to say about the far larger surface the repository pulls in from
// outside it, and the question "is every external dependency declared, pinned,
// and there for a stated reason" had no facts to be asked over.
//
// DIRECT dependencies only, deliberately. The transitive closure of a lockfile
// runs to tens of thousands of entries on an ordinary Node or Rust project, and
// every one of them would enter a graph whose cost is already dominated by
// node count. It is also the boundary the regulation draws: the Cyber Resilience
// Act's Annex I asks for a machine-readable bill of materials "covering at the
// very least the top-level dependencies", which is exactly this set.
//
// What this extractor does NOT do is connect a package to the code that uses
// it. Resolving an import path to a declared package is a per-ecosystem
// resolution problem, and guessing it would put edges in the graph that no
// parser proved — the one thing nothing here is allowed to do. A package is a
// leaf node: measured, named, versioned, and joined to code only by the
// declarations a human writes about it.
package manifestextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// The ecosystem vocabulary and the `package` type live in internal/facts,
// beside the via kinds, for the reason those do: a declaration validates
// against them at parse time, so the extractor that writes a value and the
// declaration that names one cannot come to disagree about the spelling.
const (
	EcosystemGo       = facts.EcosystemGo
	EcosystemNPM      = facts.EcosystemNPM
	EcosystemRubyGems = facts.EcosystemRubyGems
	EcosystemCargo    = facts.EcosystemCargo
	EcosystemPub      = facts.EcosystemPub
	EcosystemPyPI     = facts.EcosystemPyPI

	TypePackage = facts.TypePackage
)

// purlType maps an ecosystem to its Package URL type, so a fact's name is the
// identifier the rest of the world already uses for the same package.
var purlType = map[string]string{
	EcosystemGo:       "golang",
	EcosystemNPM:      "npm",
	EcosystemRubyGems: "gem",
	EcosystemCargo:    "cargo",
	EcosystemPub:      "pub",
	EcosystemPyPI:     "pypi",
}

// manifestReaders maps a manifest's base name to the parser that reads it. The
// map is also the detection predicate: a repository is interesting to this
// extractor exactly when it carries a file one of these names.
var manifestReaders = map[string]func(rc *readCtx, relFile string) []pkgDep{
	"go.mod":           readGoMod,
	"package.json":     readPackageJSON,
	"Gemfile":          readGemfile,
	"Cargo.toml":       readCargoToml,
	"pubspec.yaml":     readPubspec,
	"requirements.txt": readRequirements,
	"pyproject.toml":   readPyproject,
}

// pkgDep is one declared direct dependency before it becomes a fact.
type pkgDep struct {
	Name       string
	Ecosystem  string
	Constraint string
	Resolved   string
	Dev        bool
	Manifest   string
	// Path is where a Cargo path dependency lives, relative to its manifest: a
	// crate the repository may hold, so its name is never read as a package
	// from elsewhere.
	Path string
	// LockUnread names a lockfile that sits beside the manifest and that enola
	// cannot read. It is the difference between "this dependency resolves to no
	// version" and "something resolved it and we did not look" — and only the
	// first is a finding.
	LockUnread string
}

// Extractor emits one fact per declared direct dependency.
type Extractor struct{}

func New() *Extractor             { return &Extractor{} }
func (e *Extractor) Name() string { return "manifests" }

// Detect walks for itself, for the one caller with no engine walk to borrow.
// DetectFiles below is the answer the engine uses; both read the same predicate,
// so they cannot drift.
func (e *Extractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles reports whether any walked name is a manifest this extractor
// reads. The engine's names include files the ignore globs exclude, which is
// what makes this work at all: the bundled config ignores **/*.yaml, so a
// pubspec.yaml never reaches an extractor through the normal file list.
func (e *Extractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, f := range files {
		if manifestReaders[detectnames.Base(f)] != nil {
			return true, nil
		}
	}
	return false, nil
}

// OwnsFile scopes the incremental cache to the manifests and lockfiles this
// extractor reads, so an ordinary source edit reuses its facts and a manifest
// edit does not.
func (e *Extractor) OwnsFile(relFile string) bool {
	base := detectnames.Base(relFile)
	if manifestReaders[base] != nil {
		return true
	}
	return lockNames[base]
}

// Extract reads every manifest in the repository. It walks rather than taking
// the engine's file list for the reason DetectFiles states — the ignore globs
// hide the manifests — which is the same deliberate bypass the Dart, OpenAPI
// and AsyncAPI extractors already make, for the same reason.
func (e *Extractor) Extract(ctx context.Context, repoPath string, _ []string) ([]facts.Fact, error) {
	var deps []pkgDep
	rc := &readCtx{repoPath: repoPath, locks: map[string]map[string]string{}}
	names := detectnames.Walk(repoPath)
	sort.Strings(names)
	for _, rel := range names {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		read := manifestReaders[detectnames.Base(rel)]
		if read == nil {
			continue
		}
		deps = append(deps, read(rc, rel)...)
	}
	return factsFor(deps), nil
}

// factsFor turns parsed dependencies into facts, one per package.
//
// The graph is name-keyed, so two manifests declaring the same package would
// merge into one node whatever this function did. It therefore merges them here
// instead, where the merge can be a decision rather than an accident: a package
// that is a real dependency of any manifest is not a dev dependency of the
// repository, and a version resolved by any lockfile is better evidence than
// none. Deterministic because the manifests were sorted before parsing.
func factsFor(deps []pkgDep) []facts.Fact {
	byPurl := map[string]*pkgDep{}
	var order []string
	for i := range deps {
		d := deps[i]
		if d.Name == "" || d.Ecosystem == "" {
			continue
		}
		key := purl(d)
		prev, seen := byPurl[key]
		if !seen {
			copied := d
			byPurl[key] = &copied
			order = append(order, key)
			continue
		}
		if !d.Dev {
			prev.Dev = false
		}
		if prev.Path == "" && d.Path != "" {
			prev.Path = d.Path
		}
		if prev.Resolved == "" && d.Resolved != "" {
			prev.Resolved = d.Resolved
			prev.Manifest = d.Manifest
			prev.LockUnread = ""
		}
	}
	out := make([]facts.Fact, 0, len(order))
	for _, key := range order {
		d := *byPurl[key]
		out = append(out, facts.Fact{
			Kind:  facts.KindDependency,
			Name:  key,
			File:  d.Manifest,
			Props: props(d),
		})
	}
	return out
}

// props renders one dependency's measured properties.
//
// `pinned` is the whole of H14's checkable half, computed once here rather than
// left for a rule to infer from two other props that could disagree. It is
// emitted only when the answer is KNOWN, which is the part that matters: a
// dependency whose lockfile enola cannot read has not been shown to be
// unpinned, and saying so would make every repository using an unsupported
// lockfile fail a rule it does not break. Twelve such packages in excalidraw
// were reported unpinned by a version of this code that answered anyway, and
// twelve false blocks is how a gate gets removed from CI.
//
// The unknown case carries `unresolved_lock` instead, naming the file, so the
// absence is visible rather than merely quiet — and so a `where:` selector on
// `pinned` simply does not select it, which is the correct amount of silence.
func props(d pkgDep) map[string]any {
	p := map[string]any{
		"type":             TypePackage,
		"ecosystem":        d.Ecosystem,
		"package_name":     d.Name,
		"constraint":       d.Constraint,
		"resolved_version": d.Resolved,
		"dev":              d.Dev,
		"manifest":         d.Manifest,
	}
	if d.Path != "" {
		p["path"] = d.Path
	}
	switch {
	case d.Resolved != "" || isExactConstraint(d.Constraint):
		p["pinned"] = true
	case d.LockUnread != "":
		p["unresolved_lock"] = d.LockUnread
	default:
		p["pinned"] = false
	}
	return p
}

// purl renders the Package URL identity a fact is named by. It is the name the
// rest of the ecosystem already uses, which is what lets a declaration written
// against an advisory database or a bill of materials join to this graph
// without a translation table. Not percent-encoded: this is an identifier a
// person reads and a declaration types, and a scoped npm name is more legible
// as @scope/name than as %40scope%2Fname.
func purl(d pkgDep) string {
	t := purlType[d.Ecosystem]
	if t == "" {
		t = d.Ecosystem
	}
	name := d.Name
	// PyPI names are case- and separator-insensitive (PEP 503), so two manifests
	// spelling one package differently must not become two nodes. The written
	// spelling stays on package_name, which is what a declaration matches.
	if d.Ecosystem == EcosystemPyPI {
		name = normalizePyPIName(name)
	}
	return "pkg:" + t + "/" + name
}

// isExactConstraint reports whether a version constraint names exactly one
// version. Fail closed: anything this does not recognise as exact is treated as
// a range, because reporting an unpinned dependency as pinned is the one error
// with a consequence.
func isExactConstraint(c string) bool {
	c = strings.TrimSpace(c)
	// Equality operators, longest first: pypi spells it "==", Cargo "=". They
	// are stripped rather than rejected because "=1.2.3" names exactly one
	// version — tokio pins tracing-mock that way, and reading it as a range
	// reports a pinned dependency as unpinned.
	//
	// The order is load-bearing only for "==", since the range operators that
	// end in "=" (>=, <=, ~=, !=) all START with another character and so are
	// rejected by the operator screen below rather than mistaken for equality.
	c = strings.TrimSpace(strings.TrimPrefix(c, "=="))
	c = strings.TrimSpace(strings.TrimPrefix(c, "="))
	if c == "" {
		return false
	}
	// A range operator anywhere disqualifies it, wherever the ecosystem puts
	// one: ^1.2, ~>1.2, >=1.2, 1.*, 1.x, "1.2 || 1.3".
	if strings.ContainsAny(c, "^~<>*|,= ") {
		return false
	}
	if strings.HasSuffix(c, ".x") || strings.HasSuffix(c, ".X") {
		return false
	}
	// A leading digit, or a leading v before one, is what a version looks like
	// in every ecosystem here. A git ref, a file: path or a URL is not a pin
	// this vocabulary can verify, so it is not one.
	rest := strings.TrimPrefix(c, "v")
	return rest != "" && rest[0] >= '0' && rest[0] <= '9'
}

// readCtx carries one Extract call's repository root and its parsed-lockfile
// cache. The cache is what makes the ancestor search below affordable: a
// monorepo has one lockfile at its root and a manifest in every package, so
// without it a repository with fourteen package.json files would parse the same
// multi-megabyte yarn.lock fourteen times.
type readCtx struct {
	repoPath string
	// locks is keyed by the lockfile's repo-relative path. A nil value is a
	// cached miss, which matters as much as a hit: it is what stops an absent
	// lockfile from being stat'd once per manifest.
	locks map[string]map[string]string
}

// read returns a repository file's contents, or "" — a manifest that cannot be
// read contributes no dependencies, which is the same answer as a repository
// that does not have one, and neither should fail a snapshot.
func (rc *readCtx) read(relFile string) string {
	data, err := os.ReadFile(filepath.Join(rc.repoPath, filepath.FromSlash(relFile)))
	if err != nil {
		return ""
	}
	return string(data)
}

// lock finds the nearest lockfile at or above a manifest and parses it with
// parse, memoizing the result. It returns the resolved versions and the path it
// read, or an empty path when no lockfile of that name exists anywhere above.
//
// Searching upward is what a monorepo requires and what every package manager
// itself does: the lockfile lives at the workspace root, and the manifests it
// resolves sit in packages beneath it. Reading only the sibling reported five
// of excalidraw's dependencies as unpinned when its root yarn.lock pins all of
// them — a false answer, not a missing one.
func (rc *readCtx) lock(relFile, name string, parse func(text string) map[string]string) (map[string]string, string) {
	for dir := factpath.Dir(relFile); ; dir = factpath.Dir(dir) {
		path := name
		if dir != "." && dir != "" && dir != "/" {
			path = dir + "/" + name
		}
		if cached, seen := rc.locks[path]; seen {
			if cached != nil {
				return cached, path
			}
		} else {
			text := rc.read(path)
			if text == "" {
				rc.locks[path] = nil
			} else {
				resolved := parse(text)
				rc.locks[path] = resolved
				return resolved, path
			}
		}
		if dir == "." || dir == "" || dir == "/" {
			return nil, ""
		}
	}
}

// exists reports whether a file sits at or above a manifest, without parsing
// it — the question asked of a lockfile this extractor cannot read.
func (rc *readCtx) exists(relFile, name string) string {
	for dir := factpath.Dir(relFile); ; dir = factpath.Dir(dir) {
		path := name
		if dir != "." && dir != "" && dir != "/" {
			path = dir + "/" + name
		}
		if rc.read(path) != "" {
			return path
		}
		if dir == "." || dir == "" || dir == "/" {
			return ""
		}
	}
}

// lines splits a manifest into lines with comments and blanks removed, which is
// the shape every hand-rolled reader below wants.
func lines(text string) []string {
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		out = append(out, strings.TrimRight(ln, "\r"))
	}
	return out
}
