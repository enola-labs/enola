package dotnetextractor

import (
	"testing"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	csharp "github.com/enola-labs/enola/internal/extractors/dotnetextractor/grammar"
)

// TestGrammarPatchesParseClean pins each fix under grammar/patches at the parse level:
// every source here is a syntax error in upstream tree-sitter-c-sharp. Some of them cost
// no symbols, because error recovery happens to stay local, so the extraction tests
// alone would not notice the patch going missing.
func TestGrammarPatchesParseClean(t *testing.T) {
	cases := []struct{ patch, src string }{
		{"01-pointer-indirection", "class A { unsafe void F(byte* p) { *(int*)p = 1; c = *++p; *p++ ^= m; } }"},
		{"02-ref-partial-struct", "public ref partial struct R { }"},
		{"02-ref-partial-struct", "readonly ref partial struct R { }"},
		{"03-allows-ref-struct", "interface I<T> where T : allows ref struct { }"},
		{"03-allows-ref-struct", "class C { void F<T>() where T : class, allows ref struct { } }"},
		{"04-safe-modifier", "class C { public static safe extern double Acos(double d); }"},
		{"04-safe-modifier", "struct S { public safe int Handle; }"},
	}
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(csharp.Language())); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	for _, tc := range cases {
		tree := parser.Parse([]byte(tc.src), nil)
		if root := tree.RootNode(); root.HasError() {
			t.Errorf("%s: parse error in %q:\n%s", tc.patch, tc.src, root.ToSexp())
		}
		tree.Close()
	}
}

// TestGrammarPatchesKeepContextualKeywords: each patch that makes a word a keyword in
// one position keeps it an ordinary identifier everywhere else, the way C# treats it.
func TestGrammarPatchesKeepContextualKeywords(t *testing.T) {
	for _, src := range []string{
		"class C { int allows = 1; void F() { allows++; var x = allows; } }",
		"class C { bool safe; void F() { safe = true; var x = safe ? 1 : 2; safe.ToString(); } }",
		"class C { safe F() => null; void G(safe s, int safe) { } public safe safe; }",
	} {
		parser := sitter.NewParser()
		_ = parser.SetLanguage(sitter.NewLanguage(csharp.Language()))
		tree := parser.Parse([]byte(src), nil)
		if root := tree.RootNode(); root.HasError() {
			t.Errorf("parse error in %q:\n%s", src, root.ToSexp())
		}
		tree.Close()
		parser.Close()
	}
}
