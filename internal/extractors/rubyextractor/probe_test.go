package rubyextractor

import (
	"testing"

	sitter "github.com/tree-sitter/go-tree-sitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

// TestGrammarSmoke is the ABI guard, and it is the most important test in this package.
//
// The vendored go-tree-sitter runtime accepts at most tree-sitter ABI 14. A grammar
// generated against ABI 15 is refused by SetLanguage, and extractFileAST returns nil on
// that error — so the rejection is SILENT: every Ruby file parses to nothing, and the
// result is indistinguishable from a repository containing no Ruby. That is exactly how
// the C# grammar failed once (see dotnetextractor/csharp.go), and why tree-sitter-python
// carries a pin and the C#, Scala and Dart grammars are vendored with a regeneration
// step today.
//
// tree-sitter-ruby v0.23.1 is both the newest release and still ABI 14, so nothing is
// pinned back here. This test exists for the day upstream regenerates at ABI 15 and
// dependabot proposes it: the bump then fails loudly here instead of quietly deleting
// every Ruby fact from the graph.
//
// If this fails after a dependency bump, the fix is to pin the grammar to the last ABI-14
// release, not to loosen the assertion.
func TestGrammarSmoke(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
		t.Fatalf("SetLanguage failed — the grammar is almost certainly built against a "+
			"newer tree-sitter ABI than the vendored runtime accepts. Pin tree-sitter-ruby "+
			"to the last ABI-14 release. Error: %v", err)
	}

	// Each case exercises a different half of the grammar, so a regeneration that broke
	// one would not pass on the others.
	for _, tc := range []struct{ name, src string }{
		{"module_class_mixin", "module Acme\n  class Service < Base\n    include Comparable\n" +
			"    extend Forwardable\n    def call(x)\n      x.to_s\n    end\n  end\nend\n"},
		// The Rails shapes the extractor reads for storage and association facts. A grammar
		// that parsed the class but shredded the DSL calls would lose every model fact.
		{"rails_model", "class Post < ApplicationRecord\n  belongs_to :author\n" +
			"  has_many :comments, dependent: :destroy\n  scope :recent, -> { order(created_at: :desc) }\n" +
			"  validates :title, presence: true\nend\n"},
		{"routes_dsl", "Rails.application.routes.draw do\n  root 'home#index'\n" +
			"  namespace :api do\n    resources :posts, only: [:index, :show]\n  end\nend\n"},
		// Heredocs, endless methods and singleton classes are the shapes the old regex
		// scanner could not read; if the grammar stopped handling them the AST walker
		// would regress to that behavior silently.
		{"modern_syntax", "class C\n  class << self\n    def build = new\n  end\n" +
			"  def sql\n    <<~SQL\n      SELECT 1\n    SQL\n  end\n" +
			"  def self.make(**opts) = new(**opts)\nend\n"},
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
				t.Errorf("a trivial valid file parsed with errors — grammar mismatch:\n%s", root.ToSexp())
			}
			// A grammar the runtime refused yields a root with no children rather than an
			// error, so HasError alone would not catch it.
			if root.ChildCount() == 0 {
				t.Error("root has no children — the grammar was probably rejected")
			}
		})
	}
}

// TestWalkerNodeKindsStillExist pins the exact grammar node kinds the walker dispatches
// on. A grammar upgrade that renamed one would otherwise degrade extraction silently —
// the walker descends into anything it does not recognize, so a renamed `singleton_method`
// would stop producing symbols without producing a single error.
//
// The list is every string the package switches on (or compares Kind() against) that is a
// real node type in the grammar; the source below is built to produce all of them.
func TestWalkerNodeKindsStillExist(t *testing.T) {
	const src = `# a comment
require 'set'

module Acme
  CONST = 1

  class Service < Base
    include Comparable
    @@count = 0
    $global = nil

    class << self
      def build = new
    end

    def self.create(name)
      super
      new(name)
    end

    def initialize(name)
      @name = name
      @@count += 1
      a, b = 1, 2
      words = %w[alpha beta]
      syms = %i[one two]
      list = [1, 2]
      opts = { key: :value, "s" => :"quoted sym" }
      greeting = "hi #{@name}"
      i = 0
      while i < 3
        i += 1
      end
      until i.zero?
        i -= 1
      end
      i += 1 while i < 2
      i -= 1 until i.zero?
      for x in list do
        puts x
      end
      list.each { |v| puts v }
      list.each do |v|
        puts v
      end
      puts Acme::Service
      puts self
    end
  end
end
`
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	tree := parser.Parse([]byte(src), nil)
	defer tree.Close()
	if tree.RootNode().HasError() {
		t.Fatalf("the pinning source no longer parses cleanly:\n%s", tree.RootNode().ToSexp())
	}

	seen := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		seen[n.Kind()] = true
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())

	for _, kind := range []string{
		"array", "assignment", "block", "call", "class", "class_variable", "comment",
		"constant", "delimited_symbol", "do_block", "for", "global_variable", "hash",
		"identifier", "instance_variable", "integer", "interpolation",
		"left_assignment_list", "method", "module", "operator_assignment", "pair",
		"scope_resolution", "self", "simple_symbol", "singleton_class", "singleton_method",
		"string", "string_array", "string_content", "super", "symbol_array", "until",
		"until_modifier", "while", "while_modifier",
	} {
		if !seen[kind] {
			t.Errorf("grammar no longer produces node kind %q — the walker dispatches on it "+
				"and would silently stop extracting", kind)
		}
	}
}
