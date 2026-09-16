package perf

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// riskySymbol is a function calling a storage fact inside a loop: confirmed I/O, so
// the analyzer rates it high rather than on a name alone.
func riskySymbol(name, file string, depth int) facts.Fact {
	return facts.Fact{
		Kind: facts.KindSymbol, Name: name, File: file, Repo: "app", Line: 5,
		Props: map[string]any{
			"language": "go", "symbol_kind": facts.SymbolFunc,
			"loop_depth": depth, "scaling_loop_depth": depth, "loop_count": 1,
			"calls_in_loop": []string{"app/data.LoadUser"},
		},
		Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "app/data.LoadUser"}},
	}
}

func storageFact(name string) facts.Fact {
	return facts.Fact{Kind: facts.KindStorage, Name: name, Repo: "app"}
}

func annotate(t *testing.T, ff ...facts.Fact) map[string]any {
	t.Helper()
	store := facts.NewStore()
	store.Add(ff...)
	if err := (&Explainer{}).Annotate(context.Background(), store); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	out := map[string]any{}
	for _, f := range store.All() {
		if v, ok := f.Props[PropPerfRisk]; ok {
			out[f.Name] = v
		}
	}
	return out
}

// The point of the annotator: a diff identifies insights by title, so a finding past
// the 50-insight cap reaches a diff only as the rollup's number. A prop is diffed as
// a fact attribute, so the function a change made expensive can still be NAMED.
func TestHighSeverityRiskIsMarkedOnTheSymbol(t *testing.T) {
	got := annotate(t, storageFact("app/data.LoadUser"), riskySymbol("app.Handler", "app/h.go", 1))
	if got["app.Handler"] != "call-in-loop" {
		t.Fatalf("perf_risk on app.Handler = %v, want call-in-loop", got["app.Handler"])
	}
}

// Only the high tier. Medium is a curated-keyword match and low is a cold path;
// stamping those would put "your change made this slow" into a diff on evidence that
// does not support the sentence.
func TestOnlyHighSeverityIsMarked(t *testing.T) {
	// No storage fact, so the in-loop callee is a name-only match at best.
	got := annotate(t, facts.Fact{
		Kind: facts.KindSymbol, Name: "app.mild", File: "app/m.go", Repo: "app",
		Props: map[string]any{
			"language": "go", "symbol_kind": facts.SymbolFunc,
			"loop_depth": 1, "scaling_loop_depth": 1, "loop_count": 1,
			"calls_in_loop": []string{"app.formatName"},
		},
	})
	if _, marked := got["app.mild"]; marked {
		t.Fatalf("a finding below high severity was marked: %v", got)
	}
}

// Written ONLY on a symbol that carries a risk. Marking every other symbol would add
// a prop to every symbol fact in the graph, and a diff against a baseline taken
// without annotations would report thousands of changed facts instead of the few
// that moved.
func TestCleanSymbolsAreLeftAlone(t *testing.T) {
	clean := facts.Fact{
		Kind: facts.KindSymbol, Name: "app.Plain", File: "app/p.go", Repo: "app",
		Props: map[string]any{"language": "go", "symbol_kind": facts.SymbolFunc},
	}
	got := annotate(t, storageFact("app/data.LoadUser"), riskySymbol("app.Handler", "app/h.go", 1), clean)
	if _, marked := got["app.Plain"]; marked {
		t.Fatal("a symbol with no finding was annotated")
	}
	if len(got) != 1 {
		t.Fatalf("annotated %d symbols, want exactly the risky one: %v", len(got), got)
	}
}

// Names are unique only within a repo. Keying on the name alone would mark one
// repository's clean function because another repository's namesake is slow.
func TestAnnotationIsScopedToItsRepo(t *testing.T) {
	other := facts.Fact{
		Kind: facts.KindSymbol, Name: "app.Handler", File: "app/h.go", Repo: "other",
		Props: map[string]any{"language": "go", "symbol_kind": facts.SymbolFunc},
	}
	store := facts.NewStore()
	store.Add(storageFact("app/data.LoadUser"), riskySymbol("app.Handler", "app/h.go", 1), other)
	if err := (&Explainer{}).Annotate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	for _, f := range store.All() {
		if f.Repo == "other" {
			if _, marked := f.Props[PropPerfRisk]; marked {
				t.Fatal("a same-named symbol in another repo was annotated")
			}
		}
	}
}

// Nothing to say means no props at all, so a repository with no risk produces a
// snapshot byte-identical to one taken before the annotator existed.
func TestNoFindingsWritesNothing(t *testing.T) {
	if got := annotate(t); len(got) != 0 {
		t.Fatalf("empty store produced annotations: %v", got)
	}
}
