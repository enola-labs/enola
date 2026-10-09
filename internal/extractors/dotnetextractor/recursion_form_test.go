package dotnetextractor

import "testing"

// `base.M()` from an override of M resolves to this type's M, the only M the
// resolver knows, and is the one call that provably is not M calling itself.
func TestCSharpRecursion_BaseCallIsNotSelfCall(t *testing.T) {
	ff := extractFileAST([]byte(`
namespace Acme;
public class Provider : BaseProvider
{
    protected override IReadOnlyList<Item> GetItemsWithImages(Item item)
    {
        var items = base.GetItemsWithImages(item);
        return items;
    }

    public int Depth(Node n)
    {
        return n == null ? 0 : 1 + this.Depth(n.Parent);
    }

    public int Count(Node n) => n == null ? 0 : 1 + Count(n.Next);
}
`), "src/Provider.cs")

	if f := factByName(ff, "src.Provider.GetItemsWithImages"); f == nil {
		t.Fatalf("missing src.Provider.GetItemsWithImages")
	} else if f.Props["recursive_self"] == true {
		t.Errorf("base.GetItemsWithImages(item) is the parent's implementation, not recursion")
	}
	for _, name := range []string{"src.Provider.Depth", "src.Provider.Count"} {
		if f := factByName(ff, name); f == nil || f.Props["recursive_self"] != true {
			t.Errorf("%s calls itself and is not flagged recursive_self", name)
		}
	}
}
