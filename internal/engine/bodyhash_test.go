package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Every symbol with a span gets the hash of what the span covers; a symbol without one,
// and a fact that is not a symbol, get none.
func TestSymbolBodyHash(t *testing.T) {
	dir := t.TempDir()
	src := "function a() {\n\treturn 1;\n}\n\nfunction b() {\n\treturn 2;\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "x.ts"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Engine{store: facts.NewStore()}
	e.store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "x", File: "x.ts", Line: 1, EndLine: 7},
		facts.Fact{Kind: facts.KindSymbol, Name: "x.a", File: "x.ts", Line: 1, EndLine: 3},
		facts.Fact{Kind: facts.KindSymbol, Name: "x.b", File: "x.ts", Line: 5, EndLine: 7},
		facts.Fact{Kind: facts.KindSymbol, Name: "x.c", File: "x.ts", Line: 5},
	)
	e.hashSymbolBodies(dir, 0)

	got := map[string]string{}
	for _, f := range e.store.FactsRef() {
		got[f.Name] = f.PropString(facts.PropBodyHash)
	}
	if want := facts.BodyHash([]byte(src), 1, 3); got["x.a"] != want || want == "" {
		t.Errorf("x.a: body_hash %q, want %q", got["x.a"], want)
	}
	if got["x.b"] == "" || got["x.b"] == got["x.a"] {
		t.Errorf("x.b: body_hash %q, want its own", got["x.b"])
	}
	if got["x"] != "" || got["x.c"] != "" {
		t.Errorf("unspanned or non-symbol facts hashed: module %q, x.c %q", got["x"], got["x.c"])
	}
}

// Hashing from a line's offset is the same as hashing the whole file, at every span,
// with and without a trailing newline and past the end of the file.
func TestSymbolBodyHashMatchesWholeFile(t *testing.T) {
	for _, src := range []string{"a\n\n b\nc\n", "a\n\n b\nc", "", "\n\n"} {
		starts := lineStarts([]byte(src))
		for start := 1; start <= 6; start++ {
			for end := start; end <= 6; end++ {
				want := facts.BodyHash([]byte(src), start, end)
				got := ""
				if start <= len(starts) {
					got = facts.BodyHash([]byte(src)[starts[start-1]:], 1, end-start+1)
				}
				if got != want {
					t.Errorf("%q %d-%d: got %q, want %q", src, start, end, got, want)
				}
			}
		}
	}
}
