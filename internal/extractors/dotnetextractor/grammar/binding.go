// Package grammar vendors the tree-sitter C# grammar (tree-sitter/tree-sitter-c-sharp)
// at master commit 8c0abe0, with one change to grammar.js: upstream's `*` accepts only
// an lvalue, so `*(T*)p`, `*++p` and `*p++` are syntax errors, and under error recovery
// one of them can turn the whole enclosing class into an ERROR node. pointer-indirection.patch
// is the change; ATTRIBUTION.md records how src/ was regenerated from it.
//
// Go's build cache does not track the src/ files the cgo_*.c shims include: after
// replacing them, build with -a, or the old parser stays linked.
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
