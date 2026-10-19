package dotnetextractor

import (
	"testing"

	csharp "github.com/enola-labs/enola/internal/extractors/dotnetextractor/grammar"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

const csMainSrc = `#if DEBUG
using System;
#elif RELEASE
using System.IO;
#else
using System.Text;
#endif
using Acme.Support;

namespace Acme.Http;

public delegate void Notify(int code);

public interface IHandler { string Handle(string input); }

public enum Status { Active, Archived }

public struct Point { public int X; public int Y; }

public record Person(string Name, int Age);

public record Employee(string Name) : Person(Name, 0);

public class Base { public Base(int x) {} }

public class Controller(int seed) : Base(seed), IHandler
{
    private readonly Helper _helper = new Helper();
    public const int Max = 10;
    public event EventHandler Changed;
    public string Name { get; set; }
    public int this[int i] => i;

    public Controller() : this(0) {}

    ~Controller() {}

    public static Controller operator +(Controller a, Controller b) => a;
    public static explicit operator int(Controller c) => 0;

    public string Handle(string input)
    {
        int n = input.Length + Max;
        var arr = new int[] { 1, 2 };
        int? maybe = null;
        var verbatim = @"c:\path";
        var interp = $"hi {input}";
        Func<int, int> lambda = x => x + 1;
        Notify anon = delegate(int c) { };
        int Local() => 1;
        for (int i = 0; i < n; i++) { n += i; }
        foreach (var a in arr) { n += a; }
        while (n > 0) { n--; }
        do { n++; } while (n < 0);
        var g = new List<string>();
        _helper.Run();
        global::System.Console.WriteLine(interp);
        return Local() + n + lambda(1) + verbatim + maybe;
    }
}
`

// A block-bodied namespace cannot coexist with a file-scoped one, and unsafe pointer
// types need their own compilation unit to keep the main sample plain C#.
const csBlockNsSrc = `[module: System.Reflection.AssemblyMetadata("k", "v")]

namespace Acme.Legacy
{
    unsafe class P { int* ptr; }
}
`

// Top-level statements are only legal in a file with no namespace declaration.
const csTopLevelSrc = `using System;

Console.WriteLine("hi");
`

// Raw string literals need their own file so the triple quotes do not fight with Go's
// backtick strings.
var csRawStringSrc = "class R { string V() => \"\"\"\nliteral\n\"\"\"; }\n"

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
	if err := parser.SetLanguage(sitter.NewLanguage(csharp.Language())); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	seen := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		seen[n.Kind()] = true
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	for name, src := range map[string]string{
		"main": csMainSrc, "blockns": csBlockNsSrc,
		"toplevel": csTopLevelSrc, "raw": csRawStringSrc,
	} {
		tree := parser.Parse([]byte(src), nil)
		if tree.RootNode().HasError() {
			t.Fatalf("pinning source %v no longer parses cleanly:\n%s", name, tree.RootNode().ToSexp())
		}
		walk(tree.RootNode())
		tree.Close()
	}
	want := []string{
		"alias_qualified_name", "anonymous_method_expression", "argument", "argument_list",
		"array_type", "arrow_expression_clause", "binary_expression", "class", "class_declaration",
		"compilation_unit", "constructor_declaration", "conversion_operator_declaration",
		"delegate_declaration", "destructor_declaration", "do_statement", "enum",
		"enum_declaration", "event_field_declaration", "field_declaration",
		"file_scoped_namespace_declaration", "for_statement", "foreach_statement", "generic_name",
		"global", "global_statement", "identifier", "indexer_declaration", "integer_literal",
		"interface", "interface_declaration", "interpolated_string_expression",
		"invocation_expression", "lambda_expression", "local_declaration_statement",
		"local_function_statement", "member_access_expression", "method_declaration", "modifier",
		"module", "namespace", "namespace_declaration", "nullable_type",
		"object_creation_expression", "operator_declaration", "parameter", "pointer_type",
		"preproc_elif", "preproc_else", "preproc_if", "primary_constructor_base_type",
		"property_declaration", "qualified_name", "raw_string_literal", "record_declaration",
		"static", "string_literal", "struct_declaration", "using_directive", "variable_declarator",
		"verbatim_string_literal", "while_statement",
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
// The vendored go-tree-sitter runtime accepts at most tree-sitter ABI 14. A grammar
// generated against ABI 15 is refused by SetLanguage, and the extractor returns nil on
// that error — so the rejection is SILENT: every C# file parses to nothing, and the
// fact graph becomes indistinguishable from a repository containing none. That is exactly
// how the C# grammar failed once (see dotnetextractor/csharp.go).
//
// The grammar is vendored in ./grammar: upstream v0.23.5 regenerated at ABI 14, because
// every release after v0.23.1 ships at ABI 15. This is the grammar the whole policy
// exists because of: it is the one that actually shipped broken once.
//
// If this fails after a regeneration, the fix is to regenerate with `--abi 14` (see
// grammar/ATTRIBUTION.md), not to loosen the assertion.
func TestGrammarSmoke(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(csharp.Language())); err != nil {
		t.Fatalf("SetLanguage failed — the grammar is almost certainly built against a "+
			"newer tree-sitter ABI than the vendored runtime accepts. Error: %v", err)
	}

	// Each case exercises a different half of the grammar, so a regeneration that broke
	// one would not pass on the others.
	for _, tc := range []struct{ name, src string }{
		{"namespace_class", "namespace Acme.Http;\n\npublic class Service : Base, IHandler\n{\n    private readonly Helper _helper;\n    public Service(Helper helper) { _helper = helper; }\n    public string Handle(string input) => input;\n}\n"},
		{"aspnet_routes", "[ApiController]\n[Route(\"api/[controller]\")]\npublic class PostsController : ControllerBase\n{\n    [HttpGet(\"{id}\")]\n    public ActionResult<Post> Get(int id) => Ok(new Post());\n}\n"},
		{"records_generics_linq", "public record Person(string Name, int Age);\n\npublic class Repo<T> where T : class\n{\n    public IEnumerable<T> All() => _items.Where(x => x != null).ToList();\n}\n"},
		{"block_namespace", "namespace Acme.Legacy\n{\n    internal static class Util { public static int Add(int a, int b) => a + b; }\n}\n"},
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
