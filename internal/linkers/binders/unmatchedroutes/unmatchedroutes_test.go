package unmatchedroutes

// The binder tags each client call site that resolves to no loaded server route with
// unmatched_by_server + a reason, so the aggregate coverage count becomes a queryable
// per-call list. This is the observability half of the coverage pass.

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/internal/providers"
)

func bind(t *testing.T, store *facts.Store) {
	t.Helper()
	if err := New(vocab.Default()).Bind(context.Background(), store); err != nil {
		t.Fatalf("Bind: %v", err)
	}
}

func clientRouteFact(repo, name, method string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: name, Repo: repo,
		Props: map[string]any{facts.PropRole: facts.RoleClient, "method": method}}
}

func serverRouteFact(repo, name, method string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: name, Repo: repo,
		Props: map[string]any{facts.PropRole: facts.RoleServer, "method": method}}
}

func TestFlagUnmatchedRoutes_ClientSide(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		clientRouteFact("app", "/api/items/{id}", "GET"),   // resolves to backend
		clientRouteFact("app", "/api/unknown/{id}", "GET"), // no server serves it -> path_unknown
		clientRouteFact("app", "/api/orders/{id}", "POST"), // path served, wrong verb -> method_mismatch
		clientRouteFact("app", "/health", "GET"),           // sub-2-segment path -> generic_path
		serverRouteFact("backend", "/api/items/{itemId}", "GET"),
		serverRouteFact("backend", "/api/orders/{orderId}", "GET"),
	)

	bind(t, store)

	props := map[string]map[string]any{}
	for _, f := range store.All() {
		if f.Kind == facts.KindRoute {
			props[f.Name] = f.Props
		}
	}

	// Each reason the resolver can emit for an unresolved client call, asserted by name
	// so the value set stays pinned to the crossrepo.Reason* constants.
	for _, tc := range []struct {
		name, wantReason string
	}{
		{"/api/unknown/{id}", "path_unknown"},
		{"/api/orders/{id}", "method_mismatch"},
		{"/health", "generic_path"},
	} {
		got := props[tc.name]
		if e, _ := got[PropUnmatchedByServer].(bool); !e {
			t.Errorf("%s should be unmatched_by_server; got %+v", tc.name, got)
		}
		if got[PropUnmatchedReason] != tc.wantReason {
			t.Errorf("%s reason = %v, want %s", tc.name, got[PropUnmatchedReason], tc.wantReason)
		}
	}

	items := props["/api/items/{id}"]
	if _, has := items[PropUnmatchedByServer]; has {
		t.Errorf("resolved client call must not be flagged; got %+v", items)
	}

	// The server route must never carry the client-side flag.
	server := props["/api/items/{itemId}"]
	if _, has := server[PropUnmatchedByServer]; has {
		t.Errorf("server route must not carry unmatched_by_server; got %+v", server)
	}
}

// TestFlagUnmatchedRoutes_ClearsStaleVerdict pins the idempotency property the binder
// depends on across appends: loading a second repo can RESOLVE a call site that was
// unmatched when only one repo was in the store, and the stale prop must not survive
// that. Without the clearing branches a re-link would leave every route carrying the
// verdict from whichever append first saw it.
func TestFlagUnmatchedRoutes_ClearsStaleVerdict(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		clientRouteFact("app", "/api/items/{id}", "GET"),
		// A second repo so the linker engages at all (it is a no-op single-repo).
		serverRouteFact("other", "/api/unrelated/{id}", "GET"),
	)

	bind(t, store)

	var flagged bool
	for _, f := range store.All() {
		if f.Name == "/api/items/{id}" {
			flagged = f.PropBool(PropUnmatchedByServer)
		}
	}
	if !flagged {
		t.Fatal("precondition: the call site should be unmatched with no server serving it")
	}

	// Now the backend appears, as it would on a second append.
	store.Add(serverRouteFact("backend", "/api/items/{itemId}", "GET"))
	bind(t, store)

	for _, f := range store.All() {
		if f.Name != "/api/items/{id}" {
			continue
		}
		if _, has := f.Props[PropUnmatchedByServer]; has {
			t.Errorf("stale unmatched_by_server survived a re-link that resolved the call; props=%+v", f.Props)
		}
		if _, has := f.Props[PropUnmatchedReason]; has {
			t.Errorf("stale unmatched_reason survived a re-link; props=%+v", f.Props)
		}
	}
}

