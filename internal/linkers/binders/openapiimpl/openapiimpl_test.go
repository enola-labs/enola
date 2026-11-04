package openapiimpl

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/binders/vendoredspecs"
)

func bind(t *testing.T, store *facts.Store) {
	t.Helper()
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatalf("Bind: %v", err)
	}
}

func op(path, spec, id string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindRoute, Name: path, File: spec,
		Props: map[string]any{
			facts.PropSource:    facts.RouteSourceOpenAPI,
			facts.PropRole:      facts.RoleServer,
			"method":            "GET",
			"operationId":       id,
			"spec_file":         spec,
			facts.PropFramework: "openapi",
		},
	}
}

func method(name string, generated bool) facts.Fact {
	props := map[string]any{"symbol_kind": facts.SymbolMethod}
	if generated {
		props["generated"] = true
	}
	return facts.Fact{Kind: facts.KindSymbol, Name: name, Props: props}
}

func handledBy(t *testing.T, store *facts.Store, path, spec string) string {
	t.Helper()
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.Name != path || f.PropString("spec_file") != spec {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				return r.Target
			}
		}
		return ""
	}
	t.Fatalf("no route %s in %s", path, spec)
	return ""
}

// The generated interface, its wrappers and a mock all name the operation; only the
// hand-written implementation is bound.
func TestBindsStrictServerImplementation(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		op("/item/{id}", "api/service.yml", "getItem"),
		op("/items", "api/service.yml", "list_items"),
		method("internal/api.StrictServerInterface.GetItem", true),
		method("internal/api.ServerInterfaceWrapper.GetItem", true),
		method("internal/api.strictHandler.GetItem", true),
		method("internal/api/mocks.MockStrictServerInterface.GetItem", true),
		method("internal/server.Server.GetItem", false),
		method("internal/server.Server.ListItems", false),
		method("internal/other.Cache.GetItem", false),
	)
	bind(t, store)

	if got := handledBy(t, store, "/item/{id}", "api/service.yml"); got != "internal/server.Server.GetItem" {
		t.Errorf("getItem handled_by = %q", got)
	}
	if got := handledBy(t, store, "/items", "api/service.yml"); got != "internal/server.Server.ListItems" {
		t.Errorf("list_items handled_by = %q", got)
	}
}

// Two specs in one repo share an operation name; each binds to the type implementing
// the rest of its own spec.
func TestScopesSharedOperationsBySpec(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		op("/healthz", "api/a.yml", "isLive"),
		op("/items", "api/a.yml", "items"),
		op("/healthz", "api/b.yml", "isLive"),
		op("/filters", "api/b.yml", "filters"),
		method("server.A.IsLive", false),
		method("server.A.Items", false),
		method("server.B.IsLive", false),
		method("server.B.Filters", false),
	)
	bind(t, store)

	if got := handledBy(t, store, "/healthz", "api/a.yml"); got != "server.A.IsLive" {
		t.Errorf("a.yml isLive = %q", got)
	}
	if got := handledBy(t, store, "/healthz", "api/b.yml"); got != "server.B.IsLive" {
		t.Errorf("b.yml isLive = %q", got)
	}
}

// Two types implementing the whole spec equally: nothing is bound.
func TestTieBindsNothing(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		op("/a", "api.yml", "getA"),
		method("x.One.GetA", false),
		method("x.Two.GetA", false),
	)
	bind(t, store)
	if got := handledBy(t, store, "/a", "api.yml"); got != "" {
		t.Errorf("handled_by = %q, want none", got)
	}
}

// A client spec and a route another binder already bound are left alone, and a second
// run adds nothing.
func TestLeavesClientAndBoundRoutes(t *testing.T) {
	client := op("/c", "api.yml", "getC")
	client.Props[facts.PropRole] = facts.RoleClient
	bound := op("/b", "api.yml", "getB")
	bound.Relations = []facts.Relation{{Kind: facts.RelHandledBy, Target: "x.Other.Handle"}}
	store := facts.NewStore()
	store.Add(client, bound, op("/a", "api.yml", "getA"),
		method("x.S.GetC", false), method("x.S.GetB", false), method("x.S.GetA", false))
	bind(t, store)
	bind(t, store)

	if got := handledBy(t, store, "/c", "api.yml"); got != "" {
		t.Errorf("client route bound to %q", got)
	}
	if got := handledBy(t, store, "/b", "api.yml"); got != "x.Other.Handle" {
		t.Errorf("bound route rebound to %q", got)
	}
	for _, f := range store.ByKind(facts.KindRoute) {
		n := 0
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				n++
			}
		}
		if n > 1 {
			t.Errorf("%s has %d handled_by edges", f.Name, n)
		}
	}
}

