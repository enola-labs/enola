package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Overloads share a fact key, and the whole comment is kept per key. Restored from
// docs.jsonl, two overloads that each rendered their own summary keep no text, so each
// renders its own again; overloads that rendered one whole comment get it back.
func TestReadDocTexts_Overloads(t *testing.T) {
	overload := func(line int, doc string) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: "System.String.String", File: "String.cs", Line: line, Repo: "runtime",
			Props: map[string]any{facts.PropDoc: doc}}
	}
	key := facts.FactKey{Repo: "runtime", Kind: facts.KindSymbol, Name: "System.String.String", File: "String.cs"}
	read := func(store *facts.Store, docs string) {
		t.Helper()
		p := filepath.Join(t.TempDir(), "docs.jsonl")
		if err := os.WriteFile(p, []byte(docs), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if err := readDocTexts(store, f); err != nil {
			t.Fatal(err)
		}
	}

	own := facts.NewStore()
	own.Add(overload(71, "Copies from a span."), overload(93, "Copies from an array."))
	read(own, `{"repo":"runtime","kind":"symbol","name":"System.String.String","file":"String.cs","line":71,"doc":"Copies from a span."}
{"repo":"runtime","kind":"symbol","name":"System.String.String","file":"String.cs","line":93,"doc":"Copies from an array."}
`)
	if got := own.DocText(key); got != "" {
		t.Errorf("overloads rendering their own summaries got a shared text: %q", got)
	}

	whole := facts.NewStore()
	whole.Add(overload(71, "Copies."), overload(93, "Copies too."))
	read(whole, `{"repo":"runtime","kind":"symbol","name":"System.String.String","file":"String.cs","line":71,"doc":"Copies.\nThe whole comment."}
{"repo":"runtime","kind":"symbol","name":"System.String.String","file":"String.cs","line":93,"doc":"Copies.\nThe whole comment."}
`)
	if got := whole.DocText(key); got != "Copies.\nThe whole comment." {
		t.Errorf("the whole comment was not restored: %q", got)
	}
}
