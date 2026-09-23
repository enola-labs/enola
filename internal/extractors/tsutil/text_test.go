package tsutil

import (
	"testing"

	sitter "github.com/tree-sitter/go-tree-sitter"
	rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
)

func TestTextNilNode(t *testing.T) {
	if got := Text(nil, []byte("fn main() {}")); got != "" {
		t.Fatalf("Text(nil) = %q, want empty", got)
	}
}

func TestTextNodeSpan(t *testing.T) {
	src := []byte("fn main() {}")
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(rust.Language())); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()
	fn := tree.RootNode().Child(0)
	if got := Text(fn.ChildByFieldName("name"), src); got != "main" {
		t.Fatalf("Text(name) = %q, want main", got)
	}
	if got := Text(fn.ChildByFieldName("return_type"), src); got != "" {
		t.Fatalf("Text(absent field) = %q, want empty", got)
	}
}
