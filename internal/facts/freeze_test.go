package facts

import (
	"fmt"
	"reflect"
	"testing"
)

// sameMap reports whether two maps are the same allocation. Sharing is invisible to
// ordinary comparison — that is the point — so identity is what has to be asserted.
func sameMap(a, b map[string]any) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

func sameRels(a, b []Relation) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	return &a[0] == &b[0]
}

// TestFreeze_SharesIdenticalProps — the case the whole change exists for. Extraction
// emits the same shape for every function it sees and cannot know it is a repeat.
func TestFreeze_SharesIdenticalProps(t *testing.T) {
	s := NewStore()
	for i := 0; i < 5; i++ {
		s.Add(Fact{Kind: KindSymbol, Name: fmt.Sprintf("F%d", i), File: "a.go",
			Props: map[string]any{"symbol_kind": "function", "exported": true, "cyclomatic": 1.0}})
	}
	// One fact with a different shape must NOT join them.
	s.Add(Fact{Kind: KindSymbol, Name: "Other", File: "a.go",
		Props: map[string]any{"symbol_kind": "struct", "exported": true, "cyclomatic": 1.0}})

	s.Freeze()
	ff := s.FactsRef()

	for i := 1; i < 5; i++ {
		if !sameMap(ff[0].Props, ff[i].Props) {
			t.Errorf("fact %d still holds its own Props map; identical shapes were not shared", i)
		}
	}
	if sameMap(ff[0].Props, ff[5].Props) {
		t.Error("facts with DIFFERENT props were given the same map")
	}
	// Sharing must not change what anything reads.
	if got := ff[3].PropString("symbol_kind"); got != "function" {
		t.Errorf("shared props read back as %q, want \"function\"", got)
	}
	if !ff[3].PropBool("exported") {
		t.Error("shared props lost the exported flag")
	}
}

// TestFreeze_SharesIdenticalRelations — the same for the other repeated field.
func TestFreeze_SharesIdenticalRelations(t *testing.T) {
	s := NewStore()
	mk := func(name string, targets ...string) Fact {
		f := Fact{Kind: KindSymbol, Name: name, File: "a.go"}
		for _, tg := range targets {
			f.Relations = append(f.Relations, Relation{Kind: RelCalls, Target: tg})
		}
		return f
	}
	s.Add(mk("A", "X", "Y"), mk("B", "X", "Y"), mk("C", "X"), mk("D", "Y", "X"))
	s.Freeze()
	ff := s.FactsRef()

	if !sameRels(ff[0].Relations, ff[1].Relations) {
		t.Error("identical relation slices were not shared")
	}
	if sameRels(ff[0].Relations, ff[2].Relations) {
		t.Error("a two-relation slice was shared with a one-relation slice")
	}
	// Order is part of the value: X,Y and Y,X are different edges lists.
	if sameRels(ff[0].Relations, ff[3].Relations) {
		t.Error("relation slices differing only in order were shared")
	}
}

// TestFreeze_SharedRelationsForceReallocationOnAppend — a shared slice must have no
// spare capacity, or an append by one holder silently rewrites what the others read.
// This is the failure that leaves no trace at the point it happens.
func TestFreeze_SharedRelationsForceReallocationOnAppend(t *testing.T) {
	s := NewStore()
	for _, n := range []string{"A", "B"} {
		rels := make([]Relation, 1, 8) // deliberate spare capacity
		rels[0] = Relation{Kind: RelCalls, Target: "X"}
		s.Add(Fact{Kind: KindSymbol, Name: n, File: "a.go", Relations: rels})
	}
	s.Freeze()
	ff := s.FactsRef()
	if !sameRels(ff[0].Relations, ff[1].Relations) {
		t.Fatal("identical relation slices were not shared")
	}

	shared := ff[0].Relations
	if got, want := cap(shared), len(shared); got != want {
		t.Errorf("shared slice has cap %d for len %d; an append would write into what other facts read", got, want)
	}

	// Prove it: appending through one holder must not be visible through the other.
	grown := append(shared, Relation{Kind: RelCalls, Target: "Z"})
	_ = grown
	if len(ff[1].Relations) != 1 || ff[1].Relations[0].Target != "X" {
		t.Errorf("appending through one holder changed another fact's relations: %+v", ff[1].Relations)
	}
}

