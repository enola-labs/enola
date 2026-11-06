// Package grammar vendors the tree-sitter Dart grammar (UserNobody14/tree-sitter-dart).
//
// The upstream Go binding cannot be consumed directly: it binds
// github.com/smacker/go-tree-sitter, a DIFFERENT runtime than the
// github.com/tree-sitter/go-tree-sitter enola uses, and their Language handles are
// incompatible. So upstream's committed src/ (the generated ABI 15 parser and the
// hand-written scanner.c) is copied verbatim and bound here instead.
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
// const TSLanguage *tree_sitter_dart(void);
import "C"

import "unsafe"

// Language returns the tree-sitter Language pointer for Dart, suitable for
// sitter.NewLanguage(...).
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_dart())
}
