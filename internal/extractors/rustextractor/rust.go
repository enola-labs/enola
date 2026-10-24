// Package rustextractor extracts architectural facts from Rust source code
// using tree-sitter AST parsing (see rust_ast.go for the walker implementation
// and resolve.go for crate/import resolution helpers).
package rustextractor

import (
	"context"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/extractors/ioclosure"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
)

// RustExtractor extracts architectural facts from Rust source code.
type RustExtractor struct{}

// New creates a new RustExtractor.
func New() *RustExtractor {
	return &RustExtractor{}
}

func (e *RustExtractor) Name() string {
	return "rust"
}

// Detect returns true if the repository looks like a Rust project (a Cargo
// workspace or a single crate). It checks the repo root first, then walks up
// to 3 subdirectory levels to support monorepos where a Rust crate lives in a
// subdirectory, mirroring the Python extractor's monorepo detection.
func (e *RustExtractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector.
//
// A Cargo.toml at any depth detects, replacing the old three-level scan. The .rs
// fallback is the other half, and it is not hypothetical: the Linux kernel carries
// 473 .rs files and NOT ONE Cargo.toml, so a manifest-only rule leaves the whole of
// rust/ unindexed however deep it is willing to look. The extension is unambiguous —
// nothing else spells .rs — which is what makes the fallback safe here and not for
// the JVM languages, where a build file is shared between Java, Kotlin and Scala.
func (e *RustExtractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if detectnames.HasSegment(rel, "target") {
			continue
		}
		if detectnames.Base(rel) == "Cargo.toml" || strings.HasSuffix(rel, ".rs") {
			return true, nil
		}
	}
	return false, nil
}

// isRustFile returns true if the file has a .rs extension.
func isRustFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".rs")
}

// OwnsFile implements plugin.FileOwner for incremental caching.
func (e *RustExtractor) OwnsFile(relFile string) bool { return isRustFile(relFile) }

// Extract parses Rust files and emits architectural facts.
//
// Every Cargo.toml in the repo is scanned first (cheap: no tree-sitter parse
// needed) to build a crate-name -> crate-directory index and the set of known
// module directories, both computed up front so cross-file import resolution
// (which crate/submodule a `use` path refers to) can happen synchronously
// during each file's walk rather than needing a second pass. `impl Trait for
// Type` is the one thing that genuinely needs a post-pass: the impl block is
// frequently declared in a different file than Type itself, so each file's
// walker only records the observation (astWalker.impls) and applyImplements
// attaches the resulting RelImplements edge to Type's own symbol fact once
// every file has been merged.
func (e *RustExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var rustFiles, cargoFiles []string
	for _, relFile := range files {
		switch {
		case isRustFile(relFile):
			rustFiles = append(rustFiles, relFile)
		case filepath.Base(relFile) == "Cargo.toml":
			cargoFiles = append(cargoFiles, relFile)
		}
	}

	crates := buildCrateIndex(repoPath, cargoFiles)

	moduleDirs := make(map[string]bool, len(rustFiles))
	moduleFiles := make(map[string]bool, len(rustFiles))
	for _, relFile := range rustFiles {
		moduleDirs[factpath.Dir(relFile)] = true
		moduleFiles[factpath.Slash(relFile)] = true
	}

	perFileFacts := parallel.MapFiles(ctx, rustFiles, func(relFile string) rustFileOut {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[rust-extractor] error reading %s: %v", relFile, err)
			return rustFileOut{}
		}
		return extractRustFile(src, relFile, crates, moduleDirs, false)
	})

	// A file a parent declares as `#[cfg(test)] mod name;` is test code, though
	// nothing in its own path says so: walk it again in test mode, replacing what
	// the production walk emitted. Its own body-less modules are test code in turn.
	byFile := make(map[string]int, len(rustFiles))
	for i, f := range rustFiles {
		byFile[factpath.Slash(f)] = i
	}
	isTestFile := make(map[int]bool)
	pending := make([]int, 0)
	queue := func(parent int, names []string) {
		for _, name := range names {
			if i, ok := submoduleFile(rustFiles[parent], name, byFile); ok && !isTestFile[i] {
				isTestFile[i] = true
				pending = append(pending, i)
			}
		}
	}
	for i := range perFileFacts {
		queue(i, perFileFacts[i].testSubmods)
	}
	for len(pending) > 0 {
		i := pending[0]
		pending = pending[1:]
		src, err := os.ReadFile(filepath.Join(repoPath, rustFiles[i]))
		if err != nil {
			continue
		}
		r := extractRustFile(src, rustFiles[i], crates, moduleDirs, true)
		perFileFacts[i] = rustFileOut{facts: r.facts}
		queue(i, r.testSubmods)
	}

	var allFacts []facts.Fact
	var allImpls []implPair
	var allBuilders []axumBuilder
	fileDocs := make(map[string]string)
	for i, r := range perFileFacts {
		if r.doc != "" {
			fileDocs[rustFiles[i]] = r.doc
		}
		allFacts = append(allFacts, r.facts...)
		allImpls = append(allImpls, r.impls...)
		allBuilders = append(allBuilders, r.builders...)
	}

	applyImplements(allFacts, allImpls)
	resolveTypedCalls(allFacts)
	ioclosure.Propagate(allFacts)
	// Compose `.nest(prefix, module::router())` mount prefixes onto Axum route
	// facts interprocedurally, so a route registered on a sub-router is stored at
	// its true runtime path for cross-repo client↔route matching.
	allFacts = composeAxumPrefixes(allFacts, allBuilders, crates)
	bindRustRouteHandlers(allFacts, crates)

	// A crate is published when crates.io would take it and it has a library target
	// for other crates to depend on; a binary-only crate is installed, not imported.
	libCrates := map[string]bool{}
	for _, c := range crates {
		if c.publishable && moduleFiles[path.Join(c.dir, "src", "lib.rs")] {
			libCrates[c.dir] = true
		}
	}

	docs := moduleDocs(moduleDirs, fileDocs)
	for dir := range moduleDirs {
		props := map[string]any{"language": "rust"}
		if doc := docs[dir]; doc != "" {
			facts.SetDocProp(props, doc)
		}
		if crateDir := nearestCrateDir(dir, crates); crateDir != "" {
			if name, ok := crateNameByDir(crateDir, crates); ok {
				props["crate"] = name
			}
			if libCrates[crateDir] {
				props[facts.PropPublished] = true
			}
		}
		allFacts = append(allFacts, facts.Fact{
			Kind:  facts.KindModule,
			Name:  dir,
			File:  dir,
			Props: props,
		})
	}

	return allFacts, nil
}

// submoduleFile resolves a body-less `mod name;` declared in parent to its file,
// name.rs or name/mod.rs, in the directory Rust looks in: parent's own directory
// for a lib.rs, main.rs or mod.rs, else the directory named after parent's stem.
func submoduleFile(parent, name string, byFile map[string]int) (int, bool) {
	parent = factpath.Slash(parent)
	dir := path.Dir(parent)
	switch base := path.Base(parent); base {
	case "lib.rs", "main.rs", "mod.rs":
	default:
		dir = path.Join(dir, strings.TrimSuffix(base, ".rs"))
	}
	for _, cand := range []string{path.Join(dir, name+".rs"), path.Join(dir, name, "mod.rs")} {
		if i, ok := byFile[cand]; ok {
			return i, true
		}
	}
	return 0, false
}
