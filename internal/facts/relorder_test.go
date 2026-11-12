package facts

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// A published graph holds each fact's relations in the order a restore of it does, so
// a tool answers the same whichever way the graph got there. Extractors append in
// map-iteration order; facts.jsonl is written sorted.
func TestFreezeSortsRelationsAsARestoreHoldsThem(t *testing.T) {
	unsorted := []Relation{
		{Kind: RelDeclares, Target: "pkg"},
		{Kind: RelCalls, Target: "pkg.Z"},
		{Kind: RelCalls, Target: "pkg.A"},
	}
	s := NewStore()
	// Two facts holding one slice, as a fact copied by value does.
	s.Add(Fact{Kind: KindSymbol, Name: "pkg.F", File: "f.go", Line: 1, Relations: unsorted})
	s.Add(Fact{Kind: KindSymbol, Name: "pkg.G", File: "f.go", Line: 2, Relations: unsorted})
	s.Add(Fact{Kind: KindSymbol, Name: "pkg.H", File: "f.go", Line: 3, Relations: []Relation{
		{Kind: RelImports, Target: "b"}, {Kind: RelImports, Target: "a"}, {Kind: RelImports, Target: "a"},
	}})
	s.Freeze()

	built := map[string][]Relation{}
	for _, f := range s.FactsRef() {
		if !slices.IsSortedFunc(f.Relations, compareRelations) {
			t.Errorf("%s: relations not sorted after Freeze: %v", f.Name, f.Relations)
		}
		built[f.Name] = f.Relations
	}

	path := filepath.Join(t.TempDir(), "facts.jsonl")
	if err := s.WriteJSONLFile(path); err != nil {
		t.Fatal(err)
	}
	r := NewStore()
	if err := r.ReadJSONLFileShared(path); err != nil {
		t.Fatal(err)
	}
	r.Freeze()
	for _, f := range r.FactsRef() {
		if !reflect.DeepEqual(f.Relations, built[f.Name]) {
			t.Errorf("%s: built %v, restored %v", f.Name, built[f.Name], f.Relations)
		}
	}
}

// A slice already in order is never written, so one a published store shares with
// this one is only read.
func TestSortRelationsLeavesASortedSliceAlone(t *testing.T) {
	rs := []Relation{{Kind: RelCalls, Target: "a"}, {Kind: RelCalls, Target: "a"}, {Kind: RelDeclares, Target: "p"}}
	before := slices.Clone(rs)
	if n := testing.AllocsPerRun(100, func() { sortRelations(rs) }); n != 0 {
		t.Errorf("sortRelations allocated %v times on a sorted slice", n)
	}
	if !reflect.DeepEqual(rs, before) {
		t.Errorf("sorted slice changed: %v", rs)
	}
}
