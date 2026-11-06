package kotlinextractor

import (
	"testing"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	kotlin "github.com/tree-sitter-grammars/tree-sitter-kotlin/bindings/go"
)

const nodeKindSrc = `package com.acme.svc

import com.acme.support.Helper
import com.acme.support.*

typealias Handler = (String) -> Boolean

interface Runner {
    fun run(input: String): Boolean
}

object Registry {
    val items = mutableListOf<String>()
}

abstract class Base

class Service(private val name: String, val helper: Helper) : Base(), Runner by Registry {
    private var count: Int = 0
    private val fn: (Int) -> Int = { it + 1 }
    private val cb: ((Int) -> Unit)? = null

    constructor(name: String) : this(name, Helper())

    override fun run(input: String): Boolean {
        val n: Int? = input.length
        val safe: Int = n ?: 0
        val list = listOf(1, 2, 3)
        var total = 0
        for (i in 0..safe) {
            total += i
        }
        if (total > 0) { total = 1 }
        while (total > 0) { total-- }
        do { total++ } while (total < 0)
        val label = when (total) {
            0 -> "zero"
            else -> "many"
        }
        val (a, b) = Pair(1, 2)
        val shifted = total shl 1
        try {
            helper.call(input)
        } catch (e: Exception) {
            total = 0
        }
        return (label.isNotEmpty()) && helper.nested.deep.check(a, b) && fn(shifted) > 0
    }

    fun <T> definitely(value: T & Any) = value

    fun <T> generic(value: T, block: (T) -> Unit) where T : Comparable<T> {
        block(value)
    }
}
`

// TestWalkerNodeKindsStillExist pins the exact grammar node kinds the walker dispatches
// on. A grammar upgrade that renamed one would otherwise degrade extraction silently —
// the walker descends into anything it does not recognize, so a renamed declaration node
// stops producing symbols without producing a single error.
//
// The list is every string this package switches on (or compares Kind() against) that is
// a real node type in the grammar; the sources below are built to produce all of them.
func TestWalkerNodeKindsStillExist(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(kotlin.Language())); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	tree := parser.Parse([]byte(nodeKindSrc), nil)
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
	want := []string{
		"binary_expression", "block", "call_expression", "catch_block", "class_declaration",
		"constructor_invocation", "do_while_statement", "explicit_delegation", "for_statement",
		"function_body", "function_declaration", "function_type", "function_value_parameters",
		"identifier", "if_expression", "import", "infix_expression", "interface", "modifiers",
		"multi_variable_declaration", "navigation_expression", "non_nullable_type",
		"nullable_type", "number_literal", "object_declaration", "package_header", "parameter",
		"parenthesized_expression", "parenthesized_type", "property_declaration",
		"range_expression", "type_alias", "type_constraints", "type_parameters", "user_type",
		"value_argument", "variable_declaration", "when_entry", "while_statement",
	}
	for _, k := range want {
		if !seen[k] {
			t.Errorf("grammar no longer produces node kind %q — the walker dispatches on it "+
				"and would silently stop extracting", k)
		}
	}
}

// TestGrammarSmoke is the ABI guard, and it is the most important test in this package.
//
// The go-tree-sitter runtime accepts tree-sitter ABI 13 to 15. A grammar outside that
// range is refused by SetLanguage, and the extractor returns nil on
// that error — so the rejection is SILENT: every Kotlin file parses to nothing, and the
// fact graph becomes indistinguishable from a repository containing none. That is exactly
// how the C# grammar failed once (see dotnetextractor/csharp.go).
//
// tree-sitter-kotlin v1.1.0 is the newest release (ABI 14), so nothing is pinned back and
// .github/dependabot.yml carries no bound for it. This test exists for the day upstream
// regenerates beyond what the runtime accepts: the bump then fails loudly here instead of
// quietly deleting every Kotlin fact from the graph, and the fix is to add that bound.
//
// If this fails after a dependency bump, the fix is to pin the grammar back, not to loosen
// the assertion.
func TestGrammarSmoke(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(kotlin.Language())); err != nil {
		t.Fatalf("SetLanguage failed — the grammar is almost certainly built against a "+
			"tree-sitter ABI the runtime does not accept. Error: %v", err)
	}

	// Each case exercises a different half of the grammar, so a regeneration that broke
	// one would not pass on the others.
	for _, tc := range []struct{ name, src string }{
		{"package_class", "package com.acme.svc\n\nimport com.acme.Helper\n\nclass Service(private val helper: Helper) : Base() {\n    fun run(x: String): Boolean = x.isNotEmpty()\n}\n"},
		{"data_object_sealed", "data class Point(val x: Int, val y: Int)\n\n// The object body is deliberately multi-line: tree-sitter-kotlin v1.1.0 mis-parses a\n// single-line body containing a generic call, reading `mutableListOf<String>()` as a\n// comparison. Not a regression to assert against, but a shape to keep out of the guard.\nobject Registry {\n    val items = mutableListOf<String>()\n}\n\nsealed class Outcome {\n    data class Ok(val value: Int) : Outcome()\n    object Err : Outcome()\n}\n"},
		{"coroutines_lambdas", "suspend fun load(url: String): String = withContext(Dispatchers.IO) {\n    client.get(url).also { println(it) }\n}\n"},
		{"android_annotations", "@AndroidEntryPoint\nclass MainActivity : AppCompatActivity() {\n    @Inject lateinit var repo: Repo\n    override fun onCreate(savedInstanceState: Bundle?) {\n        super.onCreate(savedInstanceState)\n    }\n}\n"},
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
