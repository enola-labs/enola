package finalinherit

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func classFact(name, repo, file string, line int, props map[string]any, rels ...facts.Relation) facts.Fact {
	return facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      name,
		Repo:      repo,
		File:      file,
		Line:      line,
		Props:     props,
		Relations: rels,
	}
}

func cls(name string, extra map[string]any, rels ...facts.Relation) facts.Fact {
	props := map[string]any{"symbol_kind": "class"}
	for k, v := range extra {
		props[k] = v
	}
	return classFact(name, "", "src/"+name+".java", 1, props, rels...)
}

func TestDerivesFromFinalTypeIsVerdicted(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		cls("app.Frozen", map[string]any{"final": true}),
		cls("app.Broken", nil, facts.Relation{Kind: facts.RelImplements, Target: "app.Frozen"}),
	)
	ins, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 {
		t.Fatalf("insights = %d, want 1", len(ins))
	}
	if ins[0].Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 — the compiler rejects this construct", ins[0].Confidence)
	}
	if len(ins[0].Evidence) != 1 || ins[0].Evidence[0].Symbol != "app.Broken" {
		t.Errorf("evidence = %+v, want the derived type", ins[0].Evidence)
	}
}

func TestSealedIsNotFinal(t *testing.T) {
	// Kotlin sealed classes CAN be subclassed (in scope); only the final prop
	// means "the compiler forbids derivation".
	store := facts.NewStore()
	store.Add(
		cls("app.Poly", map[string]any{"sealed": true}),
		cls("app.Child", nil, facts.Relation{Kind: facts.RelImplements, Target: "app.Poly"}),
	)
	ins, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 0 {
		t.Fatalf("insights = %d, want 0 — sealed permits derivation", len(ins))
	}
}

func TestEdgesThatDoNotCrossFinal(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		cls("app.Base", nil),
		cls("app.Child", nil, facts.Relation{Kind: facts.RelImplements, Target: "app.Base"}),
		// final method fact: member-level rule is out of scope (signature matching
		// the fact model lacks), and a method is not a derivation target.
		classFact("app.Base.run", "", "src/app.Base.java", 2, map[string]any{"symbol_kind": "method", "final": true}),
		// self-edge from a broken parse
		classFact("app.Self", "", "src/app.Self.cpp", 3, map[string]any{"symbol_kind": "class", "final": true},
			facts.Relation{Kind: facts.RelImplements, Target: "app.Self"}),
	)
	ins, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 0 {
		t.Fatalf("insights = %d, want 0", len(ins))
	}
}

func TestSameNameInAnotherRepoIsNotTheBase(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		classFact("app.Frozen", "other-repo", "src/Frozen.java", 1, map[string]any{"symbol_kind": "class", "final": true}),
		// repo "" looking for an implements target named app.Frozen: the final one
		// lives in other-repo, so this repo's identically-named non-final fact (if
		// any) is the base — and there is none final here.
		cls("app.Frozen", nil),
		cls("app.Child", nil, facts.Relation{Kind: facts.RelImplements, Target: "app.Frozen"}),
	)
	ins, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 0 {
		t.Fatalf("insights = %d, want 0 — repos do not mix", len(ins))
	}
}
