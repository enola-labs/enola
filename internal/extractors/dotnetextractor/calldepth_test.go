package dotnetextractor

import (
	"reflect"
	"testing"
)

// The call sits at depth 1; the function's deepest nest, further down, is 2.
func TestCSharpCallDepthIsTheNestingAroundTheCall(t *testing.T) {
	ff := extractFileAST([]byte(`
namespace Acme;
public class C
{
    public void R(List<int> rows, List<int> xs, List<int> ys)
    {
        foreach (var row in rows) { Store(row); }
        foreach (var a in xs) { foreach (var b in ys) { Pair(a, b); } }
    }
    void Store(int x) {}
    void Pair(int a, int b) {}
}
`), "src/C.cs")
	f := factByName(ff, "src.C.R")
	if f == nil {
		t.Fatalf("missing src.C.R")
	}
	calls, _ := f.Props["calls_in_scaling_loop"].([]string)
	depths, _ := f.Props["calls_in_scaling_loop_depth"].([]int)
	if !reflect.DeepEqual(calls, []string{"src.C.Store", "src.C.Pair"}) || !reflect.DeepEqual(depths, []int{1, 2}) {
		t.Errorf("calls = %v, depths = %v; want [src.C.Store src.C.Pair] at [1 2]", calls, depths)
	}
}
