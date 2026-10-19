// Package grammar vendors the tree-sitter C# grammar (tree-sitter/tree-sitter-c-sharp).
//
// The upstream Go binding cannot be consumed directly past v0.23.1: v0.23.2 and later
// commit a src/parser.c generated against tree-sitter ABI 15, while the vendored
// go-tree-sitter runtime accepts at most 14. That rejection is SILENT: SetLanguage
// fails, every file parses to nothing, and the result is indistinguishable from a
// repository containing no C#. v0.23.1 predates C# 12: a body-less type
// (`class X : Y;`) or a collection expression (`= []`) turns the whole file into one
// ERROR node, and the file produces no symbols at all.
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
// const TSLanguage *tree_sitter_c_sharp(void);
import "C"

import "unsafe"

// Language returns the tree-sitter Language pointer for C#, suitable for
// sitter.NewLanguage(...).
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_c_sharp())
}
