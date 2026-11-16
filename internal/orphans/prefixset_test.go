package orphans

import (
	"strings"
	"testing"
)

// The set must answer exactly as a scan of the raw list does, duplicates, empty
// entries and overlapping prefixes included.
func TestPrefixSetMatchesLinearScan(t *testing.T) {
	raw := []string{"report_", "", "report_", "send_", "send_mail_", "a", "ab", "zz", "report"}
	names := []string{"", "a", "ab", "abc", "b", "report", "report_x", "repor", "send_", "send_mail_now",
		"send", "zz", "zzz", "z", "Report_x", "reportx"}
	scan := func(s string) bool {
		for _, p := range raw {
			if p != "" && strings.HasPrefix(s, p) {
				return true
			}
		}
		return false
	}
	ps := newPrefixSet(raw)
	for _, n := range names {
		if got, want := ps.matches(n), scan(n); got != want {
			t.Errorf("matches(%q) = %v, linear scan %v (set %q)", n, got, want, ps)
		}
	}
	if len(newPrefixSet([]string{"", ""})) != 0 {
		t.Error("only empty prefixes should make an empty set")
	}
}
