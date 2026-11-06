package cppextractor

import (
	"testing"
	"unsafe"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
)

// This extractor is the one where a kind table can go wrong quietly, because it
// parses two grammars whose symbol ids mean different things: of the ids below
// tree-sitter-c's symbol count, only 17% name the same node type in
// tree-sitter-cpp. Read a C node's id out of the C++ table and you get a real kind
// name for the wrong node type — no panic, no parse error, just a construct the
// extractor stops recognising.
//
// So both grammars are checked, each against its own table, over sources using the
// constructs each one has and the other does not.
func TestKindTable_MatchesNodeKind(t *testing.T) {
	cases := []struct {
		lang    string
		grammar func() unsafe.Pointer
		src     string
	}{
		{langC, c.Language, `
#include <stdio.h>
#define DEVICE_ATTR(n) static struct x n##_attr
typedef struct point { int x, y; } point_t;
enum e { A = 1, B };
static int helper(const char *s, ...) { return 0; }
int main(int argc, char **argv) {
    struct point p = { .x = 1, .y = 2 };
    for (int i = 0; i < argc; i++) { helper(argv[i]); }
    switch (argc) { case 0: break; default: goto out; }
out:
    return p.x;
}
static const struct ops o = { .init = main, .exit = 0 };
`},
		{"cpp", cpp.Language, `
#include <vector>
namespace ns { inline namespace v1 {
template <typename T, int N = 0>
class C : public Base<T> {
public:
    C() noexcept = default;
    virtual ~C() override {}
    auto m() const -> decltype(auto) { return T{}; }
    template <class U> void t(U&& u) { (void)std::forward<U>(u); }
private:
    std::vector<T> v_;
};
using Alias = C<int>;
enum class E : int { A, B };
} }
struct S final { int operator()(int a) const { return a; } };
auto lambda = [](auto x) mutable noexcept -> int { return x; };
`},
	}

	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			parser := sitter.NewParser()
			defer parser.Close()
			grammar := tc.grammar()
			if err := parser.SetLanguage(sitter.NewLanguage(grammar)); err != nil {
				t.Fatal(err)
			}
			tree := parser.Parse([]byte(tc.src), nil)
			if tree == nil {
				t.Fatal("parse produced no tree")
			}
			defer tree.Close()

			table := kindsFor(tc.lang)
			var checked int
			walkAll(tree.RootNode(), func(n *sitter.Node) {
				checked++
				if got, want := kindOf(table, n), n.Kind(); got != want {
					t.Fatalf("kind table disagrees with Node.Kind(): id=%d table=%q Kind()=%q",
						n.KindId(), got, want)
				}
			})
			if checked == 0 {
				t.Fatal("no nodes checked")
			}
			t.Logf("checked %d nodes", checked)
		})
	}
}

// The tables must be DIFFERENT objects, or the two-grammar problem this file exists
// for has been quietly reintroduced by a shared package-level variable.
func TestKindTable_PerGrammar(t *testing.T) {
	if kindsFor(langC) == kindsFor("cpp") {
		t.Fatal("C and C++ share one kind table; their symbol ids do not agree")
	}
}

func walkAll(n *sitter.Node, fn func(*sitter.Node)) {
	fn(n)
	for i := uint(0); i < n.ChildCount(); i++ {
		walkAll(n.Child(i), fn)
	}
}
