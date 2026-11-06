package cppextractor

import (
	"testing"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
)

// TestGrammarSmoke is the ABI guard, and it is the most important test in this package.
//
// The go-tree-sitter runtime accepts tree-sitter ABI 13 to 15. A grammar outside that
// range is refused by SetLanguage, and extractFileAST returns nil on
// that error — so the rejection is SILENT: every file parses to nothing, and the fact
// graph becomes indistinguishable from a repository containing no C or C++. That is
// exactly how the C# grammar failed once (see dotnetextractor/csharp.go).
//
// This package is the only one that loads TWO grammars, and both are asserted:
//
//   - tree-sitter-c is pinned at v0.24.0 (ABI 15): later releases require
//     go-tree-sitter v0.24.1, which was never tagged, so they build only through the
//     module proxy. The bound is recorded in .github/dependabot.yml.
//   - tree-sitter-cpp v0.23.4 is the newest release (ABI 14).
//
// If this fails after a dependency bump, the fix is to pin the offending grammar back,
// not to loosen the assertion.
func TestGrammarSmoke(t *testing.T) {
	for _, g := range []struct {
		name string
		lang *sitter.Language
		pin  string
		srcs []struct{ name, src string }
	}{
		{
			name: "c", lang: sitter.NewLanguage(c.Language()), pin: "v0.24.0",
			srcs: []struct{ name, src string }{
				{"decls_and_includes", "#include <stdio.h>\n#include \"local.h\"\n\n" +
					"typedef struct Point { int x; int y; } Point;\n" +
					"enum Color { RED, GREEN };\n\nint process(int n) { return n; }\n"},
				// Function-pointer tables and registration macros are the shapes the
				// extractor reads for call edges in C codebases.
				{"macros_and_fnptrs", "#define MAX 10\n#define SQ(x) ((x) * (x))\n\n" +
					"static int (*fp)(int) = 0;\n" +
					"static const struct ops o = { .run = handler, .stop = 0 };\n"},
			},
		},
		{
			name: "cpp", lang: sitter.NewLanguage(cpp.Language()), pin: "v0.23.4",
			srcs: []struct{ name, src string }{
				{"namespace_class", "namespace acme {\nclass Base {\npublic:\n" +
					"  virtual ~Base();\n  virtual int run() = 0;\n};\n}\n"},
				// Templates and out-of-line definitions are what make the "<dir>.<ns::Class::member>"
				// naming scheme resolve; a grammar that shredded them would split every
				// declaration from its definition.
				{"templates", "template <typename T>\nclass Holder {\n  T v_;\npublic:\n" +
					"  Holder(T v) : v_(v) {}\n  int run();\n};\n\n" +
					"template <typename T>\nint Holder<T>::run() { return 0; }\n"},
				{"modern_cpp", "#include <vector>\nint f(const std::vector<int> &xs) {\n" +
					"  auto fn = [&](int x) { return x + 1; };\n" +
					"  for (const auto &x : xs) { (void)x; }\n" +
					"  try { return fn(1); } catch (const std::exception &e) { return 0; }\n}\n"},
			},
		},
	} {
		t.Run(g.name, func(t *testing.T) {
			parser := sitter.NewParser()
			defer parser.Close()
			if err := parser.SetLanguage(g.lang); err != nil {
				t.Fatalf("SetLanguage failed for the %s grammar — it is almost certainly built "+
					"against a tree-sitter ABI the runtime does not accept. Pin it "+
					"back to %s. Error: %v", g.name, g.pin, err)
			}
			for _, tc := range g.srcs {
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
					// A grammar the runtime refused yields a root with no children rather
					// than an error, so HasError alone would not catch it.
					if root.ChildCount() == 0 {
						t.Error("root has no children — the grammar was probably rejected")
					}
				})
			}
		})
	}
}

// sharedWalkerKinds are the node kinds the walker dispatches on that exist in BOTH the C
// and the C++ grammar. The walker is one code path over two grammars, so a rename in
// either one degrades extraction for that language silently — it descends into anything
// it does not recognize, so a renamed `function_definition` stops producing symbols
// without producing a single error.
var sharedWalkerKinds = []string{
	"assignment_expression", "binary_expression", "call_expression", "case_statement",
	"compound_literal_expression", "compound_statement", "conditional_expression",
	"declaration", "do_statement", "enum_specifier", "expression_statement",
	"field_declaration", "field_expression", "field_identifier", "for_statement",
	"function_declarator", "function_definition", "identifier", "if_statement",
	"initializer_list", "initializer_pair", "linkage_specification", "number_literal",
	"parenthesized_declarator", "pointer_expression", "preproc_def", "preproc_function_def",
	"preproc_include", "struct", "struct_specifier", "type_definition", "type_identifier",
	"type_qualifier", "union_specifier", "while_statement",
}

