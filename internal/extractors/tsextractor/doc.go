package tsextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
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

// indexFiles are the file names that stand for their directory, most preferred first.
var indexFiles = []string{"index.ts", "index.tsx", "index.mts", "index.js", "index.jsx", "index.mjs", "index.cjs"}

// fileDocTags mark a JSDoc block as describing its file or package rather than the
// declaration below it.
var fileDocTags = []string{"@packageDocumentation", "@module", "@fileoverview", "@file", "@overview"}

// packageDocs returns the doc prop of each directory that has one: the file-level
// JSDoc block of its index file. Only the comments that open the file are read, and
// only a block carrying one of fileDocTags counts, since a bare JSDoc block at the
// top of an index file is as likely the first export's.
func packageDocs(tsFiles []string, sources map[string][]byte) map[string]string {
	rank := make(map[string]int) // dir -> rank of the index file read for it
	docs := make(map[string]string)
	for _, rel := range tsFiles {
		base := rel[strings.LastIndexByte(rel, '/')+1:]
		r := -1
		for i, name := range indexFiles {
			if base == name {
				r = i
			}
		}
		if r < 0 {
			continue
		}
		dir := factpath.Dir(rel)
		if prev, seen := rank[dir]; seen && prev <= r {
			continue
		}
		rank[dir] = r
		if doc := leadingFileDoc(string(sources[rel])); doc != "" {
			docs[dir] = doc
		} else {
			delete(docs, dir)
		}
	}
	return docs
}

// leadingFileDoc returns the summary of the first file-level JSDoc block among the
// comments that open src, or "".
func leadingFileDoc(src string) string {
	src = strings.TrimPrefix(src, "\uFEFF")
	if strings.HasPrefix(src, "#!") {
		if nl := strings.IndexByte(src, '\n'); nl >= 0 {
			src = src[nl+1:]
		} else {
			return ""
		}
	}
	for {
		src = strings.TrimLeft(src, " \t\r\n")
		switch {
		case strings.HasPrefix(src, "//"):
			nl := strings.IndexByte(src, '\n')
			if nl < 0 {
				return ""
			}
			src = src[nl+1:]
		case strings.HasPrefix(src, "/*"):
			end := strings.Index(src[2:], "*/")
			if end < 0 {
				return ""
			}
			block := src[:end+4]
			src = src[end+4:]
			if strings.HasPrefix(block, "/**") && hasFileDocTag(block) {
				return facts.DocSummary(fileDocText(block))
			}
		default:
			return ""
		}
	}
}

func hasFileDocTag(block string) bool {
	for _, tag := range fileDocTags {
		rest := block
		for {
			i := strings.Index(rest, tag)
			if i < 0 {
				break
			}
			rest = rest[i+len(tag):]
			if rest == "" || !isTagChar(rest[0]) {
				return true
			}
		}
	}
	return false
}

func isTagChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// fileDocText is a file-level block's description: the text before its first tag, or
// when there is none, the text of its @fileoverview, @file or @overview tag.
func fileDocText(block string) string {
	if desc := strings.TrimSpace(strings.Join(jsDocLines(block), "\n")); desc != "" {
		return desc
	}
	body := strings.TrimSuffix(strings.TrimPrefix(block, "/**"), "*/")
	var out []string
	inTag := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
		if strings.HasPrefix(line, "@") {
			if inTag {
				break
			}
			for _, tag := range []string{"@fileoverview", "@file", "@overview"} {
				if rest, ok := strings.CutPrefix(line, tag); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
					inTag = true
					line = strings.TrimSpace(rest)
					break
				}
			}
			if !inTag {
				continue
			}
		}
		if inTag {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
