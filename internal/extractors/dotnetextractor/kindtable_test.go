package dotnetextractor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sitter "github.com/tree-sitter/go-tree-sitter"
	csharp "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
)

// kindOf reads a node's type out of a symbol-id table instead of calling
// Node.Kind(), which allocates a Go string per call. The two are supposed to be the
// same string by two routes — but tree-sitter resolves an ALIASED node's type
// differently from its symbol, and nothing in the API promises the results agree.
//
// So it is checked rather than assumed, over every node of real C# rather than a
// snippet: if these ever diverge the extractor would read a plausible, wrong kind and
// silently stop recognising a construct. That failure has no other symptom.
func TestKindTable_MatchesNodeKind(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(csharp.Language())); err != nil {
		t.Fatal(err)
	}

	var checked, files int
	for _, src := range csharpTestSources(t) {
		tree := parser.Parse(src, nil)
		if tree == nil {
			continue
		}
		files++
		walkAll(tree.RootNode(), func(n *sitter.Node) {
			checked++
			if got, want := kindOf(n), n.Kind(); got != want {
				t.Fatalf("kind table disagrees with Node.Kind(): id=%d table=%q Kind()=%q",
					n.KindId(), got, want)
			}
		})
		tree.Close()
	}
	if checked == 0 {
		t.Fatal("no nodes checked — the corpus below found nothing to parse")
	}
	t.Logf("checked %d nodes across %d sources", checked, files)
}

// A nil node must not panic where Node.Kind() would: extractors guard for nil
// unevenly, and turning a crash into a miss is the safer failure for a parser.
func TestKindOf_NilNode(t *testing.T) {
	if got := kindOf(nil); got != "" {
		t.Errorf("kindOf(nil) = %q, want empty", got)
	}
}

func walkAll(n *sitter.Node, fn func(*sitter.Node)) {
	fn(n)
	for i := uint(0); i < n.ChildCount(); i++ {
		walkAll(n.Child(i), fn)
	}
}

// csharpTestSources returns this package's .cs fixtures, plus a snippet exercising
// the constructs most likely to be aliased in the grammar.
func csharpTestSources(t *testing.T) [][]byte {
	t.Helper()
	out := [][]byte{[]byte(`
namespace N;
using System;
using Alias = System.Collections.Generic.List<int>;

[Route("/x")]
public abstract partial class C<T> : Base, IFace where T : struct {
    private const int K = 1;
    public event EventHandler? Ev;
    public int P { get; init; } = 2;
    public C(int a) : base(a) { }
    ~C() { }
    public static implicit operator int(C<T> c) => 0;
    public async Task<int> M(params object[] xs) {
        var q = from x in xs where x != null select x;
        switch (xs.Length) { case 0: break; default: goto case 0; }
        try { await Task.Delay(1); } catch (Exception e) when (e is null) { throw; }
        return xs is [var first, ..] ? 1 : 0;
    }
}
public record struct R(int A, string B);
public enum E { A = 1, B }
public delegate void D(int x);
public interface IFace { static abstract int S(); }
`)}

	// The snippet above is the always-on check and is deliberately small. Set
	// ENOLA_CS_CORPUS to a checkout to run the same assertion over real C# — which
	// is the evidence that matters, since aliasing is a property of constructs
	// nobody thought to write into a fixture:
	//
	//	ENOLA_CS_CORPUS=$DEV/dotnet/runtime go test ./internal/extractors/dotnetextractor/ -run KindTable -v
	root := os.Getenv("ENOLA_CS_CORPUS")
	if root == "" {
		root = "testdata"
	}
	const maxFiles = 3000 // enough to be evidence, quick enough to stay a test
	_ = filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(path, ".cs") || len(out) > maxFiles {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil {
			out = append(out, data)
		}
		return nil
	})
	return out
}
