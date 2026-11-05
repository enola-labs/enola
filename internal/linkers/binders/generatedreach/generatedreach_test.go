package generatedreach

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const api = "Shop/OpenAPI/generated/OrdersAPI.swift"

func bind(t *testing.T, store *facts.Store) {
	t.Helper()
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatalf("Bind: %v", err)
	}
}

func symbol(name, file string, generated bool, rels ...facts.Relation) facts.Fact {
	props := map[string]any{"symbol_kind": "method"}
	if generated {
		props["generated"] = true
	}
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: "app/" + file, Repo: "app", Props: props, Relations: rels}
}

func calls(target string) facts.Relation { return facts.Relation{Kind: facts.RelCalls, Target: target} }

func clientRoute(path, caller string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindRoute, Name: path, File: "app/" + api, Repo: "app",
		Props: map[string]any{
			facts.PropRole: facts.RoleClient, facts.PropCaller: caller,
			"method": "GET", "generated": true,
		},
	}
}

func reachability(t *testing.T, store *facts.Store) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, f := range store.ByKind(facts.KindRoute) {
		out[f.Name] = f.PropAny(facts.PropClientReachable)
	}
	return out
}

// The generated client of a spec with three operations. The application calls one,
// a test calls another, and nothing calls the third. Each operation is a request
// builder wrapped by a generated convenience method, the shape generators emit.
func generatedClient() *facts.Store {
	store := facts.NewStore()
	store.Add(
		clientRoute("/orders", "OrdersAPI.listOrdersRoute"),
		clientRoute("/orders/{id}", "OrdersAPI.getOrderRoute"),
		clientRoute("/orders/{id}/notes", "OrdersAPI.listNotesRoute"),
		symbol("OrdersAPI.listOrdersRoute", api, true),
		symbol("OrdersAPI.listOrders", api, true, calls("OrdersAPI.listOrdersRoute")),
		symbol("OrdersAPI.getOrderRoute", api, true),
		symbol("OrdersAPI.getOrder", api, true, calls("OrdersAPI.getOrderRoute")),
		symbol("OrdersAPI.listNotesRoute", api, true),
		symbol("OrdersAPI.listNotes", api, true, calls("OrdersAPI.listNotesRoute")),
		symbol("OrdersScreen.load", "Shop/Orders/OrdersScreen.swift", false, calls("OrdersAPI.listOrders")),
		symbol("OrdersAPITests.testGetOrder", "Shop/Tests/OrdersAPITests.swift", false, calls("OrdersAPI.getOrder")),
	)
	return store
}

func TestOnlyGeneratedOrTestCallersIsUnreachable(t *testing.T) {
	store := generatedClient()
	bind(t, store)

	got := reachability(t, store)
	want := map[string]any{
		"/orders":            nil,   // hand-written caller: no verdict
		"/orders/{id}":       false, // reached from a test only
		"/orders/{id}/notes": false, // reached by nothing
	}
	for path, w := range want {
		if got[path] != w {
			t.Errorf("%s: client_reachable = %v, want %v", path, got[path], w)
		}
	}
}

// A verdict an extractor wrote is left alone.
func TestExtractorVerdictIsKept(t *testing.T) {
	store := generatedClient()
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind == facts.KindRoute && f.Name == "/orders/{id}/notes" {
			f.SetProp(facts.PropClientReachable, true)
		}
	})
	bind(t, store)

	if got := reachability(t, store)["/orders/{id}/notes"]; got != true {
		t.Errorf("client_reachable = %v, want the extractor's true", got)
	}
}

// A hand-written caller added after the first run withdraws the verdict.
func TestVerdictIsWithdrawnWhenACallerAppears(t *testing.T) {
	store := generatedClient()
	bind(t, store)
	store.Add(symbol("NotesScreen.load", "Shop/Notes/NotesScreen.swift", false, calls("OrdersAPI.listNotes")))
	bind(t, store)

	if got := reachability(t, store)["/orders/{id}/notes"]; got != nil {
		t.Errorf("client_reachable = %v, want unset", got)
	}
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.Name == "/orders/{id}/notes" && f.PropAny(PropReachSource) != nil {
			t.Error("source marker survived the withdrawn verdict")
		}
	}
}

// A call from hand-written code is not judged: only generated code is.
func TestHandWrittenCallSiteIsNotJudged(t *testing.T) {
	store := facts.NewStore()
	route := clientRoute("/orders", "OrdersClient.fetch")
	delete(route.Props, "generated")
	store.Add(route, symbol("OrdersClient.fetch", "Shop/Orders/OrdersClient.swift", false))
	bind(t, store)

	if got := reachability(t, store)["/orders"]; got != nil {
		t.Errorf("client_reachable = %v, want unset", got)
	}
}
