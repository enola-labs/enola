package javaextractor

import (
	"testing"

	sitter "github.com/tree-sitter/go-tree-sitter"
	java "github.com/tree-sitter/tree-sitter-java/bindings/go"
)

// TestGrammarSmoke is the ABI guard, and it is the most important test in this package.
//
// The vendored go-tree-sitter runtime accepts at most tree-sitter ABI 14. A grammar
// generated against ABI 15 is refused by SetLanguage, and extractFileAST returns nil on
// that error — so the rejection is SILENT: every Java file parses to nothing, and the
// result is indistinguishable from a repository containing no Java. That is exactly how
// the C# grammar failed once (see dotnetextractor/csharp.go), and why tree-sitter-python
// carries a pin and the C#, Scala and Dart grammars are vendored with a regeneration
// step today.
//
// tree-sitter-java v0.23.5 is both the newest release and still ABI 14, so nothing is
// pinned back here. This test exists for the day upstream regenerates at ABI 15 and
// dependabot proposes it: the bump then fails loudly here instead of quietly deleting
// every Java fact from the graph.
//
// If this fails after a dependency bump, the fix is to pin the grammar to the last ABI-14
// release, not to loosen the assertion.
func TestGrammarSmoke(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(java.Language())); err != nil {
		t.Fatalf("SetLanguage failed — the grammar is almost certainly built against a "+
			"newer tree-sitter ABI than the vendored runtime accepts. Pin tree-sitter-java "+
			"to the last ABI-14 release. Error: %v", err)
	}

	// Each case exercises a different half of the grammar, so a regeneration that broke
	// one would not pass on the others.
	for _, tc := range []struct{ name, src string }{
		{"class_and_imports", "package a.b;\n\nimport java.util.List;\n\n" +
			"public class Foo extends Bar implements Baz {\n" +
			"  private final List<String> xs;\n  public Foo(List<String> xs) { this.xs = xs; }\n}\n"},
		{"interface_enum_record", "package a.b;\n\ninterface Handler<T> { T handle(String s); }\n" +
			"enum Status { ACTIVE, ARCHIVED }\nrecord Point(int x, int y) {}\n"},
		// The Spring shapes the extractor reads for routes and injection. A grammar that
		// parsed the class but shredded annotations would lose every route fact.
		{"annotations", "package a.b;\n\n@RestController\n@RequestMapping(path = \"/api\")\n" +
			"class Ctl {\n  @Autowired private Svc svc;\n" +
			"  @GetMapping(\"/x\") public String x() { return svc.get(); }\n}\n"},
		{"generics_lambdas", "package a.b;\n\nclass G {\n" +
			"  <T> java.util.Map<String, T> m(T t) {\n" +
			"    Runnable r = () -> System.out.println(t);\n    r.run();\n" +
			"    return new java.util.HashMap<>();\n  }\n}\n"},
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
// the walker descends into anything it does not recognize, so a renamed
// `method_declaration` would stop producing symbols without producing a single error.
//
// The list is every string the package switches on (or compares Kind() against) that is a
// real node type in the grammar; the source below is built to produce all of them.
func TestWalkerNodeKindsStillExist(t *testing.T) {
	const src = `package com.acme.svc;

import java.util.List;
import java.util.*;
import static java.util.Collections.emptyList;

@interface Marker { String value(); }

@FunctionalInterface
interface Handler<T> { T handle(String in); }

enum Status { ACTIVE, ARCHIVED }

record Point(int x, int y) {}

@Component
@RequestMapping(path = "/api", produces = {"application/json"})
public class Service extends Base implements Handler<String> {
    private static final int MAX = 10;
    private static final int HEX = 0xFF;
    private static final int OCT = 017;
    private static final int BIN = 0b1010;
    private static final long BIG = 5L;
    private static final short SH = 1;
    private static final byte BY = 2;
    private static final char CH = 'a';
    private static final float F = 1.0f;
    private static final double D = 2.0;
    private final List<String> names = new ArrayList<>();
    private final String[] arr = new String[]{"a", "b"};
    private final Map.Entry<String, String> entry = null;
    private final List<@Deprecated String> annotated = new ArrayList<>();

    public Service(String name) {
        this.names.add(name);
    }

    @Override
    public String handle(String in) {
        int n = in.length() + MAX;
        String out = n > 3 ? "big" : "small";
        if (n > 0) {
            out = out.toUpperCase();
        }
        for (int i = 0; i < n; i++) {
            out += i;
        }
        for (String s : names) {
            out += s;
        }
        while (n > 0) { n--; }
        do { n++; } while (n < 0);
        switch (n) {
            case 1:
                break;
            default:
                break;
        }
        try {
            out = java.util.Objects.requireNonNull(out);
        } catch (RuntimeException e) {
            out = "";
        }
        Runnable r = () -> System.out.println(out);
        r.run();
        return out;
    }

    void varargs(String... parts) {}
}
`
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(java.Language())); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	tree := parser.Parse([]byte(src), nil)
	defer tree.Close()
	if tree.RootNode().HasError() {
		t.Fatalf("the pinning source no longer parses cleanly:\n%s", tree.RootNode().ToSexp())
	}

	// Unnamed kinds count too: the walker switches on keyword tokens such as "static"
	// and "asterisk" alongside named nodes.
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
		"annotated_type", "annotation", "annotation_type_declaration", "array_creation_expression",
		"array_initializer", "array_type", "asterisk", "binary_expression", "binary_integer_literal",
		"byte", "catch_clause", "char", "class_declaration", "constructor_declaration",
		"decimal_integer_literal", "do_statement", "double", "element_value_array_initializer",
		"element_value_pair", "enhanced_for_statement", "enum_declaration", "field_access",
		"field_declaration", "float", "for_statement", "formal_parameter", "generic_type",
		"hex_integer_literal", "identifier", "if_statement", "import_declaration", "int",
		"interface_declaration", "lambda_expression", "long", "marker_annotation",
		"method_declaration", "method_invocation", "object_creation_expression",
		"octal_integer_literal", "record_declaration", "scoped_identifier", "scoped_type_identifier",
		"short", "spread_parameter", "static", "string_literal", "switch_label",
		"ternary_expression", "type_identifier", "while_statement",
	} {
		if !seen[kind] {
			t.Errorf("grammar no longer produces node kind %q — the walker dispatches on it "+
				"and would silently stop extracting", kind)
		}
	}
}