func TestPascal(t *testing.T) {
	for in, want := range map[string]string{
		"getItem": "GetItem", "filters_facets": "FiltersFacets", "is-buyer": "IsBuyer", "v2.list": "V2List",
	} {
		if got := pascal(in); got != want {
			t.Errorf("pascal(%q) = %q, want %q", in, got, want)
		}
	}
}

// A spec the repo calls, misread as served, shares only its health checks with the
// repo's server: a type implementing a minority of the spec binds nothing.
func TestMinorityBindsNothing(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		op("/healthz", "api/called.yml", "isLive"),
		op("/analyze", "api/called.yml", "analyze"),
		op("/predict", "api/called.yml", "predict"),
		op("/versions", "api/called.yml", "listVersions"),
		method("server.S.IsLive", false),
		method("search.Filters.analyze", false),
	)
	bind(t, store)
	for _, p := range []string{"/healthz", "/analyze"} {
		if got := handledBy(t, store, p, "api/called.yml"); got != "" {
			t.Errorf("%s handled_by = %q, want none", p, got)
		}
	}
}

// A spec documenting a route the code registers is served by that route's handler.
func TestInheritsTheDocumentedRoutesHandler(t *testing.T) {
	code := facts.Fact{Kind: facts.KindRoute, Name: "/api/v1/users/:id/sessions", File: "config/routes.rb",
		Props:     map[string]any{"method": "GET", facts.PropFramework: "rails"},
		Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "Api::V1::SessionsController#index"}}}
	store := facts.NewStore()
	store.Add(code, op("/api/v1/users/{user_id}/sessions", "api/sessions.yml", "listSessions"))
	bind(t, store)
	if got := handledBy(t, store, "/api/v1/users/{user_id}/sessions", "api/sessions.yml"); got != "Api::V1::SessionsController#index" {
		t.Errorf("handled_by = %q", got)
	}
}

func roleOf(store *facts.Store, path, spec string) string {
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.Name == path && f.PropString("spec_file") == spec {
			return f.PropString(facts.PropRole)
		}
	}
	return ""
}

// A repository that implements its specs by operation does not serve a spec of which
// it implements nothing: that spec is one it calls. A rerun, with vendored-specs
// restoring what it finds flagged, settles in the same place.
func TestUnimplementedSpecIsCalled(t *testing.T) {
	store := facts.NewStore()
	repoOp := func(path, spec, id string) facts.Fact {
		f := op(path, spec, id)
		f.Repo = "svc"
		return f
	}
	repoMethod := func(name string) facts.Fact {
		f := method(name, false)
		f.Repo = "svc"
		return f
	}
	store.Add(
		repoOp("/items", "api/service.yml", "items"),
		repoOp("/healthz", "api/service.yml", "isLive"),
		repoOp("/v1/features", "api/feature_store.yml", "getFeatures"),
		repoOp("/healthz", "api/feature_store.yml", "isReady"),
		repoMethod("server.S.Items"),
		repoMethod("server.S.IsLive"),
		// a route the service registers by hand, which no spec documents
		facts.Fact{Kind: facts.KindRoute, Name: "/metrics", Repo: "svc", Props: map[string]any{"method": "GET"}},
	)
	bind(t, store)
	if r := roleOf(store, "/v1/features", "api/feature_store.yml"); r != facts.RoleClient {
		t.Errorf("unimplemented spec role = %q, want client", r)
	}
	if r := roleOf(store, "/items", "api/service.yml"); r != facts.RoleServer {
		t.Errorf("implemented spec role = %q, want server", r)
	}

	if err := vendoredspecs.New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	bind(t, store)
	if r := roleOf(store, "/v1/features", "api/feature_store.yml"); r != facts.RoleClient {
		t.Errorf("after a rerun, unimplemented spec role = %q, want client", r)
	}
}

// A repository whose specs document the routes its framework registers is not judged
// by operation: a spec it implements none of stays served.
func TestDocumentingRepoKeepsItsSpecs(t *testing.T) {
	code := facts.Fact{Kind: facts.KindRoute, Name: "/api/v1/users/:id", File: "config/routes.rb",
		Props:     map[string]any{"method": "GET"},
		Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "UsersController#show"}}}
	store := facts.NewStore()
	store.Add(code,
		op("/api/v1/users/{id}", "api/users.yml", "getUser"),
		op("/api/v1/orders", "api/orders.yml", "listOrders"),
	)
	bind(t, store)
	if r := roleOf(store, "/api/v1/orders", "api/orders.yml"); r != facts.RoleServer {
		t.Errorf("spec with no handler in a documenting repo: role = %q, want server", r)
	}
}

