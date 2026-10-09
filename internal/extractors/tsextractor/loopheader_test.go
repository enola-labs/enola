package tsextractor

import "testing"

// What a loop draws from is evaluated once, before the first iteration.
func TestTsLoopHeaderIsNotPerIteration(t *testing.T) {
	f := tsExtractFunc(t, `export function r(xs) {
  for (const s of load()) { use(s) }
}`, "src.r")
	cil := tsStrSlice(f, "calls_in_loop")
	if tsContains(cil, "src.load") {
		t.Errorf("calls_in_loop = %v: load() is the for..of collection, evaluated once", cil)
	}
	if !tsContains(cil, "src.use") {
		t.Errorf("calls_in_loop = %v, want to contain src.use", cil)
	}

	// An iterator in the header is a loop beside this one, not inside it.
	f = tsExtractFunc(t, `export function r(xs, ys) {
  for (const x of xs.filter(a => ys.find(b => b.id === a.id))) { use(x) }
}`, "src.r")
	if got := tsIntProp(t, f, "loop_depth"); got != 2 {
		t.Errorf("loop_depth = %d, want 2 (filter > find; the for..of sits beside them)", got)
	}
}
