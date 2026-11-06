package server

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A Go package beside its cgo C sources is two module facts under one directory name.
// Store order differs between a fresh generate and a restore from disk, so explore
// must answer the same whichever loaded first, and name both languages.
func TestExploreModule_SameNameModulesAnswerInAnyOrder(t *testing.T) {
	goMod := facts.Fact{Kind: facts.KindModule, Name: "x/grammar", File: "x/grammar",
		Props: map[string]any{"language": "go", "package": "grammar"}}
	cMod := facts.Fact{Kind: facts.KindModule, Name: "x/grammar", File: "x/grammar",
		Props: map[string]any{"language": "c"}}
	sym := facts.Fact{Kind: facts.KindSymbol, Name: "x/grammar.Language", File: "x/grammar/binding.go", Line: 3,
		Props:     map[string]any{"symbol_kind": "function", "exported": true, "language": "go"},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "x/grammar"}}}

	render := func(first, second facts.Fact) string {
		store := facts.NewStore()
		store.Add(first, second, sym)
		var sb strings.Builder
		if !newTestServer(store).exploreModule(store, "x/grammar", 1, modeSummary, &sb) {
			t.Fatal("exploreModule did not find x/grammar")
		}
		return sb.String()
	}
	goFirst, cFirst := render(goMod, cMod), render(cMod, goMod)
	if goFirst != cFirst {
		t.Errorf("answer depends on store order:\n--- go first\n%s\n--- c first\n%s", goFirst, cFirst)
	}
	for _, want := range []string{"- Language: c, go\n", "- Package: grammar\n"} {
		if !strings.Contains(goFirst, want) {
			t.Errorf("missing %q in:\n%s", want, goFirst)
		}
	}
}