// A spec that gives no operationIds still documents the routes the code registers:
// it is bound by method and path, and never counted as an operation to implement.
func TestInheritsTheDocumentedRoutesHandlerWithoutOperationID(t *testing.T) {
	code := facts.Fact{Kind: facts.KindRoute, Name: "/accounts/:account_id/cards", File: "engines/cards/config/routes.rb",
		Props:     map[string]any{"method": "GET", facts.PropFramework: "rails"},
		Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "Cards::CardsController#index"}}}
	impl := method("api.Server.ListCards", false)
	impl.Props["symbol_kind"] = facts.SymbolMethod
	store := facts.NewStore()
	store.Add(code, impl, op("/accounts/{account_id}/cards", "api/service.yaml", ""))
	bind(t, store)
	if got := handledBy(t, store, "/accounts/{account_id}/cards", "api/service.yaml"); got != "Cards::CardsController#index" {
		t.Errorf("handled_by = %q", got)
	}
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.PropString("spec_file") == "api/service.yaml" && f.PropString(PropHandlerBinding) != BindingCodeRoute {
			t.Errorf("handler_binding = %q, want %q", f.PropString(PropHandlerBinding), BindingCodeRoute)
		}
	}
}

func codeRoute(path, method, handler string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: path, File: "config/routes.rb",
		Props:     map[string]any{"method": method, facts.PropFramework: "rails"},
		Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: handler}}}
}

// A spec written under a server base, or relative to a prefix the code adds, is bound
// to the code route it documents; a tail two code routes share binds nothing.
func TestBindsUnderServerBaseAndByUniqueTail(t *testing.T) {
	based := op("/holidays/{id}", "stores/openapi/v1.yaml", "")
	based.Props["method"] = "DELETE"
	based.Props["server_base"] = "/stores/v1"
	tail := op("/tickets", "openapi/desk-v1.yaml", "")
	tail.Props["method"] = "POST"
	shared := op("/status", "openapi/desk-v1.yaml", "")
	store := facts.NewStore()
	store.Add(based, tail, shared,
		codeRoute("/stores/v1/holidays/:id", "DELETE", "Stores::V1::HolidaysController#destroy"),
		codeRoute("/api/v1/tickets", "POST", "Api::V1::TicketsController#create"),
		codeRoute("/api/v1/orders/status", "GET", "Api::V1::OrdersController#status"),
		codeRoute("/api/v1/carts/status", "GET", "Api::V1::CartsController#status"))
	bind(t, store)
	if got := handledBy(t, store, "/holidays/{id}", "stores/openapi/v1.yaml"); got != "Stores::V1::HolidaysController#destroy" {
		t.Errorf("server-based op handled_by = %q", got)
	}
	if got := handledBy(t, store, "/tickets", "openapi/desk-v1.yaml"); got != "Api::V1::TicketsController#create" {
		t.Errorf("tail op handled_by = %q", got)
	}
	if got := handledBy(t, store, "/status", "openapi/desk-v1.yaml"); got != "" {
		t.Errorf("a tail two routes share bound %q", got)
	}
}

// A code route whose handler an extractor named but left for a later binder to
// confirm (handler_target) binds the operation once that symbol exists.
func TestBindsThroughAConfirmedHandlerTarget(t *testing.T) {
	route := facts.Fact{Kind: facts.KindRoute, Name: "/app/v1/orders", File: "orders/router.go",
		Props: map[string]any{"method": "GET", facts.PropFramework: "chi", facts.PropHandlerTarget: "orders/handlers.ListOrders.Handle"}}
	ghost := facts.Fact{Kind: facts.KindRoute, Name: "/app/v1/carts", File: "orders/router.go",
		Props: map[string]any{"method": "GET", facts.PropFramework: "chi", facts.PropHandlerTarget: "orders/handlers.Missing.Handle"}}
	store := facts.NewStore()
	store.Add(route, ghost, method("orders/handlers.ListOrders.Handle", false),
		op("/app/v1/orders", "api/openapi/v1.yml", "listOrders"), op("/app/v1/carts", "api/openapi/v1.yml", "listCarts"))
	bind(t, store)
	if got := handledBy(t, store, "/app/v1/orders", "api/openapi/v1.yml"); got != "orders/handlers.ListOrders.Handle" {
		t.Errorf("handled_by = %q", got)
	}
	if got := handledBy(t, store, "/app/v1/carts", "api/openapi/v1.yml"); got != "" {
		t.Errorf("an unconfirmed handler_target bound %q", got)
	}
}
