package javaextractor

import "testing"

// A for-each evaluates its collection once, before the first iteration.
func TestJavaForEachCollectionIsNotPerIteration(t *testing.T) {
	f := javaClass(t, "void r() {\n  for (Item x : load()) { process(x); }\n}\njava.util.List<Item> load() { return null; }\nvoid process(Item x) {}", "r")
	cil := jStrSlice(f, "calls_in_loop")
	if jContains(cil, "m.C.load") {
		t.Errorf("calls_in_loop = %v: load() is the for-each collection, evaluated once", cil)
	}
	if !jContains(cil, "m.C.process") {
		t.Errorf("calls_in_loop = %v, want to contain m.C.process", cil)
	}
}

// Map.computeIfAbsent runs its lambda at most once, for the one key it was given.
func TestJavaComputeIfAbsentIsNotALoop(t *testing.T) {
	f := javaClass(t, "void r(java.util.List<Item> xs, java.util.Map<String, java.util.List<Item>> m) {\n  for (Item x : xs) { m.computeIfAbsent(x.key(), k -> new java.util.ArrayList<>()).add(x); }\n}", "r")
	if got := jIntProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1", got)
	}
}
