package kotlinextractor

import (
	"reflect"
	"testing"
)

// The call sits at depth 1; the function's deepest nest, further down, is 2.
func TestKtCallDepthIsTheNestingAroundTheCall(t *testing.T) {
	ff := extractAST(t, "fun save(x: Int) {}\nfun add(a: Int, b: Int) {}\nfun r(rows: List<Int>, xs: List<Int>, ys: List<Int>) {\n for (row in rows) { save(row) }\n for (a in xs) { for (b in ys) { add(a, b) } }\n}", false)
	f, _ := findFact(ff, "pkg.r")
	calls := kStrSlice(f, "calls_in_scaling_loop")
	depths, _ := f.Props["calls_in_scaling_loop_depth"].([]int)
	if !reflect.DeepEqual(calls, []string{"pkg.save", "pkg.add"}) || !reflect.DeepEqual(depths, []int{1, 2}) {
		t.Errorf("calls = %v, depths = %v; want [pkg.save pkg.add] at [1 2]", calls, depths)
	}
}
