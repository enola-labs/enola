package tsextractor

import (
	"reflect"
	"testing"
)

// The call sits at depth 1; the function's deepest nest, further down, is 2.
func TestTsCallDepthIsTheNestingAroundTheCall(t *testing.T) {
	f := tsExtractFunc(t, `export function r(rows, xs, ys) {
  for (const row of rows) { save(row) }
  for (const a of xs) { for (const b of ys) { add(a, b) } }
}`, "src.r")
	calls := tsStrSlice(f, "calls_in_scaling_loop")
	depths, _ := f.Props["calls_in_scaling_loop_depth"].([]int)
	if !reflect.DeepEqual(calls, []string{"src.save", "src.add"}) || !reflect.DeepEqual(depths, []int{1, 2}) {
		t.Errorf("calls = %v, depths = %v; want [src.save src.add] at [1 2]", calls, depths)
	}
}
