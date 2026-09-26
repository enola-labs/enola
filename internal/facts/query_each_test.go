package facts

import (
	"fmt"
	"testing"
)

// TestQueryEach_VisitsWhatQueryAdvancedReturns: QueryEach is the copy-free path summary
// mode counts through, so it must see exactly the matches QueryAdvanced pages over, in
// the same order and under the same total, offset included.
func TestQueryEach_VisitsWhatQueryAdvancedReturns(t *testing.T) {
	s := NewStore()
	for i := 0; i < 1500; i++ {
		kind := KindSymbol
		if i%3 == 0 {
			kind = KindRoute
		}
		s.Add(Fact{Kind: kind, Name: fmt.Sprintf("n%d", i), File: fmt.Sprintf("f%d.go", i%7)})
	}
	for _, opts := range []QueryOpts{
		{Kind: KindRoute},
		{Name: "n1"},
		{FilePrefix: "f3"},
		{Kind: KindSymbol, Offset: 400},
		{Names: []string{"n2", "n5", "n9"}},
	} {
		var want []string
		for off := opts.Offset; ; off += 500 {
			page := opts
			page.Offset, page.Limit = off, 500
			got, _ := s.QueryAdvanced(page)
			if len(got) == 0 {
				break
			}
			for _, f := range got {
				want = append(want, f.Name)
			}
		}
		_, wantTotal := s.QueryAdvanced(opts)

		var seen []string
		total := s.QueryEach(opts, func(f *Fact) { seen = append(seen, f.Name) })
		if total != wantTotal || fmt.Sprint(seen) != fmt.Sprint(want) {
			t.Errorf("%+v: QueryEach visited %d (total %d), QueryAdvanced pages %d (total %d)",
				opts, len(seen), total, len(want), wantTotal)
		}
	}
}
