package cppextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The doc prop of a C or C++ declaration.
//
// The comment written directly above it, or above the template or declaration that
// holds it: a Doxygen block (`/** */`, `/*! */`), or a run of `///`, `//!` or `//`
// lines, since Google-style code documents with plain `//`. A plain `/* */` block is
// a licence or disabled code, and a kernel-doc block is `/**`. `@brief` and `\brief`
// open the description; any other tag ends it. A comment trailing the code on its
// line (`int x; ///< what`) belongs to that code, and a licence or a tool directive
// is no description.

// cppWrappers hold a declaration and carry its comment above them.
var cppWrappers = map[string]bool{
	"template_declaration": true,
	"declaration":          true,
	"field_declaration":    true,
	"type_definition":      true,
}

// cppDirectives start a line comment that instructs a tool rather than describing
// code.
var cppDirectives = []string{"NOLINT", "clang-format", "cppcheck-suppress", "coverity", "LINT", "IWYU"}

// cppDoc returns the doc prop of the declaration n, or "".
func cppDoc(kinds *tsutil.KindTable, n *sitter.Node, src []byte) string {
	for p := n.Parent(); p != nil && cppWrappers[kindOf(kinds, p)]; p = n.Parent() {
		n = p
	}
	var comments []string
	next := n
	for c := n.PrevSibling(); c != nil && kindOf(kinds, c) == "comment"; c = c.PrevSibling() {
		if c.EndPosition().Row+1 < next.StartPosition().Row {
			break
		}
		if p := c.PrevSibling(); p != nil && kindOf(kinds, p) != "comment" && p.EndPosition().Row == c.StartPosition().Row {
			break // trails the code before it
		}
		comments = append([]string{tsutil.Text(c, src)}, comments...)
		next = c
	}
	doc := facts.DocSummary(cppCommentText(comments))
	if strings.HasPrefix(doc, "Copyright") || strings.Contains(doc, "SPDX-License-Identifier") {
		return ""
	}
	return doc
}

// cppCommentText reduces the comments above a declaration to the description the one
// documenting it gives: the last Doxygen block when there is one, else the run of
// line comments below the last plain block.
func cppCommentText(comments []string) string {
	var lines []string
	for i := len(comments) - 1; i >= 0; i-- {
		c := strings.TrimSpace(comments[i])
		if strings.HasPrefix(c, "/*") {
			if len(lines) == 0 && (strings.HasPrefix(c, "/**") || strings.HasPrefix(c, "/*!")) && !strings.HasPrefix(c, "/***") {
				body := strings.TrimSuffix(c[3:], "*/")
				for _, line := range strings.Split(body, "\n") {
					lines = append(lines, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*")))
				}
			}
			break
		}
		var rest string
		switch {
		case strings.HasPrefix(c, "///<"), strings.HasPrefix(c, "//!<"):
			return ""
		case strings.HasPrefix(c, "///"), strings.HasPrefix(c, "//!"):
			rest = c[3:]
		default:
			rest = c[2:]
		}
		rest = strings.TrimSpace(rest)
		if isCppDirective(rest) {
			break
		}
		lines = append([]string{rest}, lines...)
	}
	return doxygenDescription(lines)
}

// doxygenDescription is the text of lines before their first tag, with a leading
// `@brief` or `\brief` taken as the start of the description.
func doxygenDescription(lines []string) string {
	var out []string
	for _, line := range lines {
		for _, brief := range []string{"@brief", `\brief`} {
			if rest, ok := strings.CutPrefix(line, brief); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
				line = strings.TrimSpace(rest)
			}
		}
		if strings.HasPrefix(line, "@") || strings.HasPrefix(line, `\`) {
			break
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func isCppDirective(line string) bool {
	for _, d := range cppDirectives {
		if strings.HasPrefix(line, d) {
			return true
		}
	}
	return false
}
