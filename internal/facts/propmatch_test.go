package facts

import "testing"

func TestPropMatchesComparesVersionsNumbersAndLists(t *testing.T) {
	for _, c := range []struct {
		v        any
		op, want string
		match    bool
	}{
		{"0.138.0", PropOpLt, "0.150", true},
		{"0.154.0", PropOpLt, "0.150", false},
		{"0.150.0", PropOpGe, "0.150", true},
		{"v1.9", PropOpLt, "v1.10", true},
		{"1.2.3-rc1", PropOpEq, "1.2.3-rc1", true},
		{"1.2.3-rc1", PropOpLe, "1.2.3", true},
		{13, PropOpGt, "9", true},
		{"9", PropOpGt, "13", false},
		{"apple", PropOpLt, "banana", true},
		{"common-deployment", PropOpPrefix, "common-", true},
		{"common-deployment", "", "common-deployment", true},
		{"common-deployment", PropOpNe, "common-deployment", false},
		{[]string{"A=1", "CART_URL=http://cart"}, PropOpContains, "cart", true},
		{[]any{"a", "b"}, PropOpEq, "b", true},
		{[]any{"a", "b"}, PropOpEq, "[a b]", true},
		{[]string{"a"}, PropOpEq, "c", false},
	} {
		if got := PropMatches(c.v, c.op, c.want); got != c.match {
			t.Errorf("PropMatches(%v, %q, %q) = %v, want %v", c.v, c.op, c.want, got, c.match)
		}
	}
	if ValidPropOp("between") || !ValidPropOp("") || !ValidPropOp(PropOpGe) {
		t.Error("ValidPropOp")
	}
}

func TestQueryFiltersPropsByRange(t *testing.T) {
	s := NewStore()
	for _, v := range []string{"0.110.0", "0.138.0", "0.150.0", "0.154.0"} {
		s.Add(Fact{Kind: KindSymbol, Name: "app-" + v, Props: map[string]any{"chart_version": v}})
	}
	got, total := s.QueryAdvanced(QueryOpts{Kind: KindSymbol, Prop: "chart_version", PropOp: PropOpLt, PropValue: "0.150"})
	if total != 2 || len(got) != 2 {
		t.Fatalf("got %d (total %d), want the two older", len(got), total)
	}
}
