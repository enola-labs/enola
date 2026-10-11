package kotlinextractor

import (
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// extractFileAST parses a Kotlin file using tree-sitter and emits architectural facts.
// Output is intended to be a superset of the legacy regex extractor's output: every
// declaration / import / Room-storage fact is preserved, and new RelInstantiates /
// RelInjects relations are attached to symbol facts when call sites or @Inject
// constructor parameters are observed.
func extractFileAST(src []byte, relFile string, isAndroid bool, sourceRoot, basePackage string, packageIndex map[string]string) []facts.Fact {
	var out []facts.Fact
	withTree(src, relFile, isAndroid, sourceRoot, basePackage, packageIndex, func(_ *sitter.Node, walked []facts.Fact) {
		out = walked
	})
	return out
}
