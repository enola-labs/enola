package phpextractor

import "testing"

// A for loop is fixed-count only when it both starts and stops at a constant.
func TestPHPDescendingLoopToALiteralStillScales(t *testing.T) {
	depth := func(body string) int {
		f := symbolsByName(extractFileAST([]byte("<?php\nfunction r($xs) {\n"+body+"\n}\n"), "x.php"))["r"]
		d, _ := f.Props["scaling_loop_depth"].(int)
		return d
	}
	if got := depth(`for ($i = count($xs) - 1; $i >= 0; $i--) { use_it($xs[$i]); }`); got != 1 {
		t.Errorf("descending from a count: scaling_loop_depth = %d, want 1", got)
	}
	if got := depth(`for (; $i > 0; $i--) { use_it($i); }`); got != 1 {
		t.Errorf("no initialiser: scaling_loop_depth = %d, want 1", got)
	}
	if got := depth(`for ($i = 0; $i < 3; $i++) { use_it($i); }`); got != 0 {
		t.Errorf("literal to literal: scaling_loop_depth = %d, want 0", got)
	}
	if got := depth(`for ($i = 7; $i >= 0; $i--) { use_it($i); }`); got != 0 {
		t.Errorf("descending between two literals: scaling_loop_depth = %d, want 0", got)
	}
}
