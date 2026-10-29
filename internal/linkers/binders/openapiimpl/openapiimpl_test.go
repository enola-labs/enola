package openapiimpl

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
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
