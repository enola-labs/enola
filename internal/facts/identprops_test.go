package facts

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// symbolWith builds a symbol whose identity props are set the way the engine sets them,
// so they live in the field.
func symbolWith(name string, body, inner, decl string, spanStart int) Fact {
	f := Fact{Kind: KindSymbol, Name: name, File: "a.py", Line: 10, EndLine: 20, Repo: "r",
		Props: map[string]any{"symbol_kind": "function", "language": "python", "exported": true}}
	f.SetProp(PropBodyHash, body)
	f.SetProp(PropInnerHash, inner)
	if decl != "" {
		f.SetProp(PropDeclHash, decl)
	}
	SetSpanStart(&f, spanStart)
	return f
}

// mapForm is the same fact with every prop in the map, as Fact held it before the field
// existed. Its encoding is the oracle.
func mapForm(f Fact) Fact {
	g := f
	g.ident = nil
	g.Props = f.AllProps()
	return g
}

// The field changes where the props live, never what is written: a fact encodes byte for
// byte as it did with everything in Props, alone and as a facts.jsonl line.
func TestIdentProps_EncodeAsTheyDidInTheMap(t *testing.T) {
	ff := []Fact{
		symbolWith("a.f", "f2151f78a7606f57", "32664498bdc36443", "", 8),
		symbolWith("a.g", "25fbdae7e04eba84", "884c2980325a7c6a", "0123456789abcdef", 0),
		{Kind: KindModule, Name: "a", File: "a"},
	}
	ff[2].SetProp(PropBodyHash, "only-a-hash-here")
	for _, f := range ff {
		got, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(mapForm(f).JSONView())
		if !bytes.Equal(got, want) {
			t.Fatalf("encoded %s\nwant    %s", got, want)
		}
		if f.ident == nil {
			t.Fatalf("%s: the identity props were not held in the field", f.Name)
		}
	}

	held, flat := NewStore(), NewStore()
	held.Add(ff...)
	for _, f := range ff {
		flat.Add(mapForm(f))
	}
	var a, b bytes.Buffer
	if err := held.WriteJSONL(&a); err != nil {
		t.Fatal(err)
	}
	if err := flat.WriteJSONL(&b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("facts.jsonl differs:\n%s\nwant\n%s", a.Bytes(), b.Bytes())
	}
}

// Reading facts.jsonl moves the identity props off the map, so two symbols that differ
// only in them share one Props map once frozen: the reason the field exists.
func TestIdentProps_ReadMovesThemOffTheMapSoMapsShare(t *testing.T) {
	src := NewStore()
	src.Add(symbolWith("a.f", "1111111111111111", "2222222222222222", "", 0),
		symbolWith("a.g", "3333333333333333", "4444444444444444", "", 7))
	path := filepath.Join(t.TempDir(), "facts.jsonl")
	if err := src.WriteJSONLFile(path); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func(*Store) error{
		func(s *Store) error { return s.ReadJSONLFile(path) },
		func(s *Store) error { return s.ReadJSONLFileShared(path) },
	} {
		s := NewStore()
		if err := read(s); err != nil {
			t.Fatal(err)
		}
		s.Freeze()
		ff := s.FactsRef()
		for _, f := range ff {
			if _, inMap := f.Props[PropBodyHash]; inMap {
				t.Fatalf("%s: body_hash is still in the map after the read", f.Name)
			}
		}
		if !sameMap(ff[0].Props, ff[1].Props) {
			t.Fatal("two symbols differing only in their hashes hold separate Props maps")
		}
		if got := ff[1].PropString(PropInnerHash); got != "4444444444444444" {
			t.Fatalf("inner_hash reads %q", got)
		}
		if got := ff[1].SpanStart(); got != 7 {
			t.Fatalf("span_start reads %d", got)
		}
		if got := ff[0].SpanStart(); got != ff[0].Line {
			t.Fatalf("a fact without span_start starts at %d, want its line", got)
		}
	}
}

// Every reader sees the identity props as props: the accessors, the full set, the names,
// the count, and a value the field cannot hold exactly stays in the map, where it reads
// back unchanged.
func TestIdentProps_ReadAsProps(t *testing.T) {
	f := symbolWith("a.f", "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "", 5)
	if v, ok := f.Prop(PropBodyHash); !ok || v != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("Prop(body_hash) = %v, %v", v, ok)
	}
	if f.PropAny(PropSpanStart) != 5 || f.PropCount() != 6 {
		t.Fatalf("span_start %v, count %d", f.PropAny(PropSpanStart), f.PropCount())
	}
	var names []string
	for k := range f.PropNames() {
		names = append(names, k)
	}
	sort.Strings(names)
	want := []string{"body_hash", "exported", "inner_hash", "language", "span_start", "symbol_kind"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("PropNames %v, want %v", names, want)
	}
	if got := f.AllProps(); len(got) != 6 || got[PropInnerHash] != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("AllProps %v", got)
	}

	for _, odd := range []struct {
		key string
		val any
	}{{PropBodyHash, ""}, {PropSpanStart, 1.5}, {PropSpanStart, -3}, {PropDeclHash, 42}} {
		g := symbolWith("a.g", "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "", 5)
		g.SetProp(odd.key, odd.val)
		if v, ok := g.Prop(odd.key); !ok || !reflect.DeepEqual(v, odd.val) {
			t.Errorf("%s=%v reads back as %v, %v", odd.key, odd.val, v, ok)
		}
		if _, inMap := g.Props[odd.key]; !inMap {
			t.Errorf("%s=%v is not in the map", odd.key, odd.val)
		}
	}

	g := symbolWith("a.h", "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "", 0)
	g.DelProp(PropBodyHash)
	g.DelProp(PropInnerHash)
	if g.ident != nil || g.PropCount() != 3 {
		t.Fatalf("after deleting both hashes: ident %v, count %d", g.ident, g.PropCount())
	}
}

// A Fact copied by value shares the field as it shares Props; writing one copy's identity
// props must not reach the other, since frozen facts are copied out by value.
func TestIdentProps_WritingACopyLeavesTheOriginal(t *testing.T) {
	f := symbolWith("a.f", "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "", 0)
	g := f
	g.SetProp(PropBodyHash, "cccccccccccccccc")
	g.DelProp(PropInnerHash)
	if f.PropString(PropBodyHash) != "aaaaaaaaaaaaaaaa" || f.PropString(PropInnerHash) != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("writing a copy changed the original: %v", f.AllProps())
	}
}
