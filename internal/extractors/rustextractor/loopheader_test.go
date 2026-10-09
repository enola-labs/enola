package rustextractor

import "testing"

// A `for` evaluates what it iterates once, before the first iteration.
func TestRustForIterableIsNotPerIteration(t *testing.T) {
	f, _ := findFact(extractAST(t, `
fn load() -> Vec<u32> { vec![] }
fn touch(_x: u32) {}
fn r() {
    for x in load() { touch(x); }
}
`), "pkg.r")
	calls, _ := f.Props["calls_in_loop"].([]string)
	has := func(name string) bool {
		for _, c := range calls {
			if c == name {
				return true
			}
		}
		return false
	}
	if has("pkg.load") {
		t.Errorf("calls_in_loop = %v: load() is what the for iterates, evaluated once", calls)
	}
	if !has("pkg.touch") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.touch", calls)
	}
}
