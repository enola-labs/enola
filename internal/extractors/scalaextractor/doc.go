package scalaextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// scalaDirectives start a line comment that instructs a tool rather than
// describing code.
var scalaDirectives = []string{"scalastyle:", "scalafmt:", "format:", "noinspection", "$COVERAGE-", "nowarn"}

// scalaDoc returns the doc prop of a definition: its Scaladoc up to the first tag,
// or the run of `//` lines above it. Annotations are part of the definition's node,
// so the comment above them is the node's sibling. An enum case's comment is above
// the case list that holds it.
func scalaDoc(n *sitter.Node, src []byte) string {
	comments := commentsAbove(n, n.PrevSibling(), src)
	if len(comments) == 0 {
		comments = trailingComments(n, src)
	}
	if len(comments) == 0 && n.PrevNamedSibling() == nil {
		if p := n.Parent(); p != nil && kindOf(p) == "enum_case_definitions" {
			comments = commentsAbove(p, p.PrevSibling(), src)
		}
	}
	return facts.DocSummary(tsutil.DocCommentText(comments, []string{"//"}, func(line string) bool {
		for _, d := range scalaDirectives {
			if strings.HasPrefix(line, d) {
				return true
			}
		}
		return false
	}))
}

func isScalaComment(n *sitter.Node) bool {
	k := kindOf(n)
	return k == "comment" || k == "block_comment"
}

// commentsAbove returns the run of comments ending directly above n, top first,
// reading back from c. A comment on the same line as the code before it trails that
// code and ends the run: `val a = 1 // why` says nothing about the next member.
func commentsAbove(n, c *sitter.Node, src []byte) []string {
	var out []string
	next := n
	for ; c != nil && isScalaComment(c); c = c.PrevSibling() {
		if c.EndPosition().Row+1 < next.StartPosition().Row {
			break
		}
		if p := c.PrevSibling(); p != nil && !isScalaComment(p) && p.EndPosition().Row == c.StartPosition().Row {
			break
		}
		out = append([]string{tsutil.Text(c, src)}, out...)
		next = c
	}
	return out
}

// trailingComments returns the comments directly above n that the grammar put at
// the end of the definition before it. A def whose body is a bare expression
// (`def f(x: Int) = g(x)`) runs on to the next member, and takes the comments
// between them into its subtree.
func trailingComments(n *sitter.Node, src []byte) []string {
	prev := n.PrevSibling()
	for prev != nil && prev.ChildCount() > 0 {
		last := prev.Child(prev.ChildCount() - 1)
		if isScalaComment(last) {
			return commentsAbove(n, last, src)
		}
		prev = last
	}
	return nil
}
