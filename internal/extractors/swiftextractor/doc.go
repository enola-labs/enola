package swiftextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The doc prop: what Xcode's Quick Help shows for a declaration.
//
// Its documentation comment, `///` lines or a `/** */` block, written directly
// above it. Attributes are inside the declaration's node, so the comment above them
// is the node's sibling. A plain `//` or `/* */` comment is no doc, and neither is
// a comment trailing the code on its line.

// swiftDoc returns the doc prop of the declaration n, or "".
func swiftDoc(n *sitter.Node, src []byte) string {
	var lines []string
	next := n
	for c := n.PrevSibling(); c != nil; c = c.PrevSibling() {
		if c.EndPosition().Row+1 < next.StartPosition().Row {
			break
		}
		if p := c.PrevSibling(); p != nil && p.EndPosition().Row == c.StartPosition().Row {
			break // trails the code before it
		}
		text := strings.TrimSpace(nodeText(c, src))
		switch kindOf(c) {
		case "comment":
			rest, ok := strings.CutPrefix(text, "///")
			if !ok || strings.HasPrefix(rest, "/") {
				break
			}
			lines = append([]string{strings.TrimSpace(rest)}, lines...)
			next = c
			continue
		case "multiline_comment":
			if len(lines) == 0 && strings.HasPrefix(text, "/**") && !strings.HasPrefix(text, "/***") && text != "/**/" {
				body := strings.TrimSuffix(strings.TrimPrefix(text, "/**"), "*/")
				for _, line := range strings.Split(body, "\n") {
					lines = append(lines, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*")))
				}
			}
		}
		break
	}
	return facts.DocSummary(strings.Join(lines, "\n"))
}
