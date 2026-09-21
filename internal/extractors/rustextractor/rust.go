// Package rustextractor extracts architectural facts from Rust source code
// using tree-sitter AST parsing (see rust_ast.go for the walker implementation
// and resolve.go for crate/import resolution helpers).
package rustextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
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
	for _, relFile := range rustFiles {
		moduleDirs[factpath.Dir(relFile)] = true
	}

	perFileFacts := parallel.MapFiles(ctx, rustFiles, func(relFile string) fileResult {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[rust-extractor] error reading %s: %v", relFile, err)
			return fileResult{}
		}
		ff, impls, builders := extractFileASTFull(src, relFile, crates, moduleDirs)
		return fileResult{facts: ff, impls: impls, builders: builders}
	})

	var allFacts []facts.Fact
	var allImpls []implPair
	var allBuilders []axumBuilder
	for _, r := range perFileFacts {
		allFacts = append(allFacts, r.facts...)
		allImpls = append(allImpls, r.impls...)
		allBuilders = append(allBuilders, r.builders...)
	}

	applyImplements(allFacts, allImpls)
	computeRustPerformsIO(allFacts)
	// Compose `.nest(prefix, module::router())` mount prefixes onto Axum route
	// facts interprocedurally, so a route registered on a sub-router is stored at
	// its true runtime path for cross-repo client↔route matching.
	allFacts = composeAxumPrefixes(allFacts, allBuilders, crates)

	for dir := range moduleDirs {
		props := map[string]any{"language": "rust"}
		if crateDir := nearestCrateDir(dir, crates); crateDir != "" {
			if name, ok := crateNameByDir(crateDir, crates); ok {
				props["crate"] = name
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

// computeRustPerformsIO propagates the direct-I/O signal (io_direct, set by the
// walker on functions making a filesystem/DB/HTTP call) transitively over the
// intra-repo call graph: a function is marked performs_io when it, or anything
// it reaches through RelCalls edges to known symbols, does I/O. This lets the
// performance analyzer recognise an in-loop call to a wrapper that itself calls
// I/O as an N+1. It mirrors the Python extractor's computePyPerformsIO fixpoint.
func computeRustPerformsIO(allFacts []facts.Fact) {
	exists := make(map[string]bool)
	for i := range allFacts {
		if allFacts[i].Kind == facts.KindSymbol {
			exists[allFacts[i].Name] = true
		}
	}

	io := make(map[string]bool)      // name → performs I/O (directly or transitively)
	adj := make(map[string][]string) // name → called names that are known symbols
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if b, _ := f.PropAny("io_direct").(bool); b {
			io[f.Name] = true
		}
		seen := make(map[string]bool)
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls || r.Target == f.Name || seen[r.Target] || !exists[r.Target] {
				continue
			}
			seen[r.Target] = true
			adj[f.Name] = append(adj[f.Name], r.Target)
		}
	}

	for changed := true; changed; {
		changed = false
		for name, callees := range adj {
			if io[name] {
				continue
			}
			for _, c := range callees {
				if io[c] {
					io[name] = true
					changed = true
					break
				}
			}
		}
	}

	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindSymbol && io[f.Name] {
			if f.Props == nil {
				f.Props = map[string]any{}
			}
			f.SetProp("performs_io", true)
		}
	}
}

// fileResult holds one file's extracted facts plus its impl-block
// observations, returned together from the parallel per-file walk.
type fileResult struct {
	facts    []facts.Fact
	impls    []implPair
	builders []axumBuilder
}
