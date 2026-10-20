package dotnetextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Under the v0.23.1 grammar each of these made the whole file one ERROR node, so the
// file produced no symbols at all; the vendored v0.23.5 parses them. Every case puts
// an ordinary member after the construct, because a file that half-parses would
// still emit the first type.
func TestCSharp12Syntax(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"body-less class with a base", `namespace Shop;

internal partial class CatalogJsonContext : JsonSerializerContext;

public class After { public void Run() { } }
`, []string{"src.CatalogJsonContext", "src.After", "src.After.Run"}},
		{"body-less class with a primary constructor", `namespace Shop;

public class ProductCountChanged(int count) : ValueChangedMessage<int>(count);

public class After { public void Run() { } }
`, []string{"src.ProductCountChanged", "src.After.Run"}},
		{"collection expression initializer", `namespace Shop;

public class Registry
{
    private readonly List<string> _names = [];
    public int[] Defaults { get; } = [1, 2, 3];
    public void Run() { }
}
`, []string{"src.Registry", "src.Registry.Run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ff := extractFileAST([]byte(tc.src), "src/Shop.cs")
			for _, want := range tc.want {
				if factByName(ff, want) == nil {
					t.Errorf("missing %s; got %v", want, names(ff, facts.KindSymbol))
				}
			}
		})
	}
}
