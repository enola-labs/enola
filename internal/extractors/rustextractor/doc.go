package rustextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The doc prop: what rustdoc renders for an item or a module.
//
// An item's doc is its outer doc comments, `///` lines or a `/** */` block, written
// directly above it or above the attributes on it. A plain `//` or `/* */` comment,
// and a `////` or `/***` one, is no doc. A module's doc is the inner doc comments,
// `//!` lines or a `/*! */` block, opening its file.

// rustDoc returns the doc prop of the item n, or "".
func rustDoc(n *sitter.Node, src []byte) string {
	var lines []string
	next := n
	for c := n.PrevSibling(); c != nil; c = c.PrevSibling() {
		if endRow(c)+1 < next.StartPosition().Row {
			break // a blank line between: about something else
		}
		text := strings.TrimSpace(nodeText(c, src))
		switch kindOf(c) {
		case "attribute_item":
			next = c
			continue
		case "line_comment":
			if rest, ok := outerLineDoc(text); ok {
				lines = append([]string{rest}, lines...)
				next = c
				continue
			}
			// `#[allow(dead_code)] // why`: a comment trailing an attribute on its line.
			if p := c.PrevSibling(); p != nil && kindOf(p) == "attribute_item" && endRow(p) == c.StartPosition().Row {
				next = c
				continue
			}
		case "block_comment":
			if len(lines) == 0 && isDocBlock(text, "/**") {
				lines = blockDocLines(text)
			}
		}
		break
	}
	return facts.DocSummary(strings.Join(lines, "\n"))
}

// rustFileDoc returns the doc prop of the module a file opens: its leading inner doc
// comments. Plain comments and inner attributes (`#![...]`) may come between them.
func rustFileDoc(root *sitter.Node, src []byte) string {
	var lines []string
	for i := uint(0); i < root.ChildCount(); i++ {
		c := root.Child(i)
		text := strings.TrimSpace(nodeText(c, src))
		switch kindOf(c) {
		case "inner_attribute_item":
			continue
		case "line_comment":
			if rest, ok := strings.CutPrefix(text, "//!"); ok {
				lines = append(lines, strings.TrimSpace(rest))
			}
			continue
		case "block_comment":
			if isDocBlock(text, "/*!") {
				lines = append(lines, blockDocLines(text)...)
			}
			continue
		}
		break
	}
	return facts.DocSummary(strings.Join(lines, "\n"))
}

// outerLineDoc returns the text of a `///` comment, and false for any other.
func outerLineDoc(text string) (string, bool) {
	rest, ok := strings.CutPrefix(text, "///")
	if !ok || strings.HasPrefix(rest, "/") {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// isDocBlock reports whether a block comment opens with marker ("/**" or "/*!") and
// is not `/***` or the empty `/**/`.
func isDocBlock(text, marker string) bool {
	if !strings.HasPrefix(text, marker) || text == "/**/" {
		return false
	}
	return !strings.HasPrefix(text[len(marker):], "*")
}

// blockDocLines returns a doc block's lines without its markers and each line's
// leading `*`.
func blockDocLines(text string) []string {
	text = strings.TrimSuffix(text[3:], "*/")
	var out []string
	for _, line := range strings.Split(text, "\n") {
		out = append(out, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*")))
	}
	return out
}

// endRow is the last row a node's text is on. A line comment's node takes in its
// newline, and so ends at column 0 of the next row.
func endRow(n *sitter.Node) uint {
	end := n.EndPosition()
	if end.Column == 0 && end.Row > n.StartPosition().Row {
		return end.Row - 1
	}
	return end.Row
}

// moduleDocFiles are the files whose inner doc describes their directory's module,
// most preferred first.
var moduleDocFiles = []string{"lib.rs", "main.rs", "mod.rs"}

// moduleDocs returns the doc prop of each module directory: the inner doc of its
// lib.rs, main.rs or mod.rs, else of the file beside it that declares it (`foo.rs`
// for `foo/`). fileDocs maps a file to its inner doc.
func moduleDocs(moduleDirs map[string]bool, fileDocs map[string]string) map[string]string {
	out := make(map[string]string)
	for dir := range moduleDirs {
		var candidates []string
		for _, name := range moduleDocFiles {
			if dir == "." {
				candidates = append(candidates, name)
			} else {
				candidates = append(candidates, dir+"/"+name)
			}
		}
		if dir != "." {
			candidates = append(candidates, dir+".rs")
		}
		for _, f := range candidates {
			if doc := fileDocs[f]; doc != "" {
				out[dir] = doc
				break
			}
		}
	}
	return out
}