// The server-side mirror of ClearsStaleVerdict, and the one the positive marker
// broke on arrival. A route matched while one set of repos was loaded, then found
// unmatched once a later append changed the index, kept saying both — 3,433 routes
// of the estate carried the pair, and a reader filtering on either marker got a
// population that overlapped the other by half.
func TestFlagUnmatchedRoutes_VerdictsAreMutuallyExclusiveAcrossAppends(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		// The first call keeps backend an HTTP provider throughout, so the second
		// route's verdict is about that route rather than about the repo.
		clientRouteFact("app", "/api/items/{id}", "GET"),
		clientRouteFact("app", "/api/other/{id}", "GET"),
		serverRouteFact("backend", "/api/items/{id}", "GET"),
		serverRouteFact("backend", "/api/other/{id}", "GET"),
	)
	bind(t, store)

	for _, f := range store.All() {
		if f.Repo == "backend" && !f.PropBool(PropMatchedByClients) {
			t.Fatalf("precondition: %s should be matched; props=%+v", f.Name, f.Props)
		}
	}

	// The caller moves off /api/other, as a later append would show.
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Repo == "app" && f.Name == "/api/other/{id}" {
			f.Name = "/api/third/{id}"
		}
	})
	bind(t, store)

	for _, f := range store.All() {
		if f.Repo != "backend" || f.Name != "/api/other/{id}" {
			continue
		}
		if !f.PropBool(PropUnmatchedByClients) {
			t.Errorf("the route lost its caller and should be unmatched; props=%+v", f.Props)
		}
		if _, has := f.Props[PropMatchedByClients]; has {
			t.Errorf("a route cannot be both matched and unmatched; props=%+v", f.Props)
		}
	}
}

// A route the linker declined to reason about carries no verdict in either
// direction. The else-branch used to claim every one of them as matched, which is
// how 2,997 unexamined routes entered the denominator of a coverage proportion.
func TestFlagUnmatchedRoutes_DeclinedRoutesCarryNoVerdict(t *testing.T) {
	page := serverRouteFact("backend", "/dashboard", "GET")
	page.Props["type"] = "page"
	verbless := serverRouteFact("backend", "/api/webhooks/stripe", "")

	store := facts.NewStore()
	store.Add(
		clientRouteFact("app", "/api/items/{id}", "GET"),
		serverRouteFact("backend", "/api/items/{id}", "GET"),
		page,
		verbless,
	)
	bind(t, store)

	for _, f := range store.All() {
		if f.Name != "/dashboard" && f.Name != "/api/webhooks/stripe" {
			continue
		}
		if _, has := f.Props[PropMatchedByClients]; has {
			t.Errorf("%s was never evaluated and must not read as matched; props=%+v", f.Name, f.Props)
		}
		if _, has := f.Props[PropUnmatchedByClients]; has {
			t.Errorf("%s was never evaluated and must not read as unmatched; props=%+v", f.Name, f.Props)
		}
	}
}

// A route identity is repo + method + path, and two facts can share one: an Ember
// page route declared on the same path as the Rails route beneath it. Deciding a
// verdict by identity alone hands the page route whatever the served route got —
// which put unmatched_by_clients on `/*path` in the estate, the exact outcome
// IsUIRoute exists to prevent.
func TestFlagUnmatchedRoutes_ColocatedPageRouteKeepsNoVerdict(t *testing.T) {
	page := serverRouteFact("backend", "/reports/{id}", "GET")
	page.Props["type"] = "page"

	store := facts.NewStore()
	store.Add(
		clientRouteFact("app", "/api/items/{id}", "GET"),
		serverRouteFact("backend", "/api/items/{id}", "GET"),
		serverRouteFact("backend", "/reports/{id}", "GET"), // served, no caller
		page, // same identity, not served
	)
	bind(t, store)

	var seen bool
	for _, f := range store.All() {
		if f.PropString("type") != "page" {
			continue
		}
		seen = true
		if _, has := f.Props[PropUnmatchedByClients]; has {
			t.Errorf("a page route must not inherit the served route's unused verdict; props=%+v", f.Props)
		}
		if _, has := f.Props[PropMatchedByClients]; has {
			t.Errorf("a page route must not inherit a matched verdict either; props=%+v", f.Props)
		}
	}
	if !seen {
		t.Fatal("precondition: the page route should be in the store")
	}
	for _, f := range store.All() {
		if f.Name == "/reports/{id}" && f.PropString("type") == "" && !f.PropBool(PropUnmatchedByClients) {
			t.Errorf("the served route on the same path should still be flagged; props=%+v", f.Props)
		}
	}
}

func graphqlRouteFact(repo, name, role string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: name, Repo: repo,
		Props: map[string]any{facts.PropRole: role, facts.PropRouteType: facts.RouteTypeGraphQL}}
}

