package scalaextractor

import (
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// extractDSLRoutes runs the Scala-source route and storage passes over one file,
// parsing it itself. The extractor goes through extractFileFacts instead, which
// shares the declaration walk's parse; this entry point remains for tests that
// exercise the DSL passes alone.
func extractDSLRoutes(src []byte, relFile string) []facts.Fact {
	ff, _ := withScalaTree(src, func(root *sitter.Node) ([]facts.Fact, string) {
		return dslFactsFrom(root, src, relFile), ""
	})
	return ff
}

// extractFileAST parses one Scala file with tree-sitter and emits its declaration
// symbols, import dependencies, and the type-reference edges visible from this file
// alone. Edge targets are emitted as fully qualified names (resolved through the
// file's own imports and package), which canonicalizeTargets rewrites to canonical
// "<dir>.<Type>" fact names once every file has been merged.
func extractFileAST(src []byte, relFile string, packageIndex map[string]string) []facts.Fact {
	ff, _ := extractFileASTFull(src, relFile, packageIndex)
	return ff
}

// extractFileASTFull additionally returns the file's declared package, which
// canonicalizeTargets needs to resolve a BARE type reference: only the file knows
// which package a name like `Base` would be in, and only the merged fact set knows
// whether such a type exists. Neither half can decide alone, so the walker records
// the package rather than guessing with it (see resolveTypeName).
func extractFileASTFull(src []byte, relFile string, packageIndex map[string]string) ([]facts.Fact, string) {
	return withScalaTree(src, func(root *sitter.Node) ([]facts.Fact, string) {
		return walkFileAST(root, src, relFile, packageIndex, pekkoHandlerBlocks(root, src, relFile))
	})
}
