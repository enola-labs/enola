package dotnetextractor

import "testing"

// A for loop is fixed-count only when it both starts and stops at a constant.
func TestCSharpDescendingLoopToALiteralStillScales(t *testing.T) {
	depth := func(body string) any {
		ff := extractFileAST([]byte("namespace Acme;\npublic class C\n{\n    public void R(List<int> xs, int n)\n    {\n"+body+"\n    }\n}\n"), "src/C.cs")
		f := factByName(ff, "src.C.R")
		if f == nil {
			t.Fatalf("missing src.C.R")
		}
		return f.Props["scaling_loop_depth"]
	}
	if got := depth("for (var i = xs.Count - 1; i >= 0; i--) { Use(xs[i]); }"); got != 1 {
		t.Errorf("descending from a count: scaling_loop_depth = %v, want 1", got)
	}
	if got := depth("int k; for (k = n; k > 0; --k) { Use(k); }"); got != 1 {
		t.Errorf("assigned from a variable: scaling_loop_depth = %v, want 1", got)
	}
	if got := depth("for (int i = 0; i < 3; i++) { Use(i); }"); got != 0 {
		t.Errorf("literal to literal: scaling_loop_depth = %v, want 0", got)
	}
	if got := depth("for (int i = 7; i >= 0; i--) { Use(i); }"); got != 0 {
		t.Errorf("descending between two literals: scaling_loop_depth = %v, want 0", got)
	}
}
