package perf

import (
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func method(name string, calls ...string) facts.Fact {
	f := facts.Fact{Kind: facts.KindSymbol, Name: name, Props: map[string]any{"symbol_kind": facts.SymbolMethod}}
	for _, c := range calls {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: c})
	}
	return f
}

func iface(name string, implementers ...string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, Props: map[string]any{
		"symbol_kind": facts.SymbolInterface, propImplementers: implementers,
	}}
}

// A handler reaches a repository through two interfaces, a service's and a
// repository's, as Go code written the usual way does. The walk crosses each
// interface to the implementation behind it and names the route; a function no
// route reaches gets none.
func TestRoutesReachingCrossesInterfaces(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindRoute, Name: "/api/news", Props: map[string]any{"method": "GET"},
			Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "http/news.Handler.List"}}},
		facts.Fact{Kind: facts.KindRoute, Name: "", Props: map[string]any{"method": "USE"},
			Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "http/news.Handler.List"}}},
		method("http/news.Handler.List", "domain/news.Service.List"),
		iface("domain/news.Service", "app/news.service"),
		method("domain/news.Service.List"),
		method("app/news.service.List", "domain/news.Repository.List"),
		iface("domain/news.Repository", "db/news.repository"),
		method("domain/news.Repository.List"),
		method("db/news.repository.List", "db/news.repository.loadCategories"),
		method("db/news.repository.loadCategories"),
		method("jobs/cleanup.Run", "db/news.repository.Purge"),
		method("db/news.repository.Purge"),
	)
	store.BuildGraph()
	x := newReachIndex(store)
	if got, n := x.routesReaching("db/news.repository.loadCategories"); n != 1 || !reflect.DeepEqual(got, []string{"GET /api/news"}) {
		t.Errorf("loadCategories: routes %v (%d), want [GET /api/news]", got, n)
	}
	if got, n := x.routesReaching("db/news.repository.Purge"); n != 0 {
		t.Errorf("Purge is reached only by a job: routes %v (%d), want none", got, n)
	}
}
