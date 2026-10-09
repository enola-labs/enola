package phpextractor

import (
	"reflect"
	"testing"
)

// The call sits at depth 1; the function's deepest nest, further down, is 2.
func TestPHPCallDepthIsTheNestingAroundTheCall(t *testing.T) {
	f := symbolsByName(extractFileAST([]byte(`<?php
function r($rows, $xs, $ys) {
    foreach ($rows as $row) { save_it($row); }
    foreach ($xs as $a) { foreach ($ys as $b) { add_it($a, $b); } }
}
`), "x.php"))["r"]
	calls, _ := f.Props["calls_in_scaling_loop"].([]string)
	depths, _ := f.Props["calls_in_scaling_loop_depth"].([]int)
	if !reflect.DeepEqual(calls, []string{"save_it", "add_it"}) || !reflect.DeepEqual(depths, []int{1, 2}) {
		t.Errorf("calls = %v, depths = %v; want [save_it add_it] at [1 2]", calls, depths)
	}
}
