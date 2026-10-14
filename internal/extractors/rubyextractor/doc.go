package rubyextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The doc prop: the run of `#` lines ending on the line above a `def`, `class` or
// `module`. A magic comment (`# frozen_string_literal: true`) or a linter directive
// (`# rubocop:disable`) is no description and ends the run, and of a YARD comment the
// text before its first tag is kept.

var rubyDocDirectives = []string{"frozen_string_literal", "rubocop:", "typed:", "-*-", "encoding:", "coding:", "warn_indent", "shareable_constant_value", "@!", ":nodoc:"}

// rubyDoc returns the doc prop for the declaration n, or "".
func rubyDoc(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	// The first statement of a body has no sibling before it: a comment above it is
	// the body's sibling, written before the body begins.
	at := n
	for at.PrevSibling() == nil && at.Parent() != nil && kindOf(at.Parent()) == "body_statement" {
		at = at.Parent()
	}
	var lines []string
	next := n
	for c := at.PrevSibling(); c != nil && kindOf(c) == "comment"; c = c.PrevSibling() {
		if c.EndPosition().Row+1 < next.StartPosition().Row {
			break
		}
		line := strings.TrimSpace(strings.TrimLeft(rubyText(c, src), "#"))
		if isRubyDirective(line) {
			break
		}
		lines = append([]string{line}, lines...)
		next = c
	}
	// YARD: the description ends at the first tag.
	for i, l := range lines {
		if strings.HasPrefix(l, "@") {
			lines = lines[:i]
			break
		}
	}
	return facts.DocSummary(strings.Join(lines, "\n"))
}

func isRubyDirective(line string) bool {
	for _, d := range rubyDocDirectives {
		if strings.HasPrefix(line, d) {
			return true
		}
	}
	return false
}
