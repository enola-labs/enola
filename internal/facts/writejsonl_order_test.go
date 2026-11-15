package facts

import (
	"bytes"
	"fmt"
	"math/rand"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// facts.jsonl is its lines sorted byte by byte. The sort compares a fixed prefix
// first, then the scalar fields a line opens with, so it must still order lines that
// share all of the prefix, lines that end inside it, lines that share their opening
// fields, and lines that are equal.
func TestWriteJSONL_SortedByWholeLines(t *testing.T) {
	s := NewStore()
	long := strings.Repeat("x", 2*jsonlKeyLen)
	exact := strings.Repeat("y", jsonlKeyLen-len(`{"kind":"symbol","name":"","file":"e.go"`))
	for i := 0; i < 50; i++ {
		// Same kind and a name longer than the key: only the file tells them apart.
		s.Add(Fact{Kind: KindSymbol, Name: long, File: fmt.Sprintf("f%02d.go", 49-i)})
		s.Add(Fact{Kind: KindSymbol, Name: long + fmt.Sprint(i%7)})
		s.Add(Fact{Kind: KindModule, Name: fmt.Sprint("m", i%5)})
		s.Add(Fact{Kind: KindModule, Name: "m"})
		// The same head, longer than the key: only the props tell them apart.
		s.Add(Fact{Kind: KindSymbol, Name: long, File: "same.go", Props: map[string]any{"v": 49 - i}})
		// A head that ends exactly where the key does: the key is all of it, and the
		// lines still differ after it.
		s.Add(Fact{Kind: KindSymbol, Name: exact, File: "e.go", Props: map[string]any{"v": 49 - i}})
	}
	var buf bytes.Buffer
	if err := s.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), []byte("\n"))
	if len(lines) != s.Count() {
		t.Fatalf("%d lines for %d facts", len(lines), s.Count())
	}
	want := slices.Clone(lines)
	slices.SortFunc(want, bytes.Compare)
	for i := range lines {
		if !bytes.Equal(lines[i], want[i]) {
			t.Fatalf("line %d is out of order:\n got  %s\n want %s", i, lines[i], want[i])
		}
	}
}

// The same across chunks and windows: more facts than one window of every worker
// holds, in an order unrelated to the sorted one.
func TestWriteJSONL_SortedAcrossChunks(t *testing.T) {
	s := NewStore()
	n := 3*jsonlChunk*max(1, runtime.GOMAXPROCS(0)) + 17
	for i := 0; i < n; i++ {
		// A multiplicative scramble: distinct names in no sorted order.
		s.Add(Fact{Kind: KindSymbol, Name: fmt.Sprintf("n%09d", (i*7919)%n), File: "a.go"})
	}
	var buf bytes.Buffer
	if err := s.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), []byte("\n"))
	if len(lines) != n {
		t.Fatalf("%d lines for %d facts", len(lines), n)
	}
	if !slices.IsSortedFunc(lines, bytes.Compare) {
		t.Fatal("facts.jsonl is not sorted across chunks")
	}
}

// The key pass reads a line's opening bytes from wireHead alone, so wireHead must
// marshal as every line begins: the same fields, order, tags and escaping.
func TestWireHead_IsALinesPrefix(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "engine", "testdata", "golden", "*.facts.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no goldens found: %v", err)
	}
	extra := []Fact{
		{Kind: "symbol", Name: `a<b>&"c"` + " ", File: "x/y.go", Line: 3, EndLine: 9, Column: 2, EndColumn: 7, Repo: "r", RepoPrefixed: true},
		{Kind: "module", Name: "m"},
	}
	checked := 0
	for _, path := range paths {
		s := NewStore()
		if err := s.ReadJSONLFile(path); err != nil {
			t.Fatal(err)
		}
		if checked == 0 {
			for _, f := range extra {
				s.Add(f)
			}
		}
		ws := newWireScratch()
		s.mu.RLock()
		for i := range s.facts {
			h, err := ws.head(&s.facts[i])
			if err != nil {
				t.Fatal(err)
			}
			h = bytes.Clone(h)
			line, err := s.wireLine(s.facts[i], ws)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(line, h) || len(line) <= len(h) {
				t.Fatalf("%s: head %s is not a prefix of %s", filepath.Base(path), h, line)
			}
			checked++
		}
		s.mu.RUnlock()
	}
	t.Logf("%d lines", checked)
}

// Facts are added in whatever order extraction produced them, which varies run to run;
// the file may not. Every golden graph, added in three shuffled orders, writes the
// bytes it writes in its own.
func TestWriteJSONL_IndependentOfInputOrder(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "engine", "testdata", "golden", "*.facts.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no goldens found: %v", err)
	}
	for _, path := range paths {
		s := NewStore()
		if err := s.ReadJSONLFile(path); err != nil {
			t.Fatal(err)
		}
		var want bytes.Buffer
		if err := s.WriteJSONL(&want); err != nil {
			t.Fatal(err)
		}
		ff := s.All()
		for seed := int64(1); seed <= 3; seed++ {
			r := rand.New(rand.NewSource(seed))
			r.Shuffle(len(ff), func(i, j int) { ff[i], ff[j] = ff[j], ff[i] })
			shuffled := NewStore()
			shuffled.Add(ff...)
			var got bytes.Buffer
			if err := shuffled.WriteJSONL(&got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("%s: shuffled with seed %d, the file differs", filepath.Base(path), seed)
			}
		}
	}
}
