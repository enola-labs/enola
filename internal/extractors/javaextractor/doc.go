package javaextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// javaDoc returns the doc prop of a declaration: its Javadoc up to the first tag, or
// the run of `//` lines above it. Annotations are part of the declaration's node, so
// the comment above them is the node's sibling.
func javaDoc(n *sitter.Node, src []byte) string {
	comments := tsutil.PrecedingComments(n, src, kindOf, func(k string) bool { return k == "line_comment" || k == "block_comment" })
	return facts.DocSummary(tsutil.DocCommentText(comments, []string{"//"}, func(line string) bool {
		return strings.HasPrefix(line, "noinspection") || strings.HasPrefix(line, "NOSONAR") || strings.HasPrefix(line, "CHECKSTYLE")
	}))
}
