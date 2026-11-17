package vendoredspecs

// The binder answers one question per repo — can this repo serve HTTP at all? — and
// rewrites its OpenAPI routes accordingly. The cases below are the two estates that
// forced the rule's shape: a mobile app whose vendored specs must NOT be served
// endpoints, and a spec-first backend whose identical-looking specs must stay served.

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

func route(repo, name, source, role string) facts.Fact {
	props := map[string]any{facts.PropSource: source, "method": "GET"}
	if role != "" {
		props[facts.PropRole] = role
	}
	return facts.Fact{Kind: facts.KindRoute, Name: name, Repo: repo, Props: props}
}

// roleOf returns the role and vendored-spec mark of the first route fact with the
// given name, so a case can assert on the outcome without re-deriving indices.
func roleOf(t *testing.T, store *facts.Store, name string) (string, bool) {
	t.Helper()
	for _, f := range store.All() {
		if f.Name == name {
			v, _ := f.Props[PropVendoredSpec].(bool)
			return f.PropString(facts.PropRole), v
		}
	}
	t.Fatalf("no fact named %q in store", name)
	return "", false
}

// A mobile app vendors the specs of services it calls. Retrofit call sites are the
// positive marker; nothing outside a spec declares a served endpoint.
func TestDemotesVendoredSpecsInNativeApp(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		route("mobile-app", "/downstream/widgets", facts.RouteSourceRetrofit, facts.RoleClient),
		route("mobile-app", "/widgets/{id}/details", facts.RouteSourceOpenAPI, facts.RoleServer),
		route("mobile-app", "/sessions/{id}", facts.RouteSourceOpenAPI, facts.RoleServer),
	)

	bind(t, store)

	for _, name := range []string{"/widgets/{id}/details", "/sessions/{id}"} {
		role, vendored := roleOf(t, store, name)
		if role != facts.RoleClient {
			t.Errorf("%s: role = %q, want %q", name, role, facts.RoleClient)
		}
		if !vendored {
			t.Errorf("%s: %s not set — the rewrite must be auditable", name, PropVendoredSpec)
		}
	}
}

// The regression that matters most. A spec-first Go service declares its whole served
// surface in api/openapi/*.yml and nothing in code, so it looks exactly like the mobile
// case to any rule keyed on "server routes come only from OpenAPI". The native-app
// marker is what tells them apart.
func TestSpecFirstBackendUntouched(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		route("spec-first-svc", "/v1/query", facts.RouteSourceOpenAPI, facts.RoleServer),
		route("spec-first-svc", "/v1/records", facts.RouteSourceOpenAPI, facts.RoleServer),
	)

	bind(t, store)

	for _, name := range []string{"/v1/query", "/v1/records"} {
		role, vendored := roleOf(t, store, name)
		if role != facts.RoleServer {
			t.Errorf("%s: role = %q, want %q — a spec-first backend still serves its spec", name, role, facts.RoleServer)
		}
		if vendored {
			t.Errorf("%s: %s was set on a repo with no native-app client", name, PropVendoredSpec)
		}
	}
}

// A Kotlin repo that both calls out via Retrofit and serves via Ktor is a server. The
// second condition is what keeps it out of the set.
func TestNativeClientThatAlsoServesUntouched(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		route("jvm-svc", "/downstream/items", facts.RouteSourceRetrofit, facts.RoleClient),
		route("jvm-svc", "/internal/jobs", "ktor", facts.RoleServer),
		route("jvm-svc", "/v1/orders", facts.RouteSourceOpenAPI, facts.RoleServer),
	)

	bind(t, store)

	if role, _ := roleOf(t, store, "/v1/orders"); role != facts.RoleServer {
		t.Errorf("role = %q, want %q — the repo serves endpoints outside its spec", role, facts.RoleServer)
	}
}

// A route with no role is a server route (see the facts.RoleServer contract), so a repo
// whose only served declarations are role-less still counts as a server.
func TestRoleLessRouteCountsAsServing(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		route("app", "/downstream/items", facts.RouteSourceRetrofit, facts.RoleClient),
		route("app", "/pages/home", "rails", ""), // no role prop at all
		route("app", "/v1/orders", facts.RouteSourceOpenAPI, facts.RoleServer),
	)

	bind(t, store)

	if role, _ := roleOf(t, store, "/v1/orders"); role != facts.RoleServer {
		t.Errorf("role = %q, want %q — a role-less route means the repo serves something", role, facts.RoleServer)
	}
}

