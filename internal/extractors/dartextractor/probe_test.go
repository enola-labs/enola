package dartextractor

import (
	"testing"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/dartextractor/grammar"
)

// TestGrammarSmoke is the ABI guard, and it is the most important test in this package.
//
// The tree-sitter-dart parser is vendored under grammar/src (see grammar/ATTRIBUTION.md).
// If an update brings a parser the go-tree-sitter runtime does not accept, the rejection
// is SILENT: SetLanguage fails, every Dart file parses to nothing, and the result is
// indistinguishable from a repository containing no Dart. That is exactly how the C#
// grammar failed once.
//
// If this fails, the fix is a parser the runtime accepts, not a looser assertion.
func TestGrammarSmoke(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(grammar.Language())); err != nil {
		t.Fatalf("SetLanguage failed — the vendored parser is almost certainly built "+
			"against a tree-sitter ABI the runtime does not accept. Error: %v", err)
	}

	// Each case exercises a different half of the grammar, so a regeneration that
	// dropped one would not pass on the others.
	for _, tc := range []struct{ name, src string }{
		{"library_and_class", "library foo;\nimport 'package:flutter/material.dart';\n\n" +
			"class Counter extends StatefulWidget {\n  final int start;\n" +
			"  const Counter({super.key, this.start = 0});\n" +
			"  @override\n  State<Counter> createState() => _CounterState();\n}\n"},
		{"mixin_extension_enum", "mixin Logger on Object { void log(String m) => print(m); }\n" +
			"extension StringX on String { String get shout => toUpperCase(); }\n" +
			"enum Status { active, archived }\n"},
		// Dart 3: records, patterns and switch expressions. A grammar predating these
		// parses the file but shreds the function, which is the failure this catches.
		{"dart3_records_patterns", "Future<(int, String)> load(Uri u) async {\n" +
			"  final r = await http.get(u);\n" +
			"  return switch (r.statusCode) { 200 => (1, r.body), _ => (0, '') };\n}\n"},
		{"null_safety_generics", "class Repo<T extends Object?> {\n" +
			"  final Map<String, T?> _cache = {};\n" +
			"  T? find(String id) => _cache[id];\n" +
			"  Future<void> save(String id, T v) async => _cache[id] = v;\n}\n"},
		// The single most common shape in Flutter code. If the widget tree does not
		// parse, essentially every app in the corpus loses its call edges.
		{"widget_tree", "Widget build(BuildContext context) {\n  return Scaffold(\n" +
			"    appBar: AppBar(title: const Text('Hi')),\n" +
			"    body: ListView.builder(itemBuilder: (ctx, i) => ListTile(title: Text('$i'))),\n" +
			"  );\n}\n"},
		{"part_directives", "part of 'host.dart';\n\nclass Piece {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := parser.Parse([]byte(tc.src), nil)
			if tree == nil {
				t.Fatal("nil tree")
			}
			defer tree.Close()
			root := tree.RootNode()
			if root == nil {
				t.Fatal("nil root node")
			}
			if root.HasError() {
				t.Errorf("parse error in %s:\n%s", tc.name, root.ToSexp())
			}
			// A grammar the runtime refused yields a root with no children rather than
			// an error, so HasError alone would not catch it.
			if root.ChildCount() == 0 {
				t.Error("root has no children — the grammar was probably rejected")
			}
		})
	}
}
