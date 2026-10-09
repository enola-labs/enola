package pythonextractor

import (
	"reflect"
	"testing"
)

// The call sits at depth 1; the function's deepest nest, further down, is 2.
func TestPyCallDepthIsTheNestingAroundTheCall(t *testing.T) {
	f := byName(astExtract(t, "svc.py", `
def save(row):
    pass

def add(a, b):
    pass

def r(rows, xs, ys):
    for row in rows:
        save(row)
    for a in xs:
        for b in ys:
            add(a, b)
`, false))["svc.r"]
	calls, _ := f.Props["calls_in_scaling_loop"].([]string)
	depths, _ := f.Props["calls_in_scaling_loop_depth"].([]int)
	if !reflect.DeepEqual(calls, []string{"svc.save", "svc.add"}) || !reflect.DeepEqual(depths, []int{1, 2}) {
		t.Errorf("calls = %v, depths = %v; want [svc.save svc.add] at [1 2]", calls, depths)
	}
}
