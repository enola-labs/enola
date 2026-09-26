package scalaextractor

import (
	"testing"

	scala "github.com/enola-labs/enola/internal/extractors/scalaextractor/grammar"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// TestGrammarSmoke is the ABI guard, and it is the most important test in this
// package.
//
// tree-sitter-scala v0.25.0 and later are generated against tree-sitter ABI 15,
// while the vendored go-tree-sitter runtime accepts at most 14. The rejection is
// SILENT: SetLanguage fails, every file parses to nothing, and the result is
// indistinguishable from a repository that contains no Scala. That is exactly how
// the C# grammar failed once, which is why the grammar is vendored under grammar/
// and regenerated at ABI 14, and why this test asserts it still loads rather than
// trusting the vendored bytes to stay put.
//
// If this fails after a grammar update, the fix is to regenerate at ABI 14 (see
// grammar/ATTRIBUTION.md), not to loosen the assertion.
func TestGrammarSmoke(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(scala.Language())); err != nil {
		t.Fatalf("SetLanguage failed — the grammar is almost certainly built against a "+
			"newer tree-sitter ABI than the vendored runtime accepts. Regenerate "+
			"grammar/src/parser.c with --abi 14. Error: %v", err)
	}

	// Both dialects, because they exercise different halves of the grammar and a
	// version that dropped one would otherwise pass on the other.
	for _, tc := range []struct{ name, src string }{
		{"scala2", "package a.b\n\nclass Foo(x: Int) extends Bar {\n  def run(): Unit = println(x)\n}\n"},
		{"scala3", "package a.b\n\nenum Color:\n  case Red, Green\n\ntrait Store[F[_]]:\n  def get(id: Long): F[String]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := parser.Parse([]byte(tc.src), nil)
			defer tree.Close()
			root := tree.RootNode()
			if root == nil {
				t.Fatal("nil root node")
			}
			if root.HasError() {
				t.Fatalf("a trivial valid file parsed with errors — grammar mismatch:\n%s", root.ToSexp())
			}
		})
	}
}

// TestWalkerNodeKindsStillExist pins the exact grammar node kinds the walker
// dispatches on. A grammar upgrade that renames one would otherwise degrade
// extraction silently — the walker's default branch descends into anything it does
// not recognize, so a renamed `object_definition` would stop producing symbols
// without producing a single error.
func TestWalkerNodeKindsStillExist(t *testing.T) {
	src := `package a.b

import c.d.E
import c.d.{F, G => H}

class Klass extends E
case class Rec(x: Int)
object Obj
case object CObj
trait Trait { def m(): Int }
enum Enum:
  case One

given Ordering[Int] with
  def compare(a: Int, b: Int): Int = 0

extension (s: String) def shout: String = s

sealed abstract class Modified

object Holder {
  type Alias = Int
  val v = 1
  var mut = 2
  def f(): Int = 1
  private def hidden(): Int = 2
  val made = new Klass()
}
`
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(scala.Language())); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	tree := parser.Parse([]byte(src), nil)
	defer tree.Close()

	seen := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n.IsNamed() {
			seen[n.Kind()] = true
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())

	// Every kind the walker's switch branches on, and the field-bearing nodes it
	// reads through ChildByFieldName.
	for _, kind := range []string{
		"package_clause", "import_declaration", "namespace_selectors",
		"arrow_renamed_identifier", "class_definition", "object_definition",
		"trait_definition", "enum_definition", "simple_enum_case",
		"function_definition", "function_declaration", "val_definition",
		"var_definition", "type_definition", "given_definition",
		"extension_definition", "instance_expression", "extends_clause",
		"modifiers", "template_body",
	} {
		if !seen[kind] {
			t.Errorf("grammar no longer produces node kind %q — the walker dispatches on it "+
				"and would silently stop extracting", kind)
		}
	}
}

// TestCaptureCheckingSyntax pins that Scala 3 capture-checking syntax parses and
// that the declarations around it keep their symbols and their nesting. A grammar
// without it does not fail loudly: error recovery re-syncs somewhere past the
// capture set, so a method whose result is `() ->{this} Unit` disappears, a class
// with a capture parameter loses its members to the enclosing object, and the
// receipt still reports no parse errors. Every form below is accepted by the
// Scala 3.8 compiler under -language:experimental.captureChecking.
func TestCaptureCheckingSyntax(t *testing.T) {
	src := `package com.example.caps

import scala.caps.{any, SharedCapability}

class CanRead extends SharedCapability {
  def read(): String = "data"
}

object Caps {
  def format(x: CanRead): () ->{x} String = () => x.read()
  def combine(a: CanRead, b: CanRead): () ->{a, b} String = () => a.read() + b.read()
  def log(msg: String): () ->{any} Unit = () => println(msg)
  def load(using r: CanRead): () ->{r} String = () => r.read()
  def pure(f: String -> Int): Int = f("x")

  class Box[T, C^] {
    private var value: T = compiletime.uninitialized
    def get: T^{C} = value
    def put(x: T): Unit = value = x
  }

  def makeBox[T](t: T): Box[T, {any}] = new Box[T, {any}]

  class Store extends SharedCapability {
    def apply(e: String): () ->{this} Unit = () => println(e)
  }

  def after: Int = 1
}
`
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(scala.Language())); err != nil {
		t.Fatal(err)
	}
	tree := parser.Parse([]byte(src), nil)
	defer tree.Close()
	if root := tree.RootNode(); root.HasError() {
		t.Errorf("capture-checking syntax parsed with errors:\n%s", root.ToSexp())
	}

	ff := extractAST(t, "src/Caps.scala", src)
	for name, kind := range map[string]string{
		"src.Caps.format":      facts.SymbolMethod,
		"src.Caps.combine":     facts.SymbolMethod,
		"src.Caps.log":         facts.SymbolMethod,
		"src.Caps.load":        facts.SymbolMethod,
		"src.Caps.pure":        facts.SymbolMethod,
		"src.Caps.Box":         facts.SymbolClass,
		"src.Caps.Box.get":     facts.SymbolMethod,
		"src.Caps.Box.put":     facts.SymbolMethod,
		"src.Caps.makeBox":     facts.SymbolMethod,
		"src.Caps.Store":       facts.SymbolClass,
		"src.Caps.Store.apply": facts.SymbolMethod,
		"src.Caps.after":       facts.SymbolMethod,
	} {
		if got := findFact(t, ff, name).Props["symbol_kind"]; got != kind {
			t.Errorf("%s: symbol_kind = %v, want %s", name, got, kind)
		}
	}
	// A class's members stay inside it rather than being hoisted to the object.
	for _, f := range ff {
		if f.Name == "src.Caps.get" || f.Name == "src.Caps.put" {
			t.Errorf("%s: a Box member was hoisted out of Box", f.Name)
		}
	}
}
