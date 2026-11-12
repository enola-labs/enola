package facts

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sharedFixture writes a facts.jsonl whose facts repeat a few Props shapes and
// Relations lists, the way a real graph does, and returns its path.
func sharedFixture(t *testing.T) string {
	t.Helper()
	s := NewStore()
	for i := 0; i < 40; i++ {
		s.Add(Fact{
			Kind: KindSymbol, Name: fmt.Sprintf("pkg.F%02d", i), File: fmt.Sprintf("f%d.go", i%3), Line: i + 1,
			Props:     map[string]any{"symbol_kind": "function", "exported": i%2 == 0, "cyclomatic": i % 3},
			Relations: []Relation{{Kind: RelCalls, Target: fmt.Sprintf("pkg.F%02d", i%4)}},
		})
	}
	path := filepath.Join(t.TempDir(), "facts.jsonl")
	if err := s.WriteJSONLFile(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// distinctProps counts the Props maps a store holds, by allocation.
func distinctProps(ff []Fact) int {
	seen := map[uintptr]bool{}
	for _, f := range ff {
		seen[reflect.ValueOf(f.Props).Pointer()] = true
	}
	return len(seen)
}

// Reading shared and then freezing ends where reading and then freezing does: the same
// facts, sharing the same number of maps. Only when the copies die differs.
func TestReadJSONLFileShared_EndsWhereReadThenFreezeDoes(t *testing.T) {
	path := sharedFixture(t)

	plain := NewStore()
	if err := plain.ReadJSONLFile(path); err != nil {
		t.Fatal(err)
	}
	plain.Freeze()

	shared := NewStore()
	if err := shared.ReadJSONLFileShared(path); err != nil {
		t.Fatal(err)
	}
	shared.Freeze()

	a, b := plain.FactsRef(), shared.FactsRef()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("a store read shared holds different facts from one read and then frozen")
	}
	if got, want := distinctProps(b), distinctProps(a); got != want {
		t.Fatalf("read shared: %d distinct Props maps, read then frozen: %d", got, want)
	}
	if !shared.Frozen() {
		t.Fatal("Freeze after a shared read did not mark the store frozen")
	}
}

// The point of the change: a repeated shape is shared while the file is still being read,
// so the copies decoding produced never accumulate. Asserted before Freeze runs.
func TestReadJSONLFileShared_SharesBeforeFreeze(t *testing.T) {
	s := NewStore()
	if err := s.ReadJSONLFileShared(sharedFixture(t)); err != nil {
		t.Fatal(err)
	}
	ff := s.FactsRef()
	if got := distinctProps(ff); got != 6 {
		t.Fatalf("%d distinct Props maps after the read, want the 6 shapes the fixture repeats", got)
	}
	if !sameRels(ff[0].Relations, ff[4].Relations) {
		t.Fatal("two facts calling the same target hold separate Relations after the read")
	}
	if cap(s.facts) != len(s.facts) {
		t.Fatalf("fact array has capacity %d for %d facts; it should be sized from the file", cap(s.facts), len(s.facts))
	}
}

// Facts the store held before the read, and facts added after it, are shared too: Freeze
// carries on from where the read stopped instead of skipping what it did not read.
func TestReadJSONLFileShared_FreezeCoversFactsAddedAroundTheRead(t *testing.T) {
	// float64, as decoding gives it: an int would be a different shape, rightly unshared.
	shape := func() map[string]any {
		return map[string]any{"symbol_kind": "function", "exported": true, "cyclomatic": float64(0)}
	}
	s := NewStore()
	s.Add(Fact{Kind: KindSymbol, Name: "before", File: "x.go", Props: shape()})
	if err := s.ReadJSONLFileShared(sharedFixture(t)); err != nil {
		t.Fatal(err)
	}
	s.Add(Fact{Kind: KindSymbol, Name: "after", File: "x.go", Props: shape()})
	s.Freeze()

	ff := s.FactsRef()
	first, last := ff[0], ff[len(ff)-1]
	if !sameMap(first.Props, ff[1].Props) {
		t.Error("the fact held before the read was not shared with the equal facts read")
	}
	if !sameMap(last.Props, first.Props) {
		t.Error("the fact added after the read was not shared at Freeze")
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

func TestCountLines(t *testing.T) {
	for in, want := range map[string]int{"": 0, "a\n": 1, "a\nb": 2, "a\n\nb\n": 3} {
		n, err := countLines(stringsReader(in))
		if err != nil || n != want {
			t.Errorf("countLines(%q) = %d, %v; want %d", in, n, err, want)
		}
	}
}
