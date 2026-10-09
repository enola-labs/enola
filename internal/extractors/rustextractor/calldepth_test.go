package rustextractor

import (
	"reflect"
	"testing"
)

// The call sits at depth 1; the function's deepest nest, further down, is 2.
func TestRustCallDepthIsTheNestingAroundTheCall(t *testing.T) {
	f, _ := findFact(extractAST(t, `
fn save(_x: u32) {}
fn add(_a: u32, _b: u32) {}
fn r(rows: &[u32], xs: &[u32], ys: &[u32]) {
    for row in rows { save(*row); }
    for a in xs { for b in ys { add(*a, *b); } }
}
`), "pkg.r")
	calls, _ := f.Props["calls_in_scaling_loop"].([]string)
	depths, _ := f.Props["calls_in_scaling_loop_depth"].([]int)
	if !reflect.DeepEqual(calls, []string{"pkg.save", "pkg.add"}) || !reflect.DeepEqual(depths, []int{1, 2}) {
		t.Errorf("calls = %v, depths = %v; want [pkg.save pkg.add] at [1 2]", calls, depths)
	}
}
