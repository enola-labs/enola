package javaextractor

import (
	"reflect"
	"testing"
)

// The call sits at depth 1; the function's deepest nest, further down, is 2.
func TestJavaCallDepthIsTheNestingAroundTheCall(t *testing.T) {
	f := javaClass(t, "void r(java.util.List<Item> rows, java.util.List<Item> xs, java.util.List<Item> ys) {\n  for (Item row : rows) { store(row); }\n  for (Item a : xs) { for (Item b : ys) { pair(a, b); } }\n}\nvoid store(Item x) {}\nvoid pair(Item a, Item b) {}", "r")
	calls := jStrSlice(f, "calls_in_scaling_loop")
	depths, _ := f.Props["calls_in_scaling_loop_depth"].([]int)
	if !reflect.DeepEqual(calls, []string{"m.C.store", "m.C.pair"}) || !reflect.DeepEqual(depths, []int{1, 2}) {
		t.Errorf("calls = %v, depths = %v; want [m.C.store m.C.pair] at [1 2]", calls, depths)
	}
}
