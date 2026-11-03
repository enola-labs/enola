package facts

import (
	"encoding/json"
	"slices"
	"testing"
)

// A nested function goes on the innermost symbol holding it. One with a symbol's lines is
// that symbol, and one no symbol holds goes nowhere. The list survives JSON.
func TestAttachNested(t *testing.T) {
	ff := []Fact{
		{Kind: KindSymbol, Name: "Outer", File: "a.go", Line: 1, EndLine: 20},
		{Kind: KindSymbol, Name: "Outer.member", File: "a.go", Line: 5, EndLine: 10},
		{Kind: KindSymbol, Name: "Other", File: "b.go", Line: 1, EndLine: 20},
	}
	AttachNested(ff, "a.go", []Nested{
		{Name: "inMember", Line: 6, EndLine: 7},
		{Name: "member", Line: 5, EndLine: 10},
		{Name: "inOuter", Line: 12, EndLine: 14},
		{Name: "outside", Line: 30, EndLine: 31},
	})
	names := func(f Fact) []string {
		var out []string
		for _, n := range NestedOf(&f) {
			out = append(out, n.Name)
		}
		return out
	}
	if got := names(ff[0]); !slices.Equal(got, []string{"inOuter"}) {
		t.Errorf("Outer: %v", got)
	}
	if got := names(ff[1]); !slices.Equal(got, []string{"inMember"}) {
		t.Errorf("Outer.member: %v", got)
	}
	if got := names(ff[2]); got != nil {
		t.Errorf("a symbol of another file was given %v", got)
	}

	data, err := json.Marshal(ff[0])
	if err != nil {
		t.Fatal(err)
	}
	var back Fact
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if got := NestedOf(&back); len(got) != 1 || got[0] != (Nested{Name: "inOuter", Line: 12, EndLine: 14}) {
		t.Errorf("read back: %+v", got)
	}
}
