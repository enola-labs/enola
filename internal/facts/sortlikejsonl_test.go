package facts

import (
	"bytes"
	"fmt"
	"slices"
	"testing"
)

// A store built in extraction order and sorted with SortLikeJSONL lists its facts as a
// store read back from its own facts.jsonl does, so an offset-paged query answers alike
// on a generated graph and a restored one. Its indexes and graph still find each fact.
func TestSortLikeJSONL_MatchesARestoredStore(t *testing.T) {
	const n = 300
	s := NewStore()
	for i := 0; i < n; i++ {
		j := (i * 7) % n // distinct, in no sorted order
		s.Add(Fact{Kind: KindSymbol, Name: fmt.Sprintf("pkg.f%03d", j), File: fmt.Sprintf("f%d.go", j%11), Repo: "r",
			Relations: []Relation{{Kind: RelCalls, Target: fmt.Sprintf("pkg.f%03d", (j+1)%n)}}})
		if j%3 == 0 {
			// Shared names reach byNameDup and a node with several declaring facts.
			s.Add(Fact{Kind: KindModule, Name: fmt.Sprintf("pkg.f%03d", j), File: fmt.Sprintf("m%d.go", j%5), Repo: "r"})
		}
	}
	s.BuildGraph()
	s.Freeze()

	var buf bytes.Buffer
	if err := s.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	restored := NewStore()
	if err := restored.ReadJSONL(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := s.SortLikeJSONL(); err != nil {
		t.Fatal(err)
	}

	names := func(ff []Fact) []string {
		out := make([]string, len(ff))
		for i, f := range ff {
			out[i] = f.Kind + " " + f.Name + " " + f.File
		}
		return out
	}
	for _, opts := range []QueryOpts{{Limit: 500}, {Kind: KindSymbol, Limit: 40, Offset: 80}, {File: "f3.go", Limit: 500}, {Repo: "r", Limit: 50, Offset: 250}} {
		got, _ := s.QueryAdvanced(opts)
		want, _ := restored.QueryAdvanced(opts)
		if !slices.Equal(names(got), names(want)) {
			t.Errorf("query %+v: sorted store answers\n %v\nrestored store answers\n %v", opts, names(got), names(want))
		}
	}

	var again bytes.Buffer
	if err := s.WriteJSONL(&again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Bytes(), buf.Bytes()) {
		t.Error("sorting the store changed what it writes")
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("pkg.f%03d", i)
		byName := s.ByName(name)
		want := 1
		if i%3 == 0 {
			want = 2
		}
		if len(byName) != want {
			t.Fatalf("ByName(%s) = %d facts, want %d", name, len(byName), want)
		}
		for _, f := range byName {
			if f.Name != name {
				t.Fatalf("ByName(%s) returned %s", name, f.Name)
			}
		}
		callers := s.Graph().ReverseFacts(name, RelCalls)
		if len(callers) != 1 || callers[0].Name != fmt.Sprintf("pkg.f%03d", (i+n-1)%n) || callers[0].Kind != KindSymbol {
			t.Fatalf("callers of %s = %v", name, names(callers))
		}
	}
}
