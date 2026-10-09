package cppextractor

import (
	"reflect"
	"testing"
)

// The call sits at depth 1; the function's deepest nest, further down, is 2.
func TestCppCallDepthIsTheNestingAroundTheCall(t *testing.T) {
	f := cppLoopFn(t, `void run(int n, int m) {
  for (int i = 0; i < n; i++) { db_query(); }
  for (int a = 0; a < n; a++) { for (int b = 0; b < m; b++) { work(); } }
}`)
	calls, _ := f.Props["calls_in_scaling_loop"].([]string)
	depths, _ := f.Props["calls_in_scaling_loop_depth"].([]int)
	if len(calls) != 2 || !reflect.DeepEqual(depths, []int{1, 2}) {
		t.Errorf("calls = %v, depths = %v; want db_query at 1 and work at 2", calls, depths)
	}
}