// cSrc is written to produce every kind in sharedWalkerKinds plus the C-only
// macro_type_specifier.
const cSrc = `#include <stdio.h>
#include "local.h"

#define MAX 10
#define SQ(x) ((x) * (x))

typedef struct Point { int x; int y; } Point;
typedef int (*handler_t)(int);
typedef MACRO_TYPE(int) alias_t;

enum Color { RED, GREEN };
union U { int i; float f; };

extern "C" {
    int exported(void);
}

static const struct Point origin = { .x = 0, .y = 0 };

static int (*fp)(int) = 0;

int process(int n, const char *s) {
    struct Point p = (struct Point){ .x = 1, .y = 2 };
    int total = n + MAX;
    total = SQ(total);
    if (total > 0) {
        total = total - 1;
    }
    for (int i = 0; i < n; i++) {
        total += i;
    }
    while (total > 0) { total--; }
    do { total++; } while (total < 0);
    switch (n) {
        case 1:
            break;
        default:
            break;
    }
    int chosen = total > 3 ? 1 : 0;
    printf("%s %d\n", s, chosen);
    return p.x + total + *(&n) + origin.y;
}
`

// cppCompatSrc is the same shapes in a form the C++ grammar also accepts, so the shared
// kind set is asserted against BOTH grammars rather than only against C.
const cppCompatSrc = `#include <stdio.h>

#define MAX 10
#define SQ(x) ((x) * (x))

extern "C" {
    int exported(void);
}

typedef struct Point { int x; int y; } Point;
typedef int (*handler_t)(int);
enum Color { RED, GREEN };
union U { int i; float f; };

static const Point origin = { .x = 0, .y = 0 };

int process(int n, const char *s) {
    Point p = Point{1, 2};
    int total = n + MAX;
    total = SQ(total);
    if (total > 0) { total = total - 1; }
    for (int i = 0; i < n; i++) { total += i; }
    while (total > 0) { total--; }
    do { total++; } while (total < 0);
    switch (n) {
        case 1:
            break;
        default:
            break;
    }
    int chosen = total > 3 ? 1 : 0;
    printf("%s %d\n", s, chosen);
    return p.x + total + *(&n) + origin.y;
}
`

const cppSrc = `#include <vector>

namespace acme {

class Base {
public:
    virtual ~Base();
    virtual int run() = 0;
};

template <typename T>
class Holder : public Base {
public:
    using value_type = T;
    friend class Peer;

    Holder(T v) : v_(v) {}
    ~Holder();
    int run() override;
    T get() const { return v_; }
    bool operator==(const Holder &o) const { return v_ == o.v_; }

private:
    T v_;
};

template <typename T>
int Holder<T>::run() {
    std::vector<T> items;
    for (const auto &it : items) {
        (void)it;
    }
    auto fn = [this](int x) { return x + 1; };
    auto made = std::make_shared<T>(v_);
    Holder<T> other(v_);
    other.compare<int>();
    try {
        return fn(1);
    } catch (const std::exception &e) {
        return 0;
    }
}

} // namespace acme
`

func kindsProduced(t *testing.T, lang *sitter.Language, src string) map[string]bool {
	t.Helper()
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	tree := parser.Parse([]byte(src), nil)
	defer tree.Close()
	if tree.RootNode().HasError() {
		t.Fatalf("a pinning source no longer parses cleanly:\n%s", tree.RootNode().ToSexp())
	}
	// Unnamed kinds count too: the walker switches on keyword tokens such as "struct"
	// and "class" alongside named nodes.
	seen := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		seen[n.Kind()] = true
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())
	return seen
}

func TestWalkerNodeKindsStillExist(t *testing.T) {
	t.Run("c", func(t *testing.T) {
		seen := kindsProduced(t, sitter.NewLanguage(c.Language()), cSrc)
		for _, kind := range append(append([]string{}, sharedWalkerKinds...), "macro_type_specifier") {
			if !seen[kind] {
				t.Errorf("the C grammar no longer produces node kind %q — the walker dispatches "+
					"on it and would silently stop extracting", kind)
			}
		}
	})

	t.Run("cpp", func(t *testing.T) {
		seen := kindsProduced(t, sitter.NewLanguage(cpp.Language()), cppSrc)
		for _, kind := range []string{
			"alias_declaration", "catch_clause", "class", "class_specifier", "destructor_name",
			"for_range_loop", "friend_declaration", "lambda_expression", "namespace_definition",
			"operator_name", "qualified_identifier", "template_declaration", "template_function",
			"template_method", "template_parameter_list", "template_type", "this",
		} {
			if !seen[kind] {
				t.Errorf("the C++ grammar no longer produces node kind %q — the walker dispatches "+
					"on it and would silently stop extracting", kind)
			}
		}

		compat := kindsProduced(t, sitter.NewLanguage(cpp.Language()), cppCompatSrc)
		for _, kind := range sharedWalkerKinds {
			if !seen[kind] && !compat[kind] {
				t.Errorf("the C++ grammar no longer produces shared node kind %q — the walker "+
					"dispatches on it and would silently stop extracting", kind)
			}
		}
	})
}
