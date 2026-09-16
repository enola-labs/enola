package orphans

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// sym builds a symbol fact of the given kind. Kind decides the confidence tier, which is
// what this annotator filters on.
func sym(name, kind string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindSymbol, Name: name, File: name + ".go",
		Props: map[string]any{"symbol_kind": kind, "exported": true},
	}
}

func annotated(t *testing.T, ff ...facts.Fact) map[string]any {
	t.Helper()
	store := facts.NewStore()
	store.Add(ff...)
	if err := (&Explainer{}).Annotate(context.Background(), store); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	out := make(map[string]any)
	for _, f := range store.All() {
		if v, ok := (f.Props)[PropOrphanClass]; ok {
			out[f.Name] = v
		}
	}
	return out
}

// Only the HIGH-confidence tier is annotated. medium (struct/class/interface) and low
// (method/type/const/var) are leads, not facts: their usage is not edge-tracked, so a
// symbol can look unreferenced while being used as a field type or reached through an
// interface. Writing "your change made this dead" into a diff on that evidence is the
// false-positive problem the tier split exists to prevent.
func TestAnnotate_HighConfidenceOnly(t *testing.T) {
	got := annotated(t,
		sym("pkg.DeadFunc", "function"),   // high — annotated
		sym("pkg.DeadStruct", "struct"),   // medium — not
		sym("pkg.DeadIface", "interface"), // medium — not
		sym("pkg.DeadMethod", "method"),   // low — not
		sym("pkg.DeadType", "type"),       // low — not
	)

	if _, ok := got["pkg.DeadFunc"]; !ok {
		t.Error("a high-confidence orphan was not annotated")
	}
	for _, n := range []string{"pkg.DeadStruct", "pkg.DeadIface", "pkg.DeadMethod", "pkg.DeadType"} {
		if v, ok := got[n]; ok {
			t.Errorf("%s annotated %q — only the high tier may be written", n, v)
		}
	}
}

// The prop is written ONLY for orphans. Marking live symbols would add a prop to every
// symbol fact in the graph, and a diff against a baseline taken without annotations would
// then report thousands of changed facts instead of the few that moved.
func TestAnnotate_LiveSymbolsAreUntouched(t *testing.T) {
	caller := sym("pkg.Caller", "function")
	caller.Relations = []facts.Relation{{Kind: facts.RelCalls, Target: "pkg.Callee"}}

	got := annotated(t, caller, sym("pkg.Callee", "function"))

	if _, ok := got["pkg.Callee"]; ok {
		t.Error("a referenced symbol was marked as an orphan")
	}
	if len(got) > 1 {
		t.Errorf("annotated %d symbols, want at most the uncalled caller: %v", len(got), got)
	}
}

// Symbol names are unique only WITHIN a repo. Keying on the name alone would let one
// repo's uncalled namesake mark another repo's live symbol as dead.
func TestAnnotate_IsScopedPerRepo(t *testing.T) {
	live := sym("shared.Helper", "function")
	live.Repo = "alpha"
	caller := sym("alpha.Caller", "function")
	caller.Repo = "alpha"
	caller.Relations = []facts.Relation{{Kind: facts.RelCalls, Target: "shared.Helper"}}

	dead := sym("shared.Helper", "function")
	dead.Repo = "beta"

	store := facts.NewStore()
	store.Add(live, caller, dead)
	if err := (&Explainer{}).Annotate(context.Background(), store); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	for _, f := range store.All() {
		if f.Name != "shared.Helper" {
			continue
		}
		_, marked := f.Props[PropOrphanClass]
		if f.Repo == "alpha" && marked {
			t.Error("the referenced copy in repo alpha was marked dead")
		}
	}
}

// The class distinguishes "references nothing and is referenced by nothing" from
// "uncalled but still calling", which is the difference between a leaf to delete and a
// subtree to unpick.
func TestAnnotate_RecordsTheClass(t *testing.T) {
	calling := sym("pkg.UncalledButCalling", "function")
	calling.Relations = []facts.Relation{{Kind: facts.RelCalls, Target: "pkg.Isolated"}}

	got := annotated(t, calling, sym("pkg.Isolated", "function"))

	if got["pkg.UncalledButCalling"] != "unreferenced" {
		t.Errorf("class = %v, want %q", got["pkg.UncalledButCalling"], "unreferenced")
	}
}
