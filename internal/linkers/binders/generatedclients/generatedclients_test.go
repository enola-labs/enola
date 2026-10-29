package generatedclients

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

func specOp(repo, specFile, path, method, id string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindRoute, Name: path, File: specFile, Repo: repo,
		Props: map[string]any{
			facts.PropSource: facts.RouteSourceOpenAPI, facts.PropRole: facts.RoleClient,
			"method": method, "operationId": id, "spec_file": specFile,
			"gateway_path": "/orders" + path,
		},
	}
}

func symbol(repo, name, file, kind string, generated bool, rels ...facts.Relation) facts.Fact {
	props := map[string]any{"symbol_kind": kind}
	if generated {
		props["generated"] = true
	}
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Repo: repo, Line: 10, Props: props, Relations: rels}
}

func calls(target string) facts.Relation { return facts.Relation{Kind: facts.RelCalls, Target: target} }
func injects(target string) facts.Relation {
	return facts.Relation{Kind: facts.RelInjects, Target: target}
}

func dep(repo, file, target string) facts.Fact {
	return facts.Fact{Kind: facts.KindDependency, Name: "mod -> " + target, File: file, Repo: repo}
}

func routesBy(store *facts.Store, source string) []facts.Fact {
	var out []facts.Fact
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.PropString(facts.PropSource) == source {
			out = append(out, f)
		}
	}
	return out
}

// A client generated at build time: the class injecting the generated API type and
// calling an operation by name is the call site.
func TestImportedClient(t *testing.T) {
	const file = "feature/orders/OrdersRepository.kt"
	store := facts.NewStore()
	store.Add(
		specOp("app", "feature/src/main/openapi/orders-api.yaml", "/orders/{id}", "GET", "GetOrder"),
		specOp("app", "feature/src/main/openapi/orders-api.yaml", "/orders", "POST", "createOrder"),
		specOp("app", "other/src/main/openapi/orders-api.yaml", "/orders/{id}", "GET", "getOrder"),
		specOp("app", "feature/src/main/openapi/billing.yaml", "/invoices", "GET", "listInvoices"),
		dep("app", file, "com.acme.feature.ordersapiedge.apis.OrdersApi"),
		dep("app", "feature/orders/OrdersRepositoryTest.kt", "com.acme.feature.ordersapi.apis.OrdersApi"),
		symbol("app", "feature/orders.OrdersRepository", file, facts.SymbolClass, false),
		symbol("app", "feature/orders.OrdersRepository.load", file, facts.SymbolMethod, false, calls("getOrder"), calls("map")),
	)
	bind(t, store)
	bind(t, store)

	got := routesBy(store, RouteSource)
	if len(got) != 1 {
		t.Fatalf("got %d call-site routes, want 1: %+v", len(got), got)
	}
	r := got[0]
	if r.Name != "/orders/{id}" || r.PropString("method") != "GET" || r.File != file ||
		r.PropString(facts.PropCaller) != "feature/orders.OrdersRepository.load" ||
		r.PropString("spec_file") != "feature/src/main/openapi/orders-api.yaml" ||
		r.PropString("gateway_path") != "/orders/orders/{id}" {
		t.Errorf("unexpected route %+v", r)
	}
	for _, f := range routesBy(store, facts.RouteSourceOpenAPI) {
		want := f.PropString("spec_file") == "feature/src/main/openapi/orders-api.yaml"
		if f.PropBool(facts.PropSpecDeclaration) != want {
			t.Errorf("%s %s declaration = %v, want %v", f.PropString("spec_file"), f.Name, !want, want)
		}
	}
}

