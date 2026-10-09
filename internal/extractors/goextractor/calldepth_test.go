package goextractor

import (
	"reflect"
	"testing"
)

// The call sits at depth 1; the function's deepest nest, further down, is 2.
func TestGoCallDepthIsTheNestingAroundTheCall(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/x.go": `package pkg

func save(int) {}
func add(int, int) {}

func r(rows []int, xs, ys []int) {
	for _, row := range rows {
		save(row)
	}
	for _, a := range xs {
		for _, b := range ys {
			add(a, b)
		}
	}
}
`})
	f, _ := findFact(ff, "pkg.r")
	calls := strSliceProp(f, "calls_in_scaling_loop")
	depths, _ := f.Props["calls_in_scaling_loop_depth"].([]int)
	if !reflect.DeepEqual(calls, []string{"pkg.save", "pkg.add"}) || !reflect.DeepEqual(depths, []int{1, 2}) {
		t.Errorf("calls = %v, depths = %v; want [pkg.save pkg.add] at [1 2]", calls, depths)
	}
	if got := intProp(t, f, "scaling_loop_depth"); got != 2 {
		t.Errorf("scaling_loop_depth = %d, want the function-wide 2", got)
	}
}
