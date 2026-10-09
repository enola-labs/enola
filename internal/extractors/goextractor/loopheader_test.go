package goextractor

import "testing"

// A range evaluates its operand once, before the first iteration.
func TestRangeOperandIsNotCalledPerIteration(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/x.go": `package pkg

func load() []int { return nil }
func use(int)     {}

func r() {
	for _, x := range load() {
		use(x)
	}
}
`})
	f, ok := findFact(ff, "pkg.r")
	if !ok {
		t.Fatalf("missing pkg.r")
	}
	cil := strSliceProp(f, "calls_in_loop")
	if containsStr(cil, "pkg.load") {
		t.Errorf("calls_in_loop = %v: load() is the range operand, evaluated once", cil)
	}
	if !containsStr(cil, "pkg.use") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.use", cil)
	}
}
