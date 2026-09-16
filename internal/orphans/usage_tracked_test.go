package orphans

import "testing"

// TestUsageTracked pins the reliability predicate the dead-weight gate relies on:
// high (function calls) and medium (class/interface instantiate/inject/implement)
// have edge-tracked usage → reliable; low (method/type/const/var) does not.
func TestUsageTracked(t *testing.T) {
	cases := []struct {
		conf string
		want bool
	}{
		{confHigh, true},
		{confMedium, true},
		{confLow, false},
	}
	for _, c := range cases {
		if got := (Orphan{Confidence: c.conf}).UsageTracked(); got != c.want {
			t.Errorf("UsageTracked(%q) = %v, want %v", c.conf, got, c.want)
		}
	}
}