// Every binder re-runs on every snapshot and append, so a second pass over its own
// output must be a no-op rather than flipping the role back and forth.
func TestIdempotentAcrossReruns(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		route("mobile-app", "/downstream/items", facts.RouteSourceRetrofit, facts.RoleClient),
		route("mobile-app", "/v1/orders", facts.RouteSourceOpenAPI, facts.RoleServer),
	)

	bind(t, store)
	first, _ := roleOf(t, store, "/v1/orders")
	bind(t, store)
	bind(t, store)
	second, vendored := roleOf(t, store, "/v1/orders")

	if first != facts.RoleClient || second != first {
		t.Errorf("role drifted across reruns: %q then %q", first, second)
	}
	if !vendored {
		t.Errorf("%s cleared by a rerun that changed nothing", PropVendoredSpec)
	}
}

// The reverse direction. A repo that grows a real server must not keep a demotion made
// when it had none — the prop is what makes the rewrite reversible.
func TestRestoresWhenRepoStartsServing(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		route("mobile-app", "/downstream/items", facts.RouteSourceRetrofit, facts.RoleClient),
		route("mobile-app", "/v1/orders", facts.RouteSourceOpenAPI, facts.RoleServer),
	)
	bind(t, store)
	if role, _ := roleOf(t, store, "/v1/orders"); role != facts.RoleClient {
		t.Fatalf("setup: role = %q, want the route demoted first", role)
	}

	store.Add(route("mobile-app", "/internal/jobs", "ktor", facts.RoleServer))
	bind(t, store)

	role, vendored := roleOf(t, store, "/v1/orders")
	if role != facts.RoleServer {
		t.Errorf("role = %q, want %q restored", role, facts.RoleServer)
	}
	if vendored {
		t.Errorf("%s survived a restore", PropVendoredSpec)
	}
}

// A spec the extractor itself classified as a client spec (the openapi/client/
// convention) carries no vendored mark, and must never be "restored" into a server
// route it was never extracted as.
func TestGenuineClientSpecNeverPromoted(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		route("go-svc", "/v1/items", facts.RouteSourceOpenAPI, facts.RoleClient),
		route("go-svc", "/v1/filters", facts.RouteSourceOpenAPI, facts.RoleServer),
	)

	bind(t, store)

	role, vendored := roleOf(t, store, "/v1/items")
	if role != facts.RoleClient {
		t.Errorf("role = %q, want %q left alone", role, facts.RoleClient)
	}
	if vendored {
		t.Errorf("%s set on a spec the extractor already classified as client", PropVendoredSpec)
	}
}

func specRoute(repo, name, file, title, host string) facts.Fact {
	f := route(repo, name, facts.RouteSourceOpenAPI, facts.RoleServer)
	f.File, f.RepoPrefixed = repo+"/"+file, true
	f.Props["spec_file"] = file
	if title != "" {
		f.Props[facts.PropSpecTitle] = title
	}
	if host != "" {
		f.Props[facts.PropSpecServerHost] = host
	}
	return f
}

// A backend keeps copies of the specs of services it calls beside its own, and a
// registry repository keeps every service's. A copy names its service: in its title,
// its server's host, or the directory it sits in. Only a loaded repository's exact
// name counts, and the repository's own name never does.
func TestDemotesASpecThatDescribesAnotherLoadedRepo(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		specRoute("orders", "/v1/orders/{id}", "api/openapi/service.yml", "orders API", "orders.internal.example"),
		specRoute("orders", "/v1/conversations/{id}", "api/openapi/chat.yml", "chat main-app OpenAPI", ""),
		specRoute("orders", "/v1/fees", "api/openapi/fees.yml", "", "pricing.example.com"),
		specRoute("registry", "/v1/orders/{id}/lines", "apis/orders/v1/spec.yml", "Orders", ""),
		specRoute("orders", "/v1/quotes", "api/openapi/quote.yml", "chatty quotes", "quotes.example.com"),
		route("chat", "/v1/conversations/{id}", facts.RouteSourceRetrofit, facts.RoleClient),
		route("pricing", "/health", "go-http", facts.RoleServer),
		route("registry", "/health", "go-http", facts.RoleServer),
	)
	bind(t, store)
	for name, want := range map[string]string{
		"/v1/orders/{id}":        facts.RoleServer, // its own spec
		"/v1/conversations/{id}": facts.RoleClient, // titled after chat
		"/v1/fees":               facts.RoleClient, // served from pricing's host
		"/v1/orders/{id}/lines":  facts.RoleClient, // filed under orders in a registry
		"/v1/quotes":             facts.RoleServer, // no loaded repository named
	} {
		if role, _ := roleOf(t, store, name); role != want {
			t.Errorf("%s: role = %q, want %q", name, role, want)
		}
	}
}