// TestFreeze_EncodingIsInjective is the soundness property. A key collision means two
// facts silently start sharing one map, so values that differ must never encode the
// same — including the cases a naive fmt-based key gets wrong: a delimiter inside a
// string, and numerically equal values of different Go types.
func TestFreeze_EncodingIsInjective(t *testing.T) {
	distinct := []map[string]any{
		{"a": "x", "b": "y"},
		{"a": "x:y"},        // the length separator, inside a value
		{"a": "x", "b": ""}, // empty value vs absent key
		{"a": "x"},          //
		{"ab": "x"},         // key boundary
		{"a": "bx"},         //
		{"n": 1},            // int
		{"n": int64(1)},     // int64 — a consumer asserting v.(int) would break
		{"n": 1.0},          // float64
		{"n": "1"},          // string
		{"n": true},         //
		{"n": nil},          //
		{"l": []string{"a", "b"}},
		{"l": []string{"a b"}}, // the classic fmt collision
		{"l": []string{"ab"}},
		{"l": []any{"a", "b"}},          // different container type
		{"m": map[string]any{"k": "v"}}, //
		{"m": map[string]any{"k": "v", "extra": "x"}},   //
		{"s": []map[string]any{{"k": "v"}}},             //
		{"s": []map[string]any{{"k": "v"}, {"k": "v"}}}, //
		{"f": 0.1},                //
		{"f": 0.1000000000000001}, // must not round together
		{"f": float32(0.1)},       // different width
	}

	seen := map[string]int{}
	for i, m := range distinct {
		key, ok := appendProps(nil, m, &propKeys{})
		if !ok {
			t.Fatalf("case %d (%v) was rejected by the encoder", i, m)
		}
		if prev, dup := seen[string(key)]; dup {
			t.Errorf("cases %d (%v) and %d (%v) encode identically — they would be silently merged",
				prev, distinct[prev], i, m)
		}
		seen[string(key)] = i
	}
}

// TestFreeze_EqualValuesEncodeEqually — the other half: values that ARE equal must
// collide, or the dedup does nothing. Map iteration order in particular must not leak
// into the key.
func TestFreeze_EqualValuesEncodeEqually(t *testing.T) {
	a := map[string]any{"z": 1, "a": "x", "m": true, "b": 2.5, "q": []string{"p", "r"}}
	b := map[string]any{"q": []string{"p", "r"}, "b": 2.5, "a": "x", "z": 1, "m": true}

	for i := 0; i < 20; i++ { // repeat: Go randomizes map iteration order per range
		ka, okA := appendProps(nil, a, &propKeys{})
		kb, okB := appendProps(nil, b, &propKeys{})
		if !okA || !okB {
			t.Fatal("encoder rejected a supported value")
		}
		if string(ka) != string(kb) {
			t.Fatalf("equal maps encoded differently on attempt %d:\n %q\n %q", i, ka, kb)
		}
	}
}

// TestFreeze_RejectsUnknownValueTypes — an unencodable value must leave the fact
// alone. Refusing to share costs a few hundred bytes; guessing risks merging facts
// that differ in a way the encoder cannot see.
func TestFreeze_RejectsUnknownValueTypes(t *testing.T) {
	type custom struct{ N int }
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "A", File: "a.go", Props: map[string]any{"weird": custom{1}}},
		Fact{Kind: KindSymbol, Name: "B", File: "a.go", Props: map[string]any{"weird": custom{2}}},
	)
	s.Freeze()
	ff := s.FactsRef()

	if sameMap(ff[0].Props, ff[1].Props) {
		t.Error("facts with an unencodable prop type were merged; the encoder must refuse what it cannot describe")
	}
	if ff[0].Props["weird"].(custom).N != 1 || ff[1].Props["weird"].(custom).N != 2 {
		t.Error("freeze altered values it could not encode")
	}
}

// TestFreeze_IsIdempotent — Freeze runs once per publication, but a second call must
// not corrupt a store that is already sharing.
func TestFreeze_IsIdempotent(t *testing.T) {
	s := NewStore()
	for i := 0; i < 4; i++ {
		s.Add(Fact{Kind: KindSymbol, Name: fmt.Sprintf("F%d", i), File: "a.go",
			Props:     map[string]any{"symbol_kind": "function"},
			Relations: []Relation{{Kind: RelCalls, Target: "X"}}})
	}
	s.Freeze()
	first := s.FactsRef()[0].Props
	s.Freeze()
	ff := s.FactsRef()

	if !sameMap(ff[0].Props, first) {
		t.Error("a second Freeze replaced the shared map")
	}
	for i := 1; i < 4; i++ {
		if !sameMap(ff[i].Props, first) {
			t.Errorf("fact %d stopped sharing after the second Freeze", i)
		}
	}
	if !s.Frozen() {
		t.Error("Frozen() is false after Freeze")
	}
}

