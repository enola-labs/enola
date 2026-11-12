package facts

import (
	"fmt"
	"reflect"
	"testing"
)

// OfKind yields exactly what ByKind returns, in the same order, without the copy.
func TestOfKind_YieldsWhatByKindReturns(t *testing.T) {
	s := NewStore()
	for i := 0; i < 10; i++ {
		kind := KindSymbol
		if i%3 == 0 {
			kind = KindModule
		}
		s.Add(Fact{Kind: kind, Name: fmt.Sprintf("n%d", i), File: "a.go", Props: map[string]any{"i": i}})
	}
	for _, kind := range []string{KindSymbol, KindModule, KindRoute} {
		var got []Fact
		for f := range s.OfKind(kind) {
			got = append(got, f)
		}
		if want := s.ByKind(kind); !reflect.DeepEqual(got, want) && (len(got) != 0 || len(want) != 0) {
			t.Fatalf("OfKind(%s) yielded %v, ByKind returns %v", kind, got, want)
		}
	}

	n := 0
	for range s.OfKind(KindSymbol) {
		n++
		if n == 2 {
			break
		}
	}
	if n != 2 {
		t.Fatalf("break after 2 yielded %d", n)
	}
}

// Facts added while iterating are not yielded, as they are not in a ByKind copy.
func TestOfKind_ReadsTheFactsAsIterationBegan(t *testing.T) {
	s := NewStore()
	s.Add(Fact{Kind: KindSymbol, Name: "a"}, Fact{Kind: KindSymbol, Name: "b"})
	seen := 0
	for range s.OfKind(KindSymbol) {
		seen++
		s.Add(Fact{Kind: KindSymbol, Name: fmt.Sprintf("added%d", seen)})
	}
	if seen != 2 {
		t.Fatalf("yielded %d facts; facts added during the loop must not be", seen)
	}
}
