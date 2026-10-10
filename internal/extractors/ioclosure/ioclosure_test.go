package ioclosure

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func sym(name, kind string, props map[string]any, rels ...facts.Relation) facts.Fact {
	p := map[string]any{"symbol_kind": kind}
	for k, v := range props {
		p[k] = v
	}
	return facts.Fact{Kind: facts.KindSymbol, Name: name, Props: p, Relations: rels}
}

func calls(target string) facts.Relation {
	return facts.Relation{Kind: facts.RelCalls, Target: target}
}

func implements(target string) facts.Relation {
	return facts.Relation{Kind: facts.RelImplements, Target: target}
}

func performsIO(t *testing.T, all []facts.Fact) map[string]bool {
	t.Helper()
	out := make(map[string]bool)
	for _, f := range all {
		if b, _ := f.PropAny("performs_io").(bool); b {
			out[f.Name] = true
		}
	}
	return out
}

func assertIO(t *testing.T, got map[string]bool, yes, no []string) {
	t.Helper()
	for _, n := range yes {
		if !got[n] {
			t.Errorf("%s: no performs_io, want it", n)
		}
	}
	for _, n := range no {
		if got[n] {
			t.Errorf("%s: performs_io, want none", n)
		}
	}
}

func TestPropagate_ReachesThroughWrappersAndCycles(t *testing.T) {
	all := []facts.Fact{
		sym("db.query", facts.SymbolFunc, map[string]any{"io_direct": true}),
		sym("repo.load", facts.SymbolFunc, nil, calls("db.query")),
		sym("svc.run", facts.SymbolFunc, nil, calls("repo.load"), calls("os.Getenv")), // an unknown callee is ignored
		// A cycle with no I/O in it stays clean, and terminates.
		sym("a.ping", facts.SymbolFunc, nil, calls("a.pong")),
		sym("a.pong", facts.SymbolFunc, nil, calls("a.ping")),
		// A cycle that reaches I/O is I/O all the way round.
		sym("b.even", facts.SymbolFunc, nil, calls("b.odd")),
		sym("b.odd", facts.SymbolFunc, nil, calls("b.even"), calls("svc.run")),
		{Kind: facts.KindModule, Name: "db"},
	}
	Propagate(all)
	assertIO(t, performsIO(t, all),
		[]string{"db.query", "repo.load", "svc.run", "b.even", "b.odd"},
		[]string{"a.ping", "a.pong", "db"})
}

// The injected-dependency case: the caller holds an interface, the I/O is in the
// class behind it.
func TestPropagate_FollowsAnInterfaceToItsImplementers(t *testing.T) {
	all := []facts.Fact{
		sym("lib.ILibrary", facts.SymbolInterface, nil),
		sym("lib.ILibrary.DeleteItem", facts.SymbolMethod, nil),
		sym("lib.ILibrary.Describe", facts.SymbolMethod, nil),
		sym("impl.Library", facts.SymbolClass, nil, implements("lib.ILibrary")),
		sym("impl.Library.DeleteItem", facts.SymbolMethod, map[string]any{"io_direct": true}),
		sym("impl.Library.Describe", facts.SymbolMethod, nil),
		sym("impl.Memory", facts.SymbolClass, nil, implements("lib.ILibrary")),
		sym("impl.Memory.DeleteItem", facts.SymbolMethod, nil),
		sym("app.Provider.DeleteEpisode", facts.SymbolMethod, nil, calls("lib.ILibrary.DeleteItem")),
		sym("app.Provider.Title", facts.SymbolMethod, nil, calls("lib.ILibrary.Describe")),
	}
	Propagate(all)
	assertIO(t, performsIO(t, all),
		[]string{"impl.Library.DeleteItem", "lib.ILibrary.DeleteItem", "app.Provider.DeleteEpisode"},
		// The other implementer does not become I/O for sharing the interface, and a
		// method no implementer does I/O in stays clean.
		[]string{"impl.Memory.DeleteItem", "lib.ILibrary.Describe", "impl.Library.Describe", "app.Provider.Title"})
}

func TestPropagate_FollowsThroughAnExtendingInterface(t *testing.T) {
	all := []facts.Fact{
		sym("a.Reader", facts.SymbolInterface, nil),
		sym("a.Reader.Read", facts.SymbolMethod, nil),
		sym("a.Store", facts.SymbolInterface, nil, implements("a.Reader")), // extends, and does not redeclare Read
		sym("b.Disk", facts.SymbolClass, nil, implements("a.Store")),
		sym("b.Disk.Read", facts.SymbolMethod, map[string]any{"io_direct": true}),
		sym("c.use", facts.SymbolFunc, nil, calls("a.Reader.Read")),
	}
	Propagate(all)
	assertIO(t, performsIO(t, all), []string{"a.Reader.Read", "c.use"}, nil)
}

// A base class's method with a body answers for itself. Only its abstract methods
// are answered by the subclasses.
func TestPropagate_ConcreteBaseMethodIsNotItsOverride(t *testing.T) {
	all := []facts.Fact{
		sym("m.Base", facts.SymbolClass, nil),
		sym("m.Base.save", facts.SymbolMethod, nil),
		sym("m.Base.fetch", facts.SymbolMethod, map[string]any{"abstract": true}),
		sym("m.Remote", facts.SymbolClass, nil, implements("m.Base")),
		sym("m.Remote.save", facts.SymbolMethod, map[string]any{"io_direct": true}),
		sym("m.Remote.fetch", facts.SymbolMethod, map[string]any{"io_direct": true}),
		sym("n.keep", facts.SymbolFunc, nil, calls("m.Base.save")),
		sym("n.pull", facts.SymbolFunc, nil, calls("m.Base.fetch")),
	}
	Propagate(all)
	assertIO(t, performsIO(t, all),
		[]string{"m.Base.fetch", "n.pull"},
		[]string{"m.Base.save", "n.keep"})
}

func TestPropagate_DoubleColonMembers(t *testing.T) {
	all := []facts.Fact{
		sym(`App\Store`, facts.SymbolInterface, nil),
		sym(`App\Store::put`, facts.SymbolMethod, nil),
		sym(`App\Disk`, facts.SymbolClass, nil, implements(`App\Store`)),
		sym(`App\Disk::put`, facts.SymbolMethod, map[string]any{"io_direct": true}),
		sym(`App\Job::run`, facts.SymbolMethod, nil, calls(`App\Store::put`)),
	}
	Propagate(all)
	assertIO(t, performsIO(t, all), []string{`App\Store::put`, `App\Job::run`}, nil)
}
