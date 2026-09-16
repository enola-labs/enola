package orphans

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// storeOf builds a store from inline facts, so these tests pin the rule rather
// than a corpus snapshot.
func storeOf(ff ...facts.Fact) *facts.Store {
	st := facts.NewStore()
	for _, f := range ff {
		st.Add(f)
	}
	return st
}

// symRel is sym() with relations; annotate_test.go owns the plain sym helper.
func symRel(name, kind string, rels ...facts.Relation) facts.Fact {
	f := sym(name, kind)
	f.File = "src/a.cs"
	f.Relations = rels
	return f
}

func orphanNames(st *facts.Store) map[string]bool {
	out := map[string]bool{}
	for _, o := range Detect(st) {
		out[o.Name] = true
	}
	return out
}

// The bitwarden shape: a static class nothing names, whose extension method is
// called. Calling `services.AddSecretsManagerServices()` references the METHOD;
// the class holding it is never named, and read as dead.
func TestMemberUse_TypeWithUsedMemberIsNotDead(t *testing.T) {
	got := orphanNames(storeOf(
		symRel("src.Extensions", facts.SymbolClass),
		symRel("src.Extensions.AddServices", facts.SymbolMethod),
		symRel("src.Startup", facts.SymbolClass),
		symRel("src.Startup.Configure", facts.SymbolMethod,
			facts.Relation{Kind: facts.RelCalls, Target: "src.Extensions.AddServices"}),
	))
	if got["src.Extensions"] {
		t.Error("a class whose member is called is not dead")
	}
	if got["src.Extensions.AddServices"] {
		t.Error("the called member itself must not be an orphan")
	}
}

// The rule must not make every type its own referent: crediting the MEMBER as the
// user of its own type would mean no type could ever be dead.
func TestMemberUse_UnusedMembersDoNotRescueTheirType(t *testing.T) {
	got := orphanNames(storeOf(
		symRel("src.DeadThing", facts.SymbolClass),
		symRel("src.DeadThing.NeverCalled", facts.SymbolMethod),
		symRel("src.Other", facts.SymbolClass),
	))
	if !got["src.DeadThing"] {
		t.Error("a class whose members are all unused is still dead")
	}
}

// A member calling something on its own type is not external use of that type.
func TestMemberUse_SelfReferenceDoesNotRescue(t *testing.T) {
	got := orphanNames(storeOf(
		symRel("src.Lonely", facts.SymbolClass),
		symRel("src.Lonely.A", facts.SymbolMethod,
			facts.Relation{Kind: facts.RelCalls, Target: "src.Lonely.B"}),
		symRel("src.Lonely.B", facts.SymbolMethod),
	))
	if !got["src.Lonely"] {
		t.Error("a class used only by its own members is still dead")
	}
}

// Only TYPE kinds own members this way. A local function nested under a method
// says nothing about the method that encloses it.
func TestMemberUse_OnlyTypesOwnMembers(t *testing.T) {
	got := orphanNames(storeOf(
		symRel("src.Holder", facts.SymbolClass),
		symRel("src.Holder.Outer", facts.SymbolMethod),
		symRel("src.Holder.Outer.Local", facts.SymbolMethod),
		symRel("src.User", facts.SymbolClass),
		symRel("src.User.Run", facts.SymbolMethod,
			facts.Relation{Kind: facts.RelCalls, Target: "src.Holder.Outer.Local"}),
	))
	if !got["src.Holder.Outer"] {
		t.Error("a method is not rescued by a local function nested inside it")
	}
}

// A controller reached only through its routed action is the same shape.
func TestMemberUse_ControllerRescuedByRoutedAction(t *testing.T) {
	st := storeOf(
		symRel("src.InfoController", facts.SymbolClass),
		symRel("src.InfoController.GetAlive", facts.SymbolMethod),
		facts.Fact{
			Kind:  facts.KindRoute,
			Name:  "/alive",
			File:  "src/a.cs",
			Props: map[string]any{"method": "GET", "handler": "src.InfoController.GetAlive"},
		},
	)
	if orphanNames(st)["src.InfoController"] {
		t.Error("a controller whose action is routed is not dead")
	}
}
