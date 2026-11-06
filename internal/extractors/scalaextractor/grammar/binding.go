// Package grammar vendors the tree-sitter Scala grammar (tree-sitter/tree-sitter-scala).
//
// The upstream Go binding is not consumed directly although the runtime loads its ABI 15
// parser: that parser reserves Scala 3's soft keywords (`given`, `export`, `enum`,
// `macro`) in every position, so `RestAssured.given()` or a Scala 2 `val enum` becomes an
// ERROR node and the calls around it are lost. ABI 14 cannot carry a reserved-word set.
// Releases up to v0.24.1, the last shipped at ABI 14, predate Scala 3 capture checking
// (`T^{x}`, `A ->{x} B`, `Box[T, C^]`) and drop and mis-nest symbols on it.
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
