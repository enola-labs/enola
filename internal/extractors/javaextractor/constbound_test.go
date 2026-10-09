package javaextractor

import "testing"

func TestJavaEnumAndNamedBoundLoopsAddNoScalingDepth(t *testing.T) {
	cases := map[string]string{
		"enum values":          "for (A x : xs) { for (Scope s : Scope.values()) { use(x, s); } }",
		"named constant bound": "for (A x : xs) { for (int i = 0; i < MAX_ATTEMPTS; i++) { use(x, i); } }",
		"qualified constant":   "for (A x : xs) { for (int i = 0; i < Limits.MAX_ATTEMPTS; i++) { use(x, i); } }",
	}
	for name, body := range cases {
		loop, scaling := javaScaling(t, body)
		if loop != 2 {
			t.Errorf("%s: loop_depth = %d, want 2", name, loop)
		}
		if scaling != 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want 1", name, scaling)
		}
	}
}

func TestJavaLoopsThatOnlyLookConstantStillScale(t *testing.T) {
	cases := map[string]string{
		"map values":             "for (A x : xs) { for (B v : byId.values()) { use(x, v); } }",
		"variable bound":         "for (A x : xs) { for (int i = 0; i < limit; i++) { use(x, i); } }",
		"descending from a size": "for (A x : xs) { for (int i = ys.size() - 1; i >= 0; i--) { use(x, i); } }",
		"values with args":       "for (A x : xs) { for (B v : Scope.values(x)) { use(x, v); } }",
	}
	for name, body := range cases {
		if _, scaling := javaScaling(t, body); scaling != 2 {
			t.Errorf("%s: scaling_loop_depth = %d, want 2", name, scaling)
		}
	}
}
