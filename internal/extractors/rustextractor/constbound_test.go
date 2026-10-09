package rustextractor

import "testing"

func TestRustConstantLoopsAddNoScalingDepth(t *testing.T) {
	cases := map[string]string{
		"range to a constant":   "for x in xs { for _ in 0..CHUNK_SIZE { touch(x); } }",
		"single-letter const":   "for x in xs { for _ in 0..N { touch(x); } }",
		"scoped constant":       "for x in xs { for _ in 0..config::CHUNK_SIZE { touch(x); } }",
		"constant table":        "for x in xs { for row in TABLE { touch(row); } }",
		"reference to a table":  "for x in xs { for row in &TABLE { touch(row); } }",
		"iterator over a table": "for x in xs { for row in TABLE.iter() { touch(row); } }",
	}
	for name, body := range cases {
		loop, scaling := rustScaling(t, body)
		if loop != 2 {
			t.Errorf("%s: loop_depth = %d, want 2", name, loop)
		}
		if scaling != 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want 1", name, scaling)
		}
	}
}

func TestRustLoopsThatOnlyLookConstantStillScale(t *testing.T) {
	cases := map[string]string{
		"range to a variable":      "for x in xs { for _ in 0..n { touch(x); } }",
		"range to a length":        "for x in xs { for i in 0..ys.len() { touch(i); } }",
		"lower-case collection":    "for x in xs { for row in table { touch(row); } }",
		"adaptor taking arguments": "for x in xs { for row in TABLE.matching(x) { touch(row); } }",
	}
	for name, body := range cases {
		if _, scaling := rustScaling(t, body); scaling != 2 {
			t.Errorf("%s: scaling_loop_depth = %d, want 2", name, scaling)
		}
	}
}
