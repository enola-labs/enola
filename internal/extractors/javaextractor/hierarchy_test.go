package javaextractor

import "testing"

func javaScaling(t *testing.T, body string) (loop, scaling int) {
	t.Helper()
	f := javaClass(t, "void r(java.util.List<A> xs, java.util.List<B> ys, java.util.Map<Integer, java.util.List<B>> byId) {\n"+body+"\n}", "r")
	return jIntProp(t, f, "loop_depth"), jIntProp(t, f, "scaling_loop_depth")
}

func TestJavaHierarchicalLoopAddsNoScalingDepth(t *testing.T) {
	cases := map[string]string{
		"method of the element": "for (A s : xs) { for (B p : s.getAllOf()) { use(p); } }",
		"field of the element":  "for (A s : xs) { for (B p : s.parts) { use(p); } }",
		"derived local":         "for (A s : xs) { List<B> parts = s.getAllOf(); for (B p : parts) { use(p); } }",
		"keyed lookup":          "for (A s : xs) { for (B p : byId.get(s.getId())) { use(p); } }",
		"three levels":          "for (A a : xs) { for (B b : a.kids()) { for (B c : b.kids()) { use(c); } } }",
	}
	for name, body := range cases {
		loop, scaling := javaScaling(t, body)
		if loop < 2 {
			t.Errorf("%s: loop_depth = %d, want the lexical nesting kept", name, loop)
		}
		if scaling != 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want 1", name, scaling)
		}
	}
}

func TestJavaIndependentLoopsStillMultiply(t *testing.T) {
	cases := map[string]string{
		"all pairs":               "for (A a : xs) { for (A b : xs) { cmp(a, b); } }",
		"two collections":         "for (A a : xs) { for (B b : ys) { cmp(a, b); } }",
		"sublist from the index":  "for (int i = 0; i < xs.size(); i++) { for (A b : xs.subList(i + 1, xs.size())) { cmp(xs.get(i), b); } }",
		"method with arguments":   "for (A t : xs) { for (B r : t.flatRelatives(false)) { use(r); } }",
		"function of the element": "for (A a : xs) { for (B b : candidatesFor(a)) { cmp(a, b); } }",
	}
	for name, body := range cases {
		if _, scaling := javaScaling(t, body); scaling < 2 {
			t.Errorf("%s: scaling_loop_depth = %d, want >= 2", name, scaling)
		}
	}
}
