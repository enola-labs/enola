// Package grammar vendors the tree-sitter Scala grammar (tree-sitter/tree-sitter-scala).
//
// The upstream Go binding cannot be consumed directly past v0.24.1: v0.25.0 and later
// commit a src/parser.c generated against tree-sitter ABI 15, while the vendored
// go-tree-sitter runtime accepts at most 14. That rejection is SILENT — SetLanguage
// fails, every file parses to nothing, and the result is indistinguishable from a
// repository containing no Scala. v0.24.1 predates the grammar's support for Scala 3
// capture checking (`T^{x}`, `A ->{x} B`, `Box[T, C^]`), and on capture-checked
// sources it drops and mis-nests symbols without reporting a parse error.
//
// So the parser is regenerated from the upstream grammar.json at ABI 14 and committed
// under src/, alongside the hand-written scanner.c copied verbatim. Regenerate with:
//
//	cd grammar && tree-sitter generate grammar.json --abi 14
//
// The generated parser (src/parser.c) and the external scanner (src/scanner.c) are
// compiled as separate translation units via the cgo_*.c shims in this directory, NOT
// #include'd together here: both define grammar-local macros (e.g. TOKEN_COUNT), so
// combining them into one translation unit triggers -Wmacro-redefined.
package grammar

// #cgo CFLAGS: -std=c11 -fPIC -I${SRCDIR}/src
// #include "tree_sitter/parser.h"
// const TSLanguage *tree_sitter_scala(void);
import "C"

import "unsafe"

// Language returns the tree-sitter Language pointer for Scala, suitable for
// sitter.NewLanguage(...).
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_scala())
}
