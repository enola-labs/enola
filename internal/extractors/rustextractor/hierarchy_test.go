package rustextractor

import "testing"

func rustScaling(t *testing.T, body string) (loop, scaling int) {
	t.Helper()
	f, ok := findFact(extractAST(t, "fn r(xs: &[A], ys: &[B]) {\n"+body+"\n}\n"), "pkg.r")
	if !ok {
		t.Fatalf("missing pkg.r")
	}
	loop, _ = f.Props["loop_depth"].(int)
	scaling, _ = f.Props["scaling_loop_depth"].(int)
	return loop, scaling
}

func TestRustHierarchicalLoopAddsNoScalingDepth(t *testing.T) {
	cases := map[string]string{
		"method of the element": "for trace in xs { for frame in trace.frames() { touch(frame); } }",
		"field of the element":  "for node in xs { for kid in &node.kids { touch(kid); } }",
		"iterator of a field":   "for node in xs { for kid in node.kids.iter() { touch(kid); } }",
		"while let on a method": "for trace in xs { while let Some(frame) = trace.next() { touch(frame); } }",
		"derived local":         "for node in xs { let kids = &node.kids; for kid in kids { touch(kid); } }",
		"destructured element":  "for (key, rows) in xs { for row in rows { touch(row); } }",
		"selected by index":     "for i in 0..xs.len() { for cell in &xs[i] { touch(cell); } }",
		"three levels":          "for a in xs { for b in &a.kids { for c in &b.kids { touch(c); } } }",
	}
	for name, body := range cases {
		loop, scaling := rustScaling(t, body)
		if loop < 2 {
			t.Errorf("%s: loop_depth = %d, want the lexical nesting kept", name, loop)
		}
		if scaling != 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want 1", name, scaling)
		}
	}
}

func TestRustIndependentLoopsStillMultiply(t *testing.T) {
	cases := map[string]string{
		"all pairs":               "for a in xs { for b in xs { cmp(a, b); } }",
		"two collections":         "for a in xs { for b in ys { cmp(a, b); } }",
		"slice from the index":    "for i in 0..xs.len() { for b in &xs[i + 1..] { cmp(&xs[i], b); } }",
		"function of the element": "for a in xs { for b in candidates_for(a) { cmp(a, b); } }",
		"method with arguments":   "for t in xs { for r in t.flat_relatives(false) { touch(r); } }",
		"filtered by the element": "for a in xs { for b in ys.iter().filter(|y| y.owner == a.id) { touch(b); } }",
	}
	for name, body := range cases {
		if _, scaling := rustScaling(t, body); scaling < 2 {
			t.Errorf("%s: scaling_loop_depth = %d, want >= 2", name, scaling)
		}
	}
}
