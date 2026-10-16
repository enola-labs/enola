package javaextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	java "github.com/tree-sitter/tree-sitter-java/bindings/go"
)

// javaDoc returns the doc prop of a declaration: its Javadoc up to the first tag, or
// the run of `//` lines above it. Annotations are part of the declaration's node, so
// the comment above them is the node's sibling.
func javaDoc(n *sitter.Node, src []byte) string {
	comments := tsutil.PrecedingComments(n, src, kindOf, func(k string) bool { return k == "line_comment" || k == "block_comment" })
	return facts.DocText(tsutil.DocCommentText(comments, []string{"//"}, func(line string) bool {
		return strings.HasPrefix(line, "noinspection") || strings.HasPrefix(line, "NOSONAR") || strings.HasPrefix(line, "CHECKSTYLE")
	}))
}

// javaPackageDoc returns the doc prop of a package from its package-info.java: the
// Javadoc above the package declaration, or above the annotations on it. A licence
// written there instead is no description.
func javaPackageDoc(src []byte) string {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(java.Language())); err != nil {
		return ""
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return ""
	}
	defer tree.Close()
	root := tree.RootNode()
	for i := uint(0); i < root.NamedChildCount(); i++ {
		n := root.NamedChild(i)
		if kindOf(n) != "package_declaration" {
			continue
		}
		doc := javaDoc(n, src)
		if first := facts.DocSummary(doc); strings.HasPrefix(first, "Copyright") || strings.Contains(first, "SPDX-License-Identifier") || strings.Contains(first, "Licensed under") {
			return ""
		}
		return doc
	}
	return ""
}

// isPackageInfo reports whether relFile is a package-info.java.
func isPackageInfo(relFile string) bool {
	return relFile == "package-info.java" || strings.HasSuffix(relFile, "/package-info.java")
}