// The GraphQL seam had a join and no verdict: the signal resolved client
// operations against loaded schemas, kept per-client coverage counters, and
// recorded nothing about whether a DECLARED operation is consumed. 201
// operations across the estate carried no marker at all, which reads exactly
// like a surface nothing can assess.
func TestFlagUnmatchedRoutes_GraphQLOperationsGetTheSameVerdicts(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		graphqlRouteFact("app", "Query.candidates", facts.RoleClient),
		graphqlRouteFact("api", "Query.candidates", facts.RoleServer),
		graphqlRouteFact("api", "Mutation.retireThis", facts.RoleServer),
	)
	bind(t, store)

	for _, f := range store.All() {
		if f.Repo != "api" {
			continue
		}
		switch f.Name {
		case "Query.candidates":
			if !f.PropBool(PropMatchedByClients) {
				t.Errorf("a selected operation is matched; props=%+v", f.Props)
			}
		case "Mutation.retireThis":
			if !f.PropBool(PropUnmatchedByClients) {
				t.Errorf("an operation no client selects is unmatched; props=%+v", f.Props)
			}
		}
	}
}

// A schema no loaded client selects anything from is not a schema of unused
// operations — it is a schema nobody asked about. Same vacuity rule the HTTP
// side applies to a repo that serves no cross-repo client.
func TestFlagUnmatchedRoutes_UnconsumedSchemaGetsNoVerdict(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		graphqlRouteFact("app", "Query.candidates", facts.RoleClient),
		graphqlRouteFact("api", "Query.candidates", facts.RoleServer),
		graphqlRouteFact("lonely", "Query.nobodyAsks", facts.RoleServer),
		graphqlRouteFact("lonely", "Mutation.norThis", facts.RoleServer),
	)
	bind(t, store)

	for _, f := range store.All() {
		if f.Repo != "lonely" {
			continue
		}
		if _, has := f.Props[PropUnmatchedByClients]; has {
			t.Errorf("%s: every operation unused describes the snapshot, not the schema; props=%+v",
				f.Name, f.Props)
		}
		if _, has := f.Props[PropMatchedByClients]; has {
			t.Errorf("%s: nothing selected it, so it is not matched either; props=%+v", f.Name, f.Props)
		}
	}
}

func TestFlagUnmatchedRoutes_RuntimeObservationsCarryNoVerdict(t *testing.T) {
	store := facts.NewStore()
	observation := facts.Fact{Kind: facts.KindRoute, Name: "runtime-route: GET /api/orders",
		Repo: "backend", File: ".enola-runtime/boot.json",
		Props: map[string]any{
			providers.PropResolutionLevel: providers.LevelRuntimeObserved,
			providers.PropObservedVia:     "rails-boot",
			"method":                      "GET",
			"path":                        "/api/orders",
			PropUnmatchedByClients:        true,
		}}
	store.Add(
		serverRouteFact("backend", "/api/orders/{orderId}", "GET"),
		observation,
	)
	bind(t, store)
	for _, f := range store.FactsRef() {
		if f.Name != "runtime-route: GET /api/orders" {
			continue
		}
		if _, claimed := f.Props[PropUnmatchedByClients]; claimed {
			t.Errorf("an observation must carry no linker verdict, got %+v", f.Props)
		}
		if _, claimed := f.Props[PropMatchedByClients]; claimed {
			t.Errorf("an observation must carry no linker verdict, got %+v", f.Props)
		}
	}
}

// A resolved call names the server route it reaches, and stops naming it once a
// later append makes the call ambiguous: the positive half is idempotent across
// appends for the same reason the markers are.
func TestFlagUnmatchedRoutes_MatchedRoutesFollowTheLinker(t *testing.T) {
	store := facts.NewStore()
	call := clientRouteFact("web", "/api/items/{id}", "GET")
	call.File = "web/src/api.ts"
	server := serverRouteFact("backend", "/api/items/{itemId}", "GET")
	server.File = "backend/items.go"
	store.Add(call, server)

	bind(t, store)

	matched := func() []any {
		for _, f := range store.All() {
			if f.Name == "/api/items/{id}" {
				refs, _ := f.Props[facts.PropMatchedRoutes].([]any)
				return refs
			}
		}
		return nil
	}
	refs := matched()
	if len(refs) != 1 {
		t.Fatalf("matched_routes = %v, want one entry", refs)
	}
	entry, _ := refs[0].(map[string]any)
	if entry["repo"] != "backend" || entry["name"] != "/api/items/{itemId}" || entry["file"] != "backend/items.go" || entry["method"] != "GET" {
		t.Fatalf("entry = %v, want the backend route's identity", entry)
	}

	// A second repo serving the same path leaves nothing to choose between them.
	replica := serverRouteFact("replica", "/api/items/{itemId}", "GET")
	replica.File = "replica/items.go"
	store.Add(replica)
	bind(t, store)

	if refs := matched(); refs != nil {
		t.Fatalf("stale matched_routes survived a re-link that made the call ambiguous: %v", refs)
	}
}
