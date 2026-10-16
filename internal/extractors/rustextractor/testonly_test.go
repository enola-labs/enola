package rustextractor

import (
	"github.com/enola-labs/enola/internal/facts"
)

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// extractFileAST parses a Rust file with tree-sitter and emits architectural
// facts: module-directory-scoped symbols (functions, methods, structs, enums,
// traits, type aliases, consts/statics), dependency facts for `use`
// declarations, RelCalls/RelInstantiates relations, per-function cyclomatic
// complexity, and (Axum) route facts. impl-trait observations are returned
// separately (see implPair) because attaching them requires the full, merged
// fact set.
// extractFileAST is the two-value form used by tests; production code calls
// extractFileASTFull to also receive the file's Axum router-builder observations
// (consumed by composeAxumPrefixes in the crate-wide pass).
func extractFileAST(src []byte, relFile string, crates []crateInfo, moduleDirs map[string]bool) ([]facts.Fact, []implPair) {
	ff, impls, _, _ := extractFileASTFull(src, relFile, crates, moduleDirs)
	return ff, impls
}
