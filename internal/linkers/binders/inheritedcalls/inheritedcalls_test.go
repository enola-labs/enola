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

// A method's calls on its own object that its file could not place resolve through
// its type's bases: a member it inherits, and a member of an attribute a base class
// declares. An entry that lands on no symbol adds nothing, and the working props are
// gone afterwards.
func TestBind_MemberCallsResolveThroughTheBases(t *testing.T) {
	base := symbol("lib.Base", facts.SymbolClass)
	base.Props[facts.PropFieldTypes] = map[string]any{"helper": "Helper"}
	handle := symbol("app.Concrete.handle", facts.SymbolMethod)
	handle.Props[facts.PropMemberCalls] = []any{"common", "helper.work", "helper.gone", "nothing", "repo.save"}
	s := facts.NewStore()
	s.Add(
		base,
		symbol("lib.Base.common", facts.SymbolMethod),
		symbol("lib.Helper", facts.SymbolClass),
		symbol("lib.Helper.work", facts.SymbolMethod),
		symbol("app.Concrete", facts.SymbolClass, facts.Relation{Kind: facts.RelImplements, Target: "lib.Base"}),
		handle,
	)
	if err := New().Bind(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	got := one(s, "app.Concrete.handle")
	if want := []string{"lib.Base.common", "lib.Helper.work"}; !reflect.DeepEqual(targets(got), want) {
		t.Errorf("calls = %v, want %v", targets(got), want)
	}
	if _, ok := got.Prop(facts.PropMemberCalls); ok {
		t.Error("member_calls is kept")
	}
	if _, ok := one(s, "lib.Base").Prop(facts.PropFieldTypes); ok {
		t.Error("field_types is kept")
	}
}
