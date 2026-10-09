package phpextractor

import "testing"

// A foreach evaluates its collection once, before the first iteration.
func TestPHPForeachCollectionIsNotPerIteration(t *testing.T) {
	f := symbolsByName(extractFileAST([]byte(`<?php
function r($a, $b) { foreach (load_all($a, $b) as $x) { use_it($x); } }
`), "x.php"))["r"]
	calls, _ := f.Props["calls_in_loop"].([]string)
	has := func(name string) bool {
		for _, c := range calls {
			if c == name {
				return true
			}
		}
		return false
	}
	if has("load_all") {
		t.Errorf("calls_in_loop = %v: load_all() is the foreach collection, evaluated once", calls)
	}
	if !has("use_it") {
		t.Errorf("calls_in_loop = %v, want to contain use_it", calls)
	}
}
