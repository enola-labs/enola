package tsutil

import (
	"regexp"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Doc comments, for the extractors whose grammars put a declaration's comment
// beside it as a sibling node.

// PrecedingComments returns the comments written directly above n, top first: the
// run of sibling nodes whose kind isComment accepts, each ending on the line before
// the next. A blank line ends the run, since a comment separated from a declaration
// by one is about something else.
func PrecedingComments(n *sitter.Node, src []byte, kindOf func(*sitter.Node) string, isComment func(string) bool) []string {
	if n == nil {
		return nil
	}
	var out []string
	next := n
	for c := n.PrevSibling(); c != nil && isComment(kindOf(c)); c = c.PrevSibling() {
		if c.EndPosition().Row+1 < next.StartPosition().Row {
			break
		}
		out = append([]string{Text(c, src)}, out...)
		next = c
	}
	return out
}

// DocBlockLines returns the description of a `/** ... */` block: its lines without
// the comment markers and each line's leading `*`, up to the first tag (`@param`).
// Javadoc, JSDoc and PHPDoc share the form.
func DocBlockLines(text string) []string {
	text = strings.TrimSuffix(strings.TrimPrefix(text, "/**"), "*/")
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
		if strings.HasPrefix(line, "@") {
			break
		}
		out = append(out, line)
	}
	return out
}

// DocCommentText reduces the comments above a declaration to the text of the one
// that documents it: the last `/** */` block when there is one, else the run of line
// comments, each stripped of its markers by lineMarkers. A plain `/* */` block is a
// licence or disabled code and documents nothing. directive reports a line comment
// that instructs a tool rather than describing code; it ends the run above it.
func DocCommentText(comments []string, lineMarkers []string, directive func(string) bool) string {
	var lines []string
	for i := len(comments) - 1; i >= 0; i-- {
		c := strings.TrimSpace(comments[i])
		switch {
		case strings.HasPrefix(c, "/**"):
			if len(lines) > 0 {
				return strings.Join(lines, "\n")
			}
			return strings.Join(DocBlockLines(c), "\n")
		case strings.HasPrefix(c, "/*"):
			return strings.Join(lines, "\n")
		}
		stripped := ""
		for _, m := range lineMarkers {
			if rest, ok := strings.CutPrefix(c, m); ok {
				stripped = strings.TrimSpace(rest)
				break
			}
		}
		if directive != nil && directive(stripped) {
			break
		}
		lines = append([]string{stripped}, lines...)
	}
	return strings.Join(lines, "\n")
}

var (
	xmlSummary = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
	xmlRef     = regexp.MustCompile(`<(?:see|seealso|paramref|typeparamref)\s+(?:cref|name|langword)="([^"]*)"\s*/>`)
	xmlTag     = regexp.MustCompile(`<[^>]+>`)
)

// XMLDocSummary returns the text of a C# XML doc comment's <summary>, or of the
// whole comment when it has none, without its tags. `<see cref="Foo"/>` keeps its
// target's name, which is what a reader of the sentence needs.
func XMLDocSummary(text string) string {
	if m := xmlSummary.FindStringSubmatch(text); m != nil {
		text = m[1]
	}
	text = xmlRef.ReplaceAllString(text, "$1")
	return xmlTag.ReplaceAllString(text, "")
}
