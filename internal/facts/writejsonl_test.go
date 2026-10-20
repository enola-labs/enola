package facts

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// WriteJSONL sorts an index of offsets into one contiguous buffer rather than sorting a
// []string of the lines. The output must be identical to what the string form produced
// — facts.jsonl is the artifact every snapshot ID is derived from — so these tests pin
// the ordering against an independent reference, and pin the memory saving that
// motivated the change.

// referenceWriteJSONL is the previous implementation, kept verbatim as an oracle:
// marshal each fact, collect the lines as strings, sort.Strings, write. If the two ever
// disagree, this is what shipped. It is faithful down to writing through a
// bufio.Writer, so it can also stand in as the allocation baseline.
func referenceWriteJSONL(t *testing.T, s *Store, w io.Writer) {
	t.Helper()
	lines := make([]string, 0, s.Count())
	for _, f := range s.FactsRef() {
		// The wire shape, built the naive way: a fresh slice per fact, an id per
		// call, no scratch reuse. WriteJSONL reuses both, and the two must still
		// produce identical bytes.
		rels := make([]wireRelation, 0, len(f.Relations))
		for _, r := range f.Relations {
			wr := wireRelation{Relation: r}
			if t := s.targetFactFor(r.Target, f.Repo); t >= 0 && LandsByName(f, r, s.FactsRef()[t]) {
				wr.TargetID = s.FactsRef()[t].Identity()
			}
			rels = append(rels, wr)
		}
		if len(rels) > 1 {
			sort.Slice(rels, func(i, j int) bool {
				if rels[i].Kind != rels[j].Kind {
					return rels[i].Kind < rels[j].Kind
				}
				return rels[i].Target < rels[j].Target
			})
		}
		b, err := json.Marshal(wireFact{Fact: f, Relations: rels, ID: f.Identity()})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	sort.Strings(lines)

	bw := bufio.NewWriter(w)
	for _, line := range lines {
		if _, err := bw.WriteString(line); err != nil {
			t.Fatal(err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			t.Fatal(err)
		}
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
}

// referenceJSONL is referenceWriteJSONL's output as a string, for the equality tests.
func referenceJSONL(t *testing.T, s *Store) string {
	t.Helper()
	var sb strings.Builder
	referenceWriteJSONL(t, s, &sb)
	return sb.String()
}

// awkwardStore builds facts whose serialized lines stress the comparator: shared
// prefixes, lines that are prefixes of other lines, embedded quotes and newlines,
// non-ASCII, and duplicate lines.
func awkwardStore() *Store {
	s := NewStore()
	names := []string{
		"a", "ab", "abc", "ab.c", "ab/c", "AB", "a\"b", "a\\b", "a\nb", "a\tb",
		"ä", "日本", "a b", "a:b", "", "z", "zz",
	}
	for i, n := range names {
		s.Add(Fact{
			Kind: KindSymbol, Name: n, File: "f" + n + ".go", Line: i,
			Props: map[string]any{"symbol_kind": "function", "quote\"key": "va\"lue"},
			Relations: []Relation{
				{Kind: RelCalls, Target: "t" + n},
				{Kind: RelCalls, Target: "t"},
			},
		})
	}
	// Exact duplicates: the sort must be able to place equal lines without losing one.
	s.Add(Fact{Kind: KindModule, Name: "dup", File: "d.go"})
	s.Add(Fact{Kind: KindModule, Name: "dup", File: "d.go"})
	return s
}

func TestWriteJSONL_MatchesStringSortReference(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func() *Store
	}{
		{"empty", NewStore},
		{"awkward", awkwardStore},
		{"corpus", func() *Store { return buildMemCorpus(2000) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.store()
			want := referenceJSONL(t, s)

			var got bytes.Buffer
			if err := s.WriteJSONL(&got); err != nil {
				t.Fatal(err)
			}
			if got.String() != want {
				t.Errorf("WriteJSONL output differs from the string-sort reference\n got %d bytes\nwant %d bytes",
					got.Len(), len(want))
				gl, wl := strings.Split(got.String(), "\n"), strings.Split(want, "\n")
				for i := 0; i < len(gl) && i < len(wl); i++ {
					if gl[i] != wl[i] {
						t.Errorf("first difference at line %d:\n got %q\nwant %q", i, gl[i], wl[i])
						break
					}
				}
			}
		})
	}
}

// TestWriteJSONL_RandomizedAgainstReference — the comparator is the whole risk of this
// change, and a bug in it might only show on particular byte sequences. Compare against
// the reference over randomized fact sets.
func TestWriteJSONL_RandomizedAgainstReference(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	alphabet := []string{"a", "b", "A", "/", ".", ":", "\"", "\\", "é", "0", "_", "-", " "}
	randName := func() string {
		var sb strings.Builder
		for i := rng.Intn(6); i >= 0; i-- {
			sb.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		return sb.String()
	}

	for round := 0; round < 30; round++ {
		s := NewStore()
		for i := 0; i < 200; i++ {
			f := Fact{Kind: KindSymbol, Name: randName(), File: randName() + ".go", Line: rng.Intn(50)}
			if rng.Intn(2) == 0 {
				f.Props = map[string]any{randName(): randName(), "n": rng.Intn(5)}
			}
			for j := rng.Intn(4); j > 0; j-- {
				f.Relations = append(f.Relations, Relation{Kind: RelCalls, Target: randName()})
			}
			s.Add(f)
		}

		want := referenceJSONL(t, s)
		var got bytes.Buffer
		if err := s.WriteJSONL(&got); err != nil {
			t.Fatal(err)
		}
		if got.String() != want {
			t.Fatalf("round %d: WriteJSONL disagrees with the string-sort reference", round)
		}
	}
}

// TestWriteJSONL_IsDeterministic — the reason the sort exists at all. Extractors emit
// facts in varying order, so two stores holding the same facts in different orders must
// serialize identically.
func TestWriteJSONL_IsDeterministic(t *testing.T) {
	base := awkwardStore().All()

	serialize := func(ff []Fact) string {
		s := NewStore()
		s.Add(ff...)
		var buf bytes.Buffer
		if err := s.WriteJSONL(&buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	want := serialize(base)
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 10; i++ {
		shuffled := append([]Fact(nil), base...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := serialize(shuffled); got != want {
			t.Fatalf("shuffle %d produced different bytes; serialization is not order-independent", i)
		}
	}
}

// TestWriteJSONL_MakesOneAllocationPerLine is the ratchet on the change's whole purpose,
// measured against the implementation it replaced.
//
// The metric is the NUMBER of allocations, not bytes. Total bytes allocated is the same
// for both — each marshals every fact once and materializes the line data once. What
// changed is that the string form made a second, separately allocated copy of every
// line and had to keep all N of them alive at once so it could sort them; the indexed
// form appends into one buffer and sorts 8-byte offsets. Allocation count is cumulative
// and exact, unlike HeapAlloc and HeapObjects, which report what the collector has got
// around to and shift under the ReadMemStats call itself.
//
// One fewer allocation per line is also one fewer object alive at the peak, which is
// what mattered: on a real graph whose output was 792 MiB the old shape moved live heap
// by 3.8 GB, twice per snapshot, and drove peak process footprint to 11.3 GB.
func TestWriteJSONL_MakesOneAllocationPerLine(t *testing.T) {
	if testing.Short() {
		t.Skip("serializes a 200k-fact store")
	}
	s := buildMemCorpus(memCorpus)

	countAllocs := func(fn func()) uint64 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		fn()
		runtime.ReadMemStats(&after)
		return after.Mallocs - before.Mallocs
	}

	var out countingWriter
	got := countAllocs(func() {
		if err := s.WriteJSONL(&out); err != nil {
			t.Fatal(err)
		}
	})
	var refOut countingWriter
	want := countAllocs(func() { referenceWriteJSONL(t, s, &refOut) })

	t.Logf("output %d bytes over %d facts", out.n, memCorpus)
	t.Logf("  indexed sort: %d allocations (%.2f per fact)", got, float64(got)/memCorpus)
	t.Logf("  string sort:  %d allocations (%.2f per fact)", want, float64(want)/memCorpus)

	// The string form allocates the marshalled bytes AND a string copy of them per
	// line; the indexed form allocates only the former. Requiring the saving to be at
	// least half a line's worth per fact keeps this robust to allocator details while
	// still failing outright if the second copy comes back.
	saved := float64(want) - float64(got)
	if saved < 0.5*memCorpus {
		t.Errorf("indexed form made %d allocations vs the string form's %d — saved %.0f, want at least %d",
			got, want, saved, memCorpus/2)
	}
}

// countingWriter counts bytes and keeps none of them.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// TestWriteJSONL_RoundTripsThroughReadJSONL — the output must remain loadable, since
// restore and the history both read it back.
func TestWriteJSONL_RoundTripsThroughReadJSONL(t *testing.T) {
	orig := awkwardStore()
	var buf bytes.Buffer
	if err := orig.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}

	restored := NewStore()
	if err := restored.ReadJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	if got, want := restored.Count(), orig.Count(); got != want {
		t.Fatalf("round trip produced %d facts, want %d", got, want)
	}

	var again bytes.Buffer
	if err := restored.WriteJSONL(&again); err != nil {
		t.Fatal(err)
	}
	var first bytes.Buffer
	if err := orig.WriteJSONL(&first); err != nil {
		t.Fatal(err)
	}
	if again.String() != first.String() {
		t.Error("a store round-tripped through JSONL does not re-serialize identically")
	}
}

// TestWriteJSONL_ReproducesRealCorpusByteForByte is the determinism check that matters
// most, and the cheapest way to get it: point ENOLA_FACTS_CORPUS at a facts.jsonl that a
// previous release wrote, load it, re-serialize it, and require the bytes back.
//
// It exercises the comparator over a real corpus — millions of lines with the escaping,
// unicode, shared prefixes and duplicate content that synthetic fixtures approximate —
// without paying for extraction. A sort bug that only shows on unusual byte sequences
// would survive every other test in this file and fail here.
func TestWriteJSONL_ReproducesRealCorpusByteForByte(t *testing.T) {
	path := os.Getenv("ENOLA_FACTS_CORPUS")
	if path == "" {
		t.Skip("set ENOLA_FACTS_CORPUS=<facts.jsonl> written by a previous build")
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore()
	if err := s.ReadJSONLFile(path); err != nil {
		t.Fatal(err)
	}
	t.Logf("corpus: %d facts, %d bytes", s.Count(), len(want))

	h := sha256.New()
	var got countingWriter
	if err := s.WriteJSONL(io.MultiWriter(h, &got)); err != nil {
		t.Fatal(err)
	}
	if got.n != int64(len(want)) {
		t.Fatalf("re-serialized %d bytes, the file on disk is %d", got.n, len(want))
	}
	wantSum := sha256.Sum256(want)
	if !bytes.Equal(h.Sum(nil), wantSum[:]) {
		t.Fatal("re-serializing the corpus did not reproduce it byte for byte")
	}
}

// TestWriteJSONL_LargeFactLine — a single fact can serialize to more than the initial
// buffer capacity; growth must not corrupt the offsets.
func TestWriteJSONL_LargeFactLine(t *testing.T) {
	s := NewStore()
	big := strings.Repeat("x", 300_000)
	s.Add(
		Fact{Kind: KindSymbol, Name: "small", File: "a.go"},
		Fact{Kind: KindSymbol, Name: big, File: "b.go"},
		Fact{Kind: KindSymbol, Name: "another", File: "c.go"},
	)
	want := referenceJSONL(t, s)
	var got bytes.Buffer
	if err := s.WriteJSONL(&got); err != nil {
		t.Fatal(err)
	}
	if got.String() != want {
		t.Errorf("output with an oversized line differs from the reference (%d vs %d bytes)", got.Len(), len(want))
	}
	if n := strings.Count(got.String(), "\n"); n != 3 {
		t.Errorf("got %d lines, want 3", n)
	}
	_ = fmt.Sprint(big[:1])
}
