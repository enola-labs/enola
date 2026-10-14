package tsextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The doc prop: the comment written on a declaration.
//
// A JSDoc block (`/** ... */`) or a run of `//` lines, ending on the line before the
// declaration, or before the `export` that holds it. A comment separated from the
// declaration by a blank line is a file or section comment, and a tool directive
// (`// eslint-disable-next-line`, `// @ts-ignore`) is no description. Of a JSDoc
// block, the text before its first tag is kept.

// tsDocDirectives start a comment that instructs a tool rather than describing code.
var tsDocDirectives = []string{"eslint", "@ts-", "prettier-ignore", "istanbul", "tslint", "#region", "#endregion", "biome-ignore"}

// tsDoc returns the doc prop for the declaration n, or "".
func tsDoc(kinds *tsutil.KindTable, n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	// The comment sits above the outermost node the declaration is written in:
	// `export const f = () => {}` is commented above `export`.
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch kindOf(kinds, p) {
		case "variable_declarator", "lexical_declaration", "variable_declaration", "export_statement":
			n = p
			continue
		}
		break
	}
	// Decorators written above a member are its siblings: `/** doc */ @Get() find()`.
	// The comment is above them.
	start := n.PrevSibling()
	next := n
	for start != nil && kindOf(kinds, start) == "decorator" {
		next = start
		start = start.PrevSibling()
	}
	var lines []string
	for c := start; c != nil && kindOf(kinds, c) == "comment"; c = c.PrevSibling() {
		if c.EndPosition().Row+1 < next.StartPosition().Row {
			break // a blank line between: not this declaration's comment
		}
		text := nodeText(c, src)
		if strings.HasPrefix(text, "/*") && !strings.HasPrefix(text, "/**") {
			break // a plain block comment, usually a licence or commented-out code
		}
		if strings.HasPrefix(text, "/**") {
			lines = append(jsDocLines(text), lines...)
			break
		}
		line := strings.TrimSpace(strings.TrimPrefix(text, "//"))
		if isDirective(line) {
			break
		}
		lines = append([]string{line}, lines...)
		next = c
	}
	return facts.DocSummary(strings.Join(lines, "\n"))
}

// jsDocLines returns the lines of a JSDoc block's description: without the comment
// markers and the leading `*` of each line, and up to its first tag.
func jsDocLines(text string) []string {
	text = strings.TrimSuffix(strings.TrimPrefix(text, "/**"), "*/")
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(line, "*"))
		if strings.HasPrefix(line, "@") {
			break
		}
		out = append(out, line)
	}
	return out
}

func isDirective(line string) bool {
	for _, d := range tsDocDirectives {
		if strings.HasPrefix(line, d) {
			return true
		}
	}
	return false
}
