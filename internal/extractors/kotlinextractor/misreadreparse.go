package kotlinextractor

import (
	"regexp"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	kotlin "github.com/tree-sitter-grammars/tree-sitter-kotlin/bindings/go"
)

// After an expression-bodied function, the grammar can read the next annotated
// declaration as a statement:
//
//	private fun style(): Style = when { … }
//
//	@PreviewLightDark
//	@Composable
//	private fun DialogPreview() = PreviewDialog(sample())
//
// comes out as a top-level assignment, with no error node, so the declaration is
// lost without a trace. On its own the same text parses as the declaration it is.
// So the node's range is parsed again with everything else in the file blanked:
// spaces for every byte outside it, newlines kept, so offsets and lines are the
// file's own and the walk reads the original source.

// kotlinDeclKeyword spots a declaration keyword in a misread statement.
var kotlinDeclKeyword = regexp.MustCompile(`(?m)(^|\s)(fun|class|interface|object|val|var)\s`)

// recoverMisreadDeclaration walks the declarations a top-level statement holds
// when the statement's text, parsed alone, is declarations, and reports whether
// it found any.
func (w *astWalker) recoverMisreadDeclaration(node *sitter.Node) bool {
	start, end := node.StartByte(), node.EndByte()
	if !kotlinDeclKeyword.Match(w.src[start:end]) {
		return false
	}
	masked := make([]byte, len(w.src))
	for i, b := range w.src {
		switch {
		case uint(i) >= start && uint(i) < end, b == '\n':
			masked[i] = b
		default:
			masked[i] = ' '
		}
	}
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(kotlin.Language())); err != nil {
		return false
	}
	tree := parser.Parse(masked, nil)
	if tree == nil {
		return false
	}
	defer tree.Close()
	root := tree.RootNode()
	found := false
	for i := uint(0); i < root.ChildCount(); i++ {
		switch child := root.Child(i); kindOf(child) {
		case "class_declaration":
			w.handleClassDeclaration(child)
			found = true
		case "object_declaration":
			w.handleObjectDeclaration(child)
			found = true
		case "function_declaration":
			w.handleFunctionDeclaration(child)
			found = true
		case "property_declaration":
			w.handlePropertyDeclaration(child)
			found = true
		}
	}
	return found
}
