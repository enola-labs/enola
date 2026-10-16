package phpextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// phpDoc returns the doc prop of a declaration: its docblock up to the first tag, or
// the run of `//` or `#` lines above it. PHP 8 attributes are part of the
// declaration's node, so the comment above them is its sibling.
func phpDoc(n *sitter.Node, src []byte) string {
	comments := tsutil.PrecedingComments(n, src, kindOf, func(k string) bool { return k == "comment" })
	return facts.DocText(tsutil.DocCommentText(comments, []string{"//", "#"}, func(line string) bool {
		return strings.HasPrefix(line, "phpcs:") || strings.HasPrefix(line, "@phpstan") || strings.HasPrefix(line, "@psalm") || strings.HasPrefix(line, "codeCoverageIgnore")
	}))
}
