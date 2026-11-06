package extractors_test

import (
	"testing"
	"unsafe"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	kotlin "github.com/tree-sitter-grammars/tree-sitter-kotlin/bindings/go"
	c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	php "github.com/tree-sitter/tree-sitter-php/bindings/go"
	python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	dart "github.com/enola-labs/enola/internal/extractors/dartextractor/grammar"
	scala "github.com/enola-labs/enola/internal/extractors/scalaextractor/grammar"
	swift "github.com/enola-labs/enola/internal/extractors/swiftextractor/grammar"
	csharp "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
)

// TestGrammarABI_EveryGrammarLoadsAtItsPinnedABI loads every grammar enola parses with
// into the runtime. SetLanguage rejects an ABI outside the runtime's range, and an
// extractor that ignores that error parses nothing and emits no facts, so a grammar
// bump that crosses the range fails here instead of silently on a corpus. The pinned
// ABI per grammar records which generator produced each parser, so a regenerated
// vendored parser or a module bump that changes it is a visible, reviewed change.
func TestGrammarABI_EveryGrammarLoadsAtItsPinnedABI(t *testing.T) {
	grammars := []struct {
		name string
		lang unsafe.Pointer
		abi  uint32
		src  string
	}{
		{"c", c.Language(), 15, "int main(void) { return 0; }\n"},
		{"cpp", cpp.Language(), 14, "class A { int f(); };\n"},
		{"java", java.Language(), 14, "class A { void f() {} }\n"},
		{"kotlin", kotlin.Language(), 14, "class A {\n    fun f() {}\n}\n"},
		{"php", php.LanguagePHP(), 14, "<?php class A { function f() {} }\n"},
		{"php-only", php.LanguagePHPOnly(), 14, "class A { function f() {} }\n"},
		{"python", python.Language(), 15, "def f():\n    return 1\n"},
		{"ruby", ruby.Language(), 14, "class A\n  def f; end\nend\n"},
		{"rust", rust.Language(), 15, "fn main() {}\n"},
		{"typescript", typescript.LanguageTypescript(), 14, "class A { f(): void {} }\n"},
		{"tsx", typescript.LanguageTSX(), 14, "const a = <div />;\n"},
		{"csharp", csharp.Language(), 15, "class A { void F() {} }\n"},
		{"dart", dart.Language(), 15, "class A { void f() {} }\n"},
		{"scala", scala.Language(), 14, "class A { def f(): Unit = () }\n"},
		{"swift", swift.Language(), 15, "class A { func f() {} }\n"},
	}
	for _, g := range grammars {
		t.Run(g.name, func(t *testing.T) {
			lang := sitter.NewLanguage(g.lang)
			if got := lang.AbiVersion(); got != g.abi {
				t.Errorf("ABI = %d, want %d", got, g.abi)
			}
			p := sitter.NewParser()
			defer p.Close()
			if err := p.SetLanguage(lang); err != nil {
				t.Fatalf("SetLanguage: %v (runtime accepts ABI %d..%d)",
					err, sitter.MIN_COMPATIBLE_LANGUAGE_VERSION, sitter.LANGUAGE_VERSION)
			}
			tree := p.Parse([]byte(g.src), nil)
			if tree == nil {
				t.Fatal("Parse returned nil")
			}
			defer tree.Close()
			if root := tree.RootNode(); root.HasError() {
				t.Errorf("parse error in smoke source: %s", root.ToSexp())
			}
		})
	}
}
