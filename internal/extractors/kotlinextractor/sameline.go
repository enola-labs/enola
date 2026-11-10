package kotlinextractor

import (
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// The grammar needs a newline or `;` between a member and the `}` closing its body
// when anything follows that body: in
//
//	class Helper { fun work() {} }
//	class Repo { fun save() {} }
//
// it reads `fun work() {} }` as unfinished and the rest of the file becomes one
// ERROR node, so every declaration in the file is lost. A file that does not parse
// is parsed again with a `;` in place of the space before each such `}`. The
// replacement keeps every byte offset, so the walk reads names, spans and hashes from
// the original source; the second tree is kept only when it has fewer errors.

// reparseSameLineCloses returns the better of tree and a parse of src with the
// separators written in. The tree not returned is closed.
func reparseSameLineCloses(parser *sitter.Parser, src []byte, tree *sitter.Tree) *sitter.Tree {
	fixed, changed := separateSameLineCloses(src)
	if !changed {
		return tree
	}
	again := parser.Parse(fixed, nil)
	if again == nil {
		return tree
	}
	if errorNodes(again.RootNode()) < errorNodes(tree.RootNode()) {
		tree.Close()
		return again
	}
	again.Close()
	return tree
}

// errorNodes counts the error and missing nodes under n.
func errorNodes(n *sitter.Node) int {
	if !n.HasError() {
		return 0
	}
	count := 0
	if n.IsError() || n.IsMissing() {
		count++
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		count += errorNodes(n.Child(i))
	}
	return count
}

// separateSameLineCloses writes `;` over the blank before a `}` that closes a body
// after code on the same line (`{} }`, `= 1 }`), outside strings and comments. A `}`
// after `{`, `;`, `,` or `->` needs no separator and is left alone.
func separateSameLineCloses(src []byte) ([]byte, bool) {
	out := make([]byte, len(src))
	copy(out, src)
	changed := false
	for i := 0; i < len(src); i++ {
		switch {
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && (src[i] != '*' || src[i+1] != '/') {
				i++
			}
			i++
		case src[i] == '"' && i+2 < len(src) && src[i+1] == '"' && src[i+2] == '"':
			i += 3
			for i+2 < len(src) && (src[i] != '"' || src[i+1] != '"' || src[i+2] != '"') {
				i++
			}
			i += 2
		case src[i] == '"' || src[i] == '\'':
			q := src[i]
			for i++; i < len(src) && src[i] != q && src[i] != '\n'; i++ {
				if src[i] == '\\' {
					i++
				}
			}
		case src[i] == '}' && i > 0 && (src[i-1] == ' ' || src[i-1] == '\t'):
			j := i - 1
			for j >= 0 && (src[j] == ' ' || src[j] == '\t') {
				j--
			}
			if j < 0 || src[j] == '\n' || src[j] == '\r' || src[j] == '{' || src[j] == ';' || src[j] == ',' ||
				(src[j] == '>' && j > 0 && src[j-1] == '-') {
				continue
			}
			out[i-1] = ';'
			changed = true
		}
	}
	return out, changed
}