// A client generated into the checkout: hand-written calls to the generated package's
// methods are the call sites; the generated code and the mock are not.
func TestInRepoClient(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		specOp("svc", "api/client/orders.json", "/orders/{id}", "GET", "getOrder"),
		specOp("svc", "api/client/orders.json", "/orders", "GET", "list-orders"),
		specOp("svc", "api/client/orders.json", "/orders", "POST", ""),
		symbol("svc", "pkg/ordersapi.ClientWithResponses.GetOrderWithResponse", "pkg/ordersapi/client.gen.go", facts.SymbolMethod, true),
		symbol("svc", "pkg/ordersapi.ClientWithResponses.ListOrdersWithResponse", "pkg/ordersapi/client.gen.go", facts.SymbolMethod, true),
		symbol("svc", "pkg/ordersapi.Client.PostOrdersWithBody", "pkg/ordersapi/client.gen.go", facts.SymbolMethod, true,
			calls("pkg/ordersapi.Client.GetOrder")),
		symbol("svc", "internal/orders.client.Fetch", "internal/orders/client.go", facts.SymbolMethod, false,
			calls("pkg/ordersapi.ClientWithResponsesInterface.GetOrderWithResponse")),
		symbol("svc", "internal/orders.client.Create", "internal/orders/client.go", facts.SymbolMethod, false,
			calls("pkg/ordersapi.ClientWithResponsesInterface.PostOrdersWithBodyWithResponse")),
		symbol("svc", "pkg/ordersapi/mocks.MockClientWithResponsesInterface.GetOrderWithResponse", "pkg/ordersapi/mocks/mock.gen.go", facts.SymbolMethod, true),
		symbol("svc", "pkg/ordersapi/mocks.MockClientWithResponsesInterface.ListOrdersWithResponse", "pkg/ordersapi/mocks/mock.gen.go", facts.SymbolMethod, true),
		symbol("svc", "pkg/ordersapi/mocks.MockClientWithResponsesInterface.PostOrdersWithBodyWithResponse", "pkg/ordersapi/mocks/mock.gen.go", facts.SymbolMethod, true),
		symbol("svc", "internal/orders/mocks.Mock.Fetch", "internal/orders/mocks/mock.go", facts.SymbolMethod, true,
			calls("pkg/ordersapi.ClientWithResponsesInterface.ListOrdersWithResponse")),
	)
	bind(t, store)

	got := map[string]string{}
	for _, r := range routesBy(store, RouteSource) {
		got[r.PropString("method")+" "+r.Name] = r.PropString(facts.PropCaller)
	}
	want := map[string]string{
		"GET /orders/{id}": "internal/orders.client.Fetch",
		"POST /orders":     "internal/orders.client.Create",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s caller = %q, want %q", k, got[k], v)
		}
	}
	for _, f := range routesBy(store, facts.RouteSourceOpenAPI) {
		if !f.PropBool(facts.PropSpecDeclaration) {
			t.Errorf("%s not marked as a declaration", f.Name)
		}
	}
}

// No code seen calling the client: the spec's operations stay as they are, and a
// package segment that only happens to equal a short spec name is not a client.
func TestNoEvidenceLeavesSpec(t *testing.T) {
	const file = "feature/Repo.kt"
	store := facts.NewStore()
	store.Add(
		specOp("app", "openapi/api.yaml", "/things", "GET", "getThings"),
		dep("app", file, "com.acme.api.Things"),
		symbol("app", "feature.Repo.load", file, facts.SymbolMethod, false, calls("getThings")),
	)
	bind(t, store)
	if got := routesBy(store, RouteSource); len(got) != 0 {
		t.Errorf("got %d routes, want none", len(got))
	}
	for _, f := range routesBy(store, facts.RouteSourceOpenAPI) {
		if f.PropBool(facts.PropSpecDeclaration) {
			t.Errorf("%s marked without evidence", f.Name)
		}
	}
}

func TestSpecKey(t *testing.T) {
	for in, want := range map[string]string{
		"a/svc-orders-api-edge.yaml": "svcordersapiedge", "b/Orders_API.json": "ordersapi",
	} {
		if got := specKey(in); got != want {
			t.Errorf("specKey(%q) = %q, want %q", in, got, want)
		}
	}
}
