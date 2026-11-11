package storedfuncs

import (
	"context"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func sym(name, kind string, flows []string, calls ...string) facts.Fact {
	f := facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "library",
		Props: map[string]any{"symbol_kind": kind, PropFuncFlows: flows}}
	for _, c := range calls {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: c})
	}
	return f
}

func callsOf(store *facts.Store, name string) []string {
	var out []string
	for _, f := range store.FactsRef() {
		if f.Name != name {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				out = append(out, r.Target)
			}
		}
	}
	return out
}

// A holder filled with its constructor's own parameter holds what that constructor's
// callers pass, and a method calling a field through another method of its type
// still calls what the field holds. Only the holder's own instance is linked.
func TestBind_HolderFilledFromAParameter(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		// queue.New(handler) stores its parameter in Queue.handle; Drain calls it
		// through next.
		sym("queue.New", facts.SymbolFunc, []string{"field queue.Queue.handle <- @0"}),
		sym("queue.Queue.Drain", facts.SymbolMethod, nil, "queue.Queue.next"),
		sym("queue.Queue.next", facts.SymbolMethod, []string{"fieldcall queue.Queue.handle"}),
		// newWorker(h) builds its holder from its own parameter.
		sym("loans.newWorker", facts.SymbolFunc, []string{"holder loans.worker.q <- queue.New(0=@0)"}),
		sym("loans.Start", facts.SymbolFunc, []string{"arg loans.newWorker#0 <- loans.Start.func1"}),
		sym("loans.Start.func1", facts.SymbolFunc, nil, "mail.Send"),
		sym("loans.worker.run", facts.SymbolMethod, []string{"holdercall loans.worker.q -> queue.Queue.Drain"}),
		// Another holder of the same type, built with another handler.
		sym("fines.newWorker", facts.SymbolFunc, []string{"holder fines.worker.q <- queue.New(0=fines.charge)"}),
		sym("fines.worker.run", facts.SymbolMethod, []string{"holdercall fines.worker.q -> queue.Queue.Drain"}),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if got := callsOf(store, "loans.worker.run"); !slices.Equal(got, []string{"loans.Start.func1"}) {
		t.Errorf("loans.worker.run calls %v, want only the closure its worker was built with", got)
	}
	if got := callsOf(store, "fines.worker.run"); !slices.Equal(got, []string{"fines.charge"}) {
		t.Errorf("fines.worker.run calls %v, want only fines.charge", got)
	}
	if got := callsOf(store, "queue.Queue.Drain"); !slices.Equal(got, []string{"queue.Queue.next"}) {
		t.Errorf("the generic Drain should gain no edge; it calls %v", got)
	}
}

// A field set where its struct is built and called by a method of the struct is
// linked by type: the method calls what the field holds. A field holding more than
// maxTypedValues functions is a dispatch point and is not.
func TestBind_FieldStoredDirectly(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		sym("shelf.New", facts.SymbolFunc, []string{"field shelf.Sorter.less <- shelf.New.func1"}),
		sym("shelf.New.func1", facts.SymbolFunc, nil, "titles.Compare"),
		sym("shelf.Sorter.Sort", facts.SymbolMethod, []string{"fieldcall shelf.Sorter.less"}),
		// A constructor handed the function, as its parameter, by one caller.
		sym("loans.NewTimer", facts.SymbolFunc, []string{"field loans.Timer.onDue <- @0"}),
		sym("loans.Start", facts.SymbolFunc, []string{"arg loans.NewTimer#0 <- loans.remind"}),
		sym("loans.Timer.fire", facts.SymbolMethod, []string{"fieldcall loans.Timer.onDue"}),
	)
	var many []string
	for _, h := range []string{"a", "b", "c", "d", "e", "f"} {
		many = append(many, "field events.Bus.handler <- events."+h)
	}
	store.Add(
		sym("events.Register", facts.SymbolFunc, many),
		sym("events.Bus.Publish", facts.SymbolMethod, []string{"fieldcall events.Bus.handler"}),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if got := callsOf(store, "shelf.Sorter.Sort"); !slices.Equal(got, []string{"shelf.New.func1"}) {
		t.Errorf("Sort calls %v, want the closure stored in its field", got)
	}
	if got := callsOf(store, "loans.Timer.fire"); !slices.Equal(got, []string{"loans.remind"}) {
		t.Errorf("fire calls %v, want the function its constructor was handed", got)
	}
	if got := callsOf(store, "events.Bus.Publish"); len(got) != 0 {
		t.Errorf("a field holding six functions should not be linked; Publish calls %v", got)
	}
}
