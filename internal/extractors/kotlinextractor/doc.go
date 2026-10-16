package kotlinextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// kotlinDirectives start a line comment that instructs a tool rather than
// describing code.
var kotlinDirectives = []string{"noinspection", "ktlint-", "detekt", "region", "endregion", "language="}

// kotlinDoc returns the doc prop of a declaration: its KDoc up to the first tag, or
// the run of `//` lines above it. Annotations are part of the declaration's node, so
// the comment above them is the node's sibling.
func kotlinDoc(n *sitter.Node, src []byte) string {
	comments := tsutil.PrecedingComments(n, src, kindOf, func(k string) bool { return k == "line_comment" || k == "block_comment" })
	return facts.DocText(tsutil.DocCommentText(comments, []string{"//"}, func(line string) bool {
		for _, d := range kotlinDirectives {
			if strings.HasPrefix(line, d) {
				return true
			}
		}
		return false
	}))
}
