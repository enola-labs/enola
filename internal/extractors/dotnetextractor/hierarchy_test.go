package dotnetextractor

import "testing"

func csScaling(t *testing.T, body string) (loop, scaling any) {
	t.Helper()
	ff := extractFileAST([]byte("namespace Acme;\npublic class C\n{\n    public void R(List<A> xs, List<B> ys, Dictionary<int, List<B>> byId)\n    {\n"+body+"\n    }\n}\n"), "src/C.cs")
	f := factByName(ff, "src.C.R")
	if f == nil {
		t.Fatalf("missing src.C.R")
	}
	return f.Props["loop_depth"], f.Props["scaling_loop_depth"]
}

func TestCSharpHierarchicalLoopAddsNoScalingDepth(t *testing.T) {
	cases := map[string]string{
		"member of the element": "foreach (var p in xs) { foreach (var o in p.Value.Operations) { Use(o); } }",
		"method of the element": "foreach (var p in xs) { foreach (var o in p.GetOperations()) { Use(o); } }",
		"derived local":         "foreach (var p in xs) { var ops = p.Operations; foreach (var o in ops) { Use(o); } }",
		"the element itself":    "foreach (var row in xs) { foreach (var cell in row) { Use(cell); } }",
		"keyed lookup":          "foreach (var key in xs) { foreach (var o in byId[key]) { Use(o); } }",
		"deconstructed element": "foreach (var (key, rows) in xs) { foreach (var row in rows) { Use(row); } }",
	}
	for name, body := range cases {
		loop, scaling := csScaling(t, body)
		if loop != 2 {
			t.Errorf("%s: loop_depth = %v, want 2", name, loop)
		}
		if scaling != 1 {
			t.Errorf("%s: scaling_loop_depth = %v, want 1", name, scaling)
		}
	}
}

func TestCSharpIndependentLoopsStillMultiply(t *testing.T) {
	cases := map[string]string{
		"all pairs":               "foreach (var a in xs) { foreach (var b in xs) { Cmp(a, b); } }",
		"two collections":         "foreach (var a in xs) { foreach (var b in ys) { Cmp(a, b); } }",
		"function of the element": "foreach (var a in xs) { foreach (var b in CandidatesFor(a)) { Cmp(a, b); } }",
		"method with arguments":   "foreach (var t in xs) { foreach (var r in t.FlatRelatives(false)) { Use(r); } }",
		"filtered by the element": "foreach (var a in xs) { foreach (var b in ys.Where(y => y.Owner == a.Id)) { Use(b); } }",
	}
	for name, body := range cases {
		if _, scaling := csScaling(t, body); scaling != 2 {
			t.Errorf("%s: scaling_loop_depth = %v, want 2", name, scaling)
		}
	}
}
