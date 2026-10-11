package pythonextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
	python "github.com/tree-sitter/tree-sitter-python/bindings/go"
)

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// buildFileIndex scans src for class declarations and populates idx.
// It does not emit any facts — it is read-only over the AST.
func buildFileIndex(src []byte, relFile string, idx *pySymbolIndex) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(python.Language())); err != nil {
		return
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()
	indexTree(tree.RootNode(), src, relFile, idx)
}
