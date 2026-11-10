package inheritedcalls

import (
	"context"
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func symbol(name, kind string, rels ...facts.Relation) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "r", File: "f.py",
		Props: map[string]any{"symbol_kind": kind}, Relations: rels}
}

func calls(t string) facts.Relation { return facts.Relation{Kind: facts.RelCalls, Target: t} }

func store() *facts.Store {
	s := facts.NewStore()
	s.Add(
		symbol("lib.Base", facts.SymbolClass),
		symbol("lib.Base.run", facts.SymbolMethod),
		symbol("app.Concrete", facts.SymbolClass, facts.Relation{Kind: facts.RelImplements, Target: "lib.Base"}),
		symbol("app.Concrete.step", facts.SymbolMethod),
		symbol("app.main", facts.SymbolFunc, calls("app.Concrete.run"), calls("app.Concrete.step"), calls("app.Concrete.gone"), calls("print")),
		symbol("app.twice", facts.SymbolFunc, calls("lib.Base.run"), calls("app.Concrete.run")),
	)
	return s
}

func one(s *facts.Store, name string) facts.Fact { return s.LookupByExactName(name)[0] }

func targets(f facts.Fact) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls {
			out = append(out, r.Target)
		}
	}
	return out
}

// A call spelled through the subclass reaches the declaration it inherits, and the
// caller keeps the spelling. A call to the subclass's own member, to a member nobody
// declares, or to no type is left as it was.
func TestBind_ACallThroughASubclassReachesTheInheritedMember(t *testing.T) {
	s := store()
	if err := New().Bind(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	main := one(s, "app.main")
	if got, want := targets(main), []string{"lib.Base.run", "app.Concrete.step", "app.Concrete.gone", "print"}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if got := main.PropAny(facts.PropInheritedCalls); !reflect.DeepEqual(got, []string{"app.Concrete.run"}) {
		t.Errorf("%s = %v", facts.PropInheritedCalls, got)
	}
}

// A caller that already calls the declaration does not get it twice, and a second
// run changes nothing.
func TestBind_NoDuplicateEdgeAndIdempotent(t *testing.T) {
	s := store()
	b := New()
	_ = b.Bind(context.Background(), s)
	first := s.All()
	_ = b.Bind(context.Background(), s)
	if !reflect.DeepEqual(first, s.All()) {
		t.Error("a second run changed the facts")
	}
	if got := targets(one(s, "app.twice")); !reflect.DeepEqual(got, []string{"lib.Base.run"}) {
		t.Errorf("calls = %v, want the declaration once", got)
	}
}
