package generatedclients

import (
	"context"
	"strings"
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
		Kind: facts.KindRoute, Name: path, File: repo + "/" + specFile, Repo: repo,
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
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: repo + "/" + file, Repo: repo, Line: 10, Props: props, Relations: rels}
}

func calls(target string) facts.Relation { return facts.Relation{Kind: facts.RelCalls, Target: target} }

func dep(repo, file, target string) facts.Fact {
	return facts.Fact{Kind: facts.KindDependency, Name: "mod -> " + target, File: repo + "/" + file, Repo: repo}
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
		specOp("app", "feature/src/main/openapi/overlay/orders-api-edge.yaml", "/orders/{id}", "GET", "getOrder"),
		specOp("app", "other/src/main/openapi/orders-api.yaml", "/orders/{id}", "GET", "getOrder"),
		specOp("app", "feature/src/main/openapi/billing.yaml", "/invoices", "GET", "listInvoices"),
		dep("app", file, "com.acme.feature.ordersapiedge.apis.OrdersApi"),
		dep("app", file, "com.acme.feature.ordersapi.apis.OrdersBaseApi"),
		dep("app", "feature/orders/OrdersRepositoryTest.kt", "com.acme.feature.ordersapi.apis.OrdersApi"),
		symbol("app", "feature/orders.OrdersRepository", file, facts.SymbolClass, false),
		symbol("app", "feature/orders.OrdersRepository.load", file, facts.SymbolMethod, false, calls("getOrder"), calls("map")),
	)
	bind(t, store)
	bind(t, store)

	got := routesBy(store, RouteSource)
	if len(got) != 2 {
		t.Fatalf("got %d call-site routes, want 2: %+v", len(got), got)
	}
	r := got[0]
	if r.Name != "/orders/{id}" || r.PropString("method") != "GET" || r.File != "app/"+file ||
		r.PropString(facts.PropCaller) != "feature/orders.OrdersRepository.load" ||
		r.PropString("spec_file") != "feature/src/main/openapi/orders-api.yaml" && r.PropString("spec_file") != "feature/src/main/openapi/overlay/orders-api-edge.yaml" ||
		r.PropString("gateway_path") != "/orders/orders/{id}" {
		t.Errorf("unexpected route %+v", r)
	}
	for _, f := range routesBy(store, facts.RouteSourceOpenAPI) {
		want := strings.HasPrefix(f.PropString("spec_file"), "feature/") && strings.Contains(f.PropString("spec_file"), "orders-api")
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

// A spec read as served that the repo only calls, through a client generated into
// the checkout, is a client spec; one a hand-written type implements stays served.
func TestServedSpecTheRepoCalls(t *testing.T) {
	served := func(specFile, path, id string) facts.Fact {
		f := specOp("svc", specFile, path, "GET", id)
		f.Props[facts.PropRole] = facts.RoleServer
		return f
	}
	store := facts.NewStore()
	store.Add(
		served("api/entities.yml", "/predict", "predict"),
		served("api/entities.yml", "/versions", "listVersions"),
		served("api/entities.yml", "/healthz", "isLive"),
		served("api/entities.yml", "/readyz", "isReady"),
		served("api/service.yml", "/items", "items"),
		served("api/service.yml", "/healthz", "isLive"),
		symbol("svc", "internal/entities/api.ClientWithResponses.PredictWithResponse", "internal/entities/api/contract.gen.go", facts.SymbolMethod, true),
		symbol("svc", "internal/entities.client.Predict", "internal/entities/client.go", facts.SymbolMethod, false,
			calls("internal/entities/api.ClientWithResponses.PredictWithResponse")),
		symbol("svc", "internal/server/api.ServerInterface.Items", "internal/server/api/server.gen.go", facts.SymbolMethod, true),
		symbol("svc", "internal/server/api.ServerInterface.IsLive", "internal/server/api/server.gen.go", facts.SymbolMethod, true),
		symbol("svc", "internal/server.Server.Items", "internal/server/items.go", facts.SymbolMethod, false,
			calls("internal/server/api.ServerInterface.IsLive")),
		symbol("svc", "internal/server.Server.IsLive", "internal/server/health.go", facts.SymbolMethod, false),
	)
	bind(t, store)
	bind(t, store)

	for _, f := range routesBy(store, facts.RouteSourceOpenAPI) {
		wantClient := f.PropString("spec_file") == "api/entities.yml"
		if got := f.PropString(facts.PropRole) == facts.RoleClient; got != wantClient {
			t.Errorf("%s %s: client = %v, want %v", f.PropString("spec_file"), f.Name, got, wantClient)
		}
		if wantClient && !f.PropBool(facts.PropSpecDeclaration) {
			t.Errorf("%s not a declaration", f.Name)
		}
	}
	got := routesBy(store, RouteSource)
	if len(got) != 1 || got[0].Name != "/predict" || got[0].PropString(facts.PropCaller) != "internal/entities.client.Predict" {
		t.Errorf("call sites = %+v", got)
	}
}
