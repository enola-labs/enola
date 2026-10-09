package kotlinextractor

import "testing"

func ktScaling(t *testing.T, body string) (loop, scaling int) {
	t.Helper()
	ff := extractAST(t, "fun r(xs: List<A>, ys: List<B>, byId: Map<Int, List<B>>) {\n"+body+"\n}", false)
	f, ok := findFact(ff, "pkg.r")
	if !ok {
		t.Fatalf("missing pkg.r")
	}
	return kIntProp(t, f, "loop_depth"), kIntProp(t, f, "scaling_loop_depth")
}

func TestKtHierarchicalLoopAddsNoScalingDepth(t *testing.T) {
	cases := map[string]string{
		"member of the element": "for (c in xs) { for (d in c.deps) { use(d) } }",
		"derived local":         "for (c in xs) { val deps = c.deps ?: emptyList()\n for (d in deps) { use(d) } }",
		"iterator lambdas":      "xs.forEach { c -> c.deps.forEach { d -> use(d) } }",
		"implicit it":           "xs.forEach { it.deps.map { d -> use(d) } }",
		"method of the element": "for (c in xs) { for (d in c.resolve().files) { use(d) } }",
		"keyed lookup":          "for (c in xs) { for (d in byId[c]!!) { use(d) } }",
		"keyed get":             "for (c in xs) { byId.getValue(c.id).forEach { d -> use(d) } }",
		"three levels":          "for (a in xs) { for (b in a.kids) { for (c in b.kids) { use(c) } } }",
	}
	for name, body := range cases {
		loop, scaling := ktScaling(t, body)
		if loop < 2 {
			t.Errorf("%s: loop_depth = %d, want the lexical nesting kept", name, loop)
		}
		if scaling != 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want 1", name, scaling)
		}
	}
}

func TestKtIndependentLoopsStillMultiply(t *testing.T) {
	cases := map[string]string{
		"all pairs":               "for (a in xs) { for (b in xs) { cmp(a, b) } }",
		"two collections":         "for (a in xs) { for (b in ys) { cmp(a, b) } }",
		"drop from the index":     "xs.forEachIndexed { i, a -> xs.drop(i + 1).forEach { b -> cmp(a, b) } }",
		"function of the element": "for (a in xs) { for (b in candidatesFor(a)) { cmp(a, b) } }",
		"method with arguments":   "for (t in xs) { for (r in t.flatRelatives(false)) { use(r) } }",
		"filtered by the element": "for (a in xs) { ys.filter { it.owner == a.id }.forEach { b -> use(b) } }",
	}
	for name, body := range cases {
		if _, scaling := ktScaling(t, body); scaling < 2 {
			t.Errorf("%s: scaling_loop_depth = %d, want >= 2", name, scaling)
		}
	}
}

func TestKtHierarchicalLoopStillRepeats(t *testing.T) {
	ff := extractAST(t, "fun save(d: B) {}\nfun r(xs: List<A>) {\n for (c in xs) { for (d in c.deps) { save(d) } }\n}", false)
	f, _ := findFact(ff, "pkg.r")
	if cil := kStrSlice(f, "calls_in_scaling_loop"); !kContains(cil, "pkg.save") {
		t.Errorf("calls_in_scaling_loop = %v, want to contain pkg.save", cil)
	}
}
