package cppextractor

import "testing"

// A for loop is fixed-count only when it both starts and stops at a constant.
func TestCppDescendingLoopToALiteralStillScales(t *testing.T) {
	scales := map[string]string{
		"descending from a variable": "void run(int n) {\n  for (int i = n - 1; i >= 0; i--) { db_query(); }\n}",
		"assigned from a variable":   "void run(int n) {\n  int k;\n  for (k = n; k > 0; --k) { db_query(); }\n}",
		"no initialiser":             "void run(int i) {\n  for (; i > 0; i--) { db_query(); }\n}",
	}
	for name, body := range scales {
		f := cppLoopFn(t, body)
		if got := intProp(t, f, "scaling_loop_depth"); got != 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want 1", name, got)
		}
		if calls, _ := f.Props["calls_in_scaling_loop"].([]string); len(calls) == 0 {
			t.Errorf("%s: calls_in_scaling_loop is empty; the call runs once per element", name)
		}
	}
	constant := map[string]string{
		"literal to literal":       "void run() {\n  for (int i = 0; i < 3; i++) { work(); }\n}",
		"descending between two":   "void run() {\n  for (int i = 7; i >= 0; i--) { work(); }\n}",
		"two counters, both fixed": "void run() {\n  for (int a = 0, b = 1; a < 4; a++) { work(); }\n}",
	}
	for name, body := range constant {
		if got := intProp(t, cppLoopFn(t, body), "scaling_loop_depth"); got != 0 {
			t.Errorf("%s: scaling_loop_depth = %d, want 0", name, got)
		}
	}
}
