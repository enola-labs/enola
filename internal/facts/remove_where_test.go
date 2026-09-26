package facts

import (
	"fmt"
	"testing"
)

// TestRemoveWhere_KeepsOrderAndIndicesAtEveryEdge: RemoveWhere now finds its first
// match before copying anything, so the edges of that search are where it can go
// wrong: nothing matching, the first fact, the last, and every fact.
func TestRemoveWhere_KeepsOrderAndIndicesAtEveryEdge(t *testing.T) {
	build := func() *Store {
		s := NewStore()
		for i := 0; i < 6; i++ {
			s.Add(Fact{Kind: KindSymbol, Name: fmt.Sprintf("s%d", i), File: fmt.Sprintf("f%d.go", i%2)})
		}
		return s
	}
	for name, tc := range map[string]struct {
		drop    func(Fact) bool
		removed int
		want    string
	}{
		"none":  {func(Fact) bool { return false }, 0, "[s0 s1 s2 s3 s4 s5]"},
		"first": {func(f Fact) bool { return f.Name == "s0" }, 1, "[s1 s2 s3 s4 s5]"},
		"last":  {func(f Fact) bool { return f.Name == "s5" }, 1, "[s0 s1 s2 s3 s4]"},
		"odd":   {func(f Fact) bool { return f.File == "f1.go" }, 3, "[s0 s2 s4]"},
		"all":   {func(Fact) bool { return true }, 6, "[]"},
	} {
		s := build()
		if got := s.RemoveWhere(tc.drop); got != tc.removed {
			t.Errorf("%s: removed %d, want %d", name, got, tc.removed)
		}
		var names []string
		for _, f := range s.All() {
			names = append(names, f.Name)
		}
		if got := fmt.Sprint(names); got != tc.want {
			t.Errorf("%s: kept %s, want %s", name, got, tc.want)
		}
		// The indices must describe the survivors.
		got, total := s.QueryAdvanced(QueryOpts{File: "f0.go", Limit: 500})
		for _, f := range got {
			if f.File != "f0.go" {
				t.Errorf("%s: file index returned %s from %s", name, f.Name, f.File)
			}
		}
		if total != len(got) {
			t.Errorf("%s: file index total %d, returned %d", name, total, len(got))
		}
	}
}
