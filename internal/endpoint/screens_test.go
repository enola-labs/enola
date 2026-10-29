package endpoint

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// The screens that depend on an endpoint: a web page from the pages the extractor found
// reaching the call, and a mobile screen from the call graph, walked up from the
// function making the call through a repository the screen's view model reaches only
// by its interface. A call nothing runs adds no screen, and the walk stops at the
// nearest screen rather than going on to what contains it.
func TestScreens_WebPagesAndTheNearestMobileScreen(t *testing.T) {
	server := facts.Fact{Kind: facts.KindRoute, Name: "/api/orders/{id}", Repo: "api", File: "api/routes.go",
		Props: map[string]any{"method": "GET", "handler": "orders.Get"}}
	matched := []any{map[string]any{"repo": "api", "method": "GET", "name": "/api/orders/{id}"}}
	client := func(repo, file, caller string, extra map[string]any) facts.Fact {
		p := map[string]any{"role": "client", "method": "GET", facts.PropCaller: caller, facts.PropMatchedRoutes: matched}
		for k, v := range extra {
			p[k] = v
		}
		return facts.Fact{Kind: facts.KindRoute, Name: "/api/orders/{}", Repo: repo, File: file, Props: p}
	}
	sym := func(name, file, lang, kind string, rels ...facts.Relation) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Repo: "app",
			Props: map[string]any{"language": lang, "symbol_kind": kind}, Relations: rels}
	}
	calls := func(t string) facts.Relation { return facts.Relation{Kind: facts.RelCalls, Target: t} }

	st := facts.NewStore()
	st.Add(
		server,
		client("web", "web/src/lib/orders.ts", "src/lib.OrdersApi.get",
			map[string]any{facts.PropClientReachable: true, facts.PropClientPages: []string{"src/app/orders/page.tsx"}}),
		client("web", "web/src/lib/orders.ts", "src/lib.OrdersApi.legacy", map[string]any{facts.PropClientReachable: false}),
		client("app", "app/data/OrdersApi.kt", "data.OrdersApi.get", nil),
		sym("data.OrdersApi.get", "app/data/OrdersApi.kt", "kotlin", facts.SymbolMethod),
		sym("data.OrderRepositoryImpl", "app/data/OrderRepositoryImpl.kt", "kotlin", facts.SymbolClass,
			facts.Relation{Kind: facts.RelImplements, Target: "domain.OrderRepository"}),
		sym("data.OrderRepositoryImpl.load", "app/data/OrderRepositoryImpl.kt", "kotlin", facts.SymbolMethod, calls("data.OrdersApi.get")),
		sym("domain.OrderRepository", "app/domain/OrderRepository.kt", "kotlin", facts.SymbolInterface),
		sym("domain.OrderRepository.load", "app/domain/OrderRepository.kt", "kotlin", facts.SymbolMethod),
		sym("ui.OrdersViewModel.refresh", "app/ui/OrdersViewModel.kt", "kotlin", facts.SymbolMethod, calls("domain.OrderRepository.load")),
		sym("ui.OrdersScreen", "app/ui/OrdersScreen.kt", "kotlin", facts.SymbolFunc, calls("ui.OrdersViewModel.refresh")),
		sym("ui.MainActivity", "app/ui/MainActivity.kt", "kotlin", facts.SymbolClass, calls("ui.OrdersScreen")),
	)
	st.BuildGraph()

	clients := []facts.Fact{}
	for _, f := range st.ByKind(facts.KindRoute) {
		if f.PropString("role") == "client" {
			clients = append(clients, f)
		}
	}
	got := screensFrom(st, clients, []facts.Fact{server})
	var files []string
	for _, s := range got {
		files = append(files, s.File)
		if !slices.Equal(s.Endpoints, []string{"GET /api/orders/{id}"}) {
			t.Errorf("%s: endpoints %v", s.File, s.Endpoints)
		}
	}
	want := []string{"app/ui/OrdersScreen.kt", "web/src/app/orders/page.tsx"}
	if !slices.Equal(files, want) {
		t.Errorf("screens = %v, want %v", files, want)
	}
}

func TestIsScreen_SwiftViewController(t *testing.T) {
	for name, want := range map[string]bool{
		"Orders/Scenes.OrdersViewController": true,
		"Orders/Scenes.OrdersInteractor":     false,
	} {
		f := facts.Fact{Kind: facts.KindSymbol, Name: name, File: "Orders/Scenes/X.swift",
			Props: map[string]any{"language": "swift", "symbol_kind": facts.SymbolClass}}
		if got := isScreen(f); got != want {
			t.Errorf("isScreen(%s) = %v, want %v", name, got, want)
		}
	}
}

// A call made from a view model's initializer reaches the screen that holds the view
// model: the screen calls another of its methods, and nothing calls the initializer.
func TestScreens_ThroughTheTypeAnInitializerRunsIn(t *testing.T) {
	server := facts.Fact{Kind: facts.KindRoute, Name: "/api/appeals/{id}", Repo: "api", File: "api/routes.go",
		Props: map[string]any{"method": "GET"}}
	client := facts.Fact{Kind: facts.KindRoute, Name: "/api/appeals/{}", Repo: "app", File: "app/vm/AppealViewModel.kt",
		Props: map[string]any{"role": "client", "method": "GET", facts.PropCaller: "vm.AppealViewModel.load",
			facts.PropMatchedRoutes: []any{map[string]any{"repo": "api", "method": "GET", "name": "/api/appeals/{id}"}}}}
	sym := func(name, kind string, rels ...facts.Relation) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, File: "app/" + name + ".kt", Repo: "app",
			Props: map[string]any{"language": "kotlin", "symbol_kind": kind}, Relations: rels}
	}
	calls := func(t string) facts.Relation { return facts.Relation{Kind: facts.RelCalls, Target: t} }
	st := facts.NewStore()
	st.Add(server, client,
		sym("vm.AppealViewModel", facts.SymbolClass, calls("vm.AppealViewModel.load")),
		sym("vm.AppealViewModel.load", facts.SymbolMethod),
		sym("vm.AppealViewModel.onBack", facts.SymbolMethod),
		sym("ui.AppealFragment", facts.SymbolClass),
		sym("ui.AppealFragment.onStart", facts.SymbolMethod, calls("vm.AppealViewModel.onBack")),
	)
	st.BuildGraph()
	got := screensFrom(st, []facts.Fact{client}, []facts.Fact{server})
	if len(got) != 1 || got[0].Symbol != "ui.AppealFragment" {
		t.Errorf("screens = %+v, want ui.AppealFragment", got)
	}
}
