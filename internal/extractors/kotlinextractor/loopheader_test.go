package kotlinextractor

import "testing"

// A for statement evaluates its collection once, before the first iteration.
func TestKtForCollectionIsNotPerIteration(t *testing.T) {
	ff := extractAST(t, "fun load(): List<Int> = emptyList()\nfun use(x: Int) {}\nfun r() {\n for (x in load()) { use(x) }\n}", false)
	f, _ := findFact(ff, "pkg.r")
	cil := kStrSlice(f, "calls_in_loop")
	if kContains(cil, "pkg.load") {
		t.Errorf("calls_in_loop = %v: load() is the for collection, evaluated once", cil)
	}
	if !kContains(cil, "pkg.use") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.use", cil)
	}
}