// TestAll_CopiesPropsFromFrozenStore is the invariant the whole design rests on.
//
// Append mode carries the previously PUBLISHED bundle's facts into a new mutable
// store, where binders delete props (unmatchedroutes) and annotators add them. If All
// handed over the frozen store's maps by reference, that write would land in a bundle
// concurrent readers are traversing — and, after Freeze, in the ~9 other facts sharing
// that map.
func TestAll_CopiesPropsFromFrozenStore(t *testing.T) {
	published := NewStore()
	for i := 0; i < 3; i++ {
		published.Add(Fact{Kind: KindSymbol, Name: fmt.Sprintf("F%d", i), File: "a.go",
			Props: map[string]any{"symbol_kind": "function", "unmatched_by_clients": true}})
	}
	published.Freeze()
	if !sameMap(published.FactsRef()[0].Props, published.FactsRef()[2].Props) {
		t.Fatal("setup: the three facts should be sharing one map")
	}

	// What append mode does: copy the published facts into a fresh store, then let a
	// binder rewrite them.
	work := NewStore()
	work.Add(published.All()...)
	work.UpdateWhere(func(f *Fact) {
		delete(f.Props, "unmatched_by_clients")
		f.Props["annotated"] = true
	})

	for i, f := range published.FactsRef() {
		if _, gone := f.Props["unmatched_by_clients"]; !gone {
			t.Errorf("published fact %d lost a prop to a mutation of the copy", i)
		}
		if _, leaked := f.Props["annotated"]; leaked {
			t.Errorf("published fact %d gained a prop from a mutation of the copy", i)
		}
	}
}

// TestFreeze_PreservesEveryFactValue — the safety net for the whole operation: after
// freezing, every fact must still equal what it was. Deep equality over the entire
// store, so a mis-shared map shows up whatever the shape.
func TestFreeze_PreservesEveryFactValue(t *testing.T) {
	s := equivalenceCorpus(t) // the awkward-shapes corpus: dupes, self-edges, collisions
	before := s.All()         // an independent deep copy, taken before freezing

	s.Freeze()
	after := s.FactsRef()

	if len(before) != len(after) {
		t.Fatalf("freeze changed the fact count: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if !reflect.DeepEqual(before[i], after[i]) {
			t.Fatalf("fact %d changed value across Freeze:\nbefore %+v\nafter  %+v", i, before[i], after[i])
		}
	}
}

// The key scratch is shared by every level of the encoding, so a nested map takes its
// run of the same backing array while the level above is still reading its own. If
// the two ever overlapped, two facts with different nested props would encode to the
// same key and Freeze would collapse them onto one map — a corruption that reads back
// as perfectly valid facts, just not the ones extraction produced.
func TestFreeze_NestedMapsDoNotShareKeyRuns(t *testing.T) {
	nested := func(inner map[string]any) map[string]any {
		return map[string]any{
			"alpha": "a",
			"omega": "z",
			"rows":  []map[string]any{{"k": "v"}, inner},
		}
	}
	s := NewStore()
	s.Add(Fact{Kind: KindSymbol, Name: "one", Props: nested(map[string]any{"x": 1, "y": 2})})
	s.Add(Fact{Kind: KindSymbol, Name: "two", Props: nested(map[string]any{"x": 1, "y": 3})})
	s.Add(Fact{Kind: KindSymbol, Name: "three", Props: nested(map[string]any{"x": 1, "y": 2})})
	s.Freeze()

	ff := s.FactsRef()
	get := func(name string) map[string]any {
		for _, f := range ff {
			if f.Name == name {
				return f.Props
			}
		}
		t.Fatalf("missing %s", name)
		return nil
	}
	one, two, three := get("one"), get("two"), get("three")

	// Equal shapes share; the one that differs deep inside a nested map must not.
	if !sameMap(one, three) {
		t.Errorf("identical nested props were not shared")
	}
	if sameMap(one, two) {
		t.Errorf("props differing only inside a nested map were collapsed onto one")
	}
	// And the values themselves must survive.
	for _, tc := range []struct {
		name string
		want int
	}{{"one", 2}, {"two", 3}, {"three", 2}} {
		rows, ok := get(tc.name)["rows"].([]map[string]any)
		if !ok || len(rows) != 2 {
			t.Fatalf("%s: rows = %v", tc.name, get(tc.name)["rows"])
		}
		if got := rows[1]["y"]; got != tc.want {
			t.Errorf("%s: rows[1][y] = %v, want %d", tc.name, got, tc.want)
		}
	}
}

func BenchmarkFreeze(b *testing.B) {
	// Shapes an extractor really repeats: a handful of distinct prop maps over many
	// facts, which is what makes Freeze worth doing at all.
	shapes := []map[string]any{
		{"symbol_kind": "function", "language": "go", "exported": true, "cyclomatic": 1},
		{"symbol_kind": "function", "language": "go", "exported": false, "cyclomatic": 4, "loop_depth": 2},
		{"symbol_kind": "method", "language": "go", "receiver": "Engine", "exported": true},
		{"source": "internal", "language": "go"},
	}
	for b.Loop() {
		b.StopTimer()
		s := NewStore()
		for i := range 20000 {
			props := make(map[string]any, len(shapes[i%len(shapes)]))
			for k, v := range shapes[i%len(shapes)] {
				props[k] = v
			}
			s.Add(Fact{Kind: KindSymbol, Name: "n" + itoa(i), Props: props})
		}
		b.StartTimer()
		s.Freeze()
	}
}
