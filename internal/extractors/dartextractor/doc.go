package dartextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The doc prop: what dartdoc renders for a declaration.
//
// Its documentation comment, `///` lines or a `/** */` block, written directly above
// it or above the annotations on it; the grammar names both documentation_comment. A
// plain `//` comment is no doc, and neither is a comment trailing the code on its
// line.

// dartDoc returns the doc prop of the declaration n, or "".
func dartDoc(n *sitter.Node, src []byte) string {
	var lines []string
	next := n
	for c := n.PrevSibling(); c != nil; c = c.PrevSibling() {
		if c.EndPosition().Row+1 < next.StartPosition().Row {
			break
		}
		if kindOf(c) == "annotation" {
			next = c
			continue
		}
		if kindOf(c) != "documentation_comment" {
			break
		}
		if p := c.PrevSibling(); p != nil && p.EndPosition().Row == c.StartPosition().Row {
			break // trails the code before it
		}
		text := strings.TrimSpace(c.Utf8Text(src))
		if rest, ok := strings.CutPrefix(text, "///"); ok {
			if strings.HasPrefix(rest, "/") {
				break
			}
			lines = append([]string{strings.TrimSpace(rest)}, lines...)
			next = c
			continue
		}
		if len(lines) == 0 && strings.HasPrefix(text, "/**") {
			body := strings.TrimSuffix(strings.TrimPrefix(text, "/**"), "*/")
			for _, line := range strings.Split(body, "\n") {
				lines = append(lines, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*")))
			}
		}
		break
	}
	return facts.DocText(strings.Join(lines, "\n"))
}

// setDoc sets the doc prop of the declaration n on the fact just appended for it.
func (w *walker) setDoc(n *sitter.Node) {
	if doc := dartDoc(n, w.src); doc != "" {
		facts.SetDoc(&w.out[len(w.out)-1], doc)
	}
}
