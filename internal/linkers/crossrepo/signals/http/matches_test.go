package http

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	"github.com/enola-labs/enola/internal/linkers/vocab"
)

func servedIn(repo, path, method, file string) facts.Fact {
	f := servedBy(repo, path, method)
	f.File = file
	return f
}

// The case a name cannot answer: the call and the route it reaches share a name, and
// from the calling repo that name resolves to the call itself. The match names the
// server route by repo and file.
func TestClientRouteMatches_NamesTheServerRouteNotTheCall(t *testing.T) {
	server := servedIn("gateway", "/v1/catalog/imports", "POST", "gateway/src/catalog.controller.ts")
	call := configuredCall("/v1/catalog/imports", "POST", "resource-api")
	call.File, call.Line = "sdk/src/connector.ts", 21

	got := ClientRouteMatches(routeindex.New(vocab.Default()), []facts.Fact{server, call})[ClientCallKey(call)]
	want := RouteMatch{Repo: "gateway", Method: "POST", Name: "/v1/catalog/imports",
		File: "gateway/src/catalog.controller.ts", Confidence: "verified"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("matches = %+v, want [%+v]", got, want)
	}
}

// A literal segment absorbed by a server parameter links, but never as verified: the
// same grading the service edge gives it.
func TestClientRouteMatches_ParameterMatchIsProbable(t *testing.T) {
	server := servedIn("gateway", "/v1/resources/:type/items", "GET", "gateway/src/resources.controller.ts")
	call := configuredCall("/v1/resources/catalog/items", "GET", "resource-api")

	got := ClientRouteMatches(routeindex.New(paramVocab(true)), []facts.Fact{server, call})[ClientCallKey(call)]
	if len(got) != 1 || got[0].Name != "/v1/resources/:type/items" || got[0].Confidence != "probable" {
		t.Fatalf("matches = %+v, want one probable match on the parameterized route", got)
	}
}

// Two repositories serve the path and nothing chooses between them. The linker draws
// no edge, so no match is recorded either: a link to every candidate would state a
// call the linker itself declined to state.
func TestClientRouteMatches_AmbiguousProviderRecordsNothing(t *testing.T) {
	gateway := servedIn("gateway", "/v1/catalog/imports", "POST", "gateway/a.ts")
	replica := servedIn("replica", "/v1/catalog/imports", "POST", "replica/a.ts")
	call := plainCall("web", "POST", "/v1/catalog/imports")

	got := ClientRouteMatches(routeindex.New(vocab.Default()), []facts.Fact{gateway, replica, call})
	if m := got[ClientCallKey(call)]; len(m) != 0 {
		t.Fatalf("an ambiguous call was linked: %+v", m)
	}
}

// A service alias chooses the provider, and only its route is recorded.
func TestClientRouteMatches_AliasNarrowsToTheProvider(t *testing.T) {
	gateway := servedIn("gateway", "/v1/catalog/imports", "POST", "gateway/a.ts")
	replica := servedIn("replica", "/v1/catalog/imports", "POST", "replica/a.ts")
	call := configuredCall("/v1/catalog/imports", "POST", "resource-api")
	v := vocab.Default()
	v.ServiceAliases = map[string]string{"resource-api": "gateway"}

	got := ClientRouteMatches(routeindex.New(v), []facts.Fact{gateway, replica, call})[ClientCallKey(call)]
	if len(got) != 1 || got[0].Repo != "gateway" {
		t.Fatalf("matches = %+v, want only the aliased gateway route", got)
	}
}

// A call its own repository serves draws no service edge, but it does reach that
// route, so the match is kept.
func TestClientRouteMatches_KeepsASelfServedCall(t *testing.T) {
	own := servedIn("web", "/v1/search/items", "GET", "web/server/search.ts")
	other := servedIn("api", "/v1/other/thing", "GET", "api/other.ts")
	call := plainCall("web", "GET", "/v1/search/items")

	got := ClientRouteMatches(routeindex.New(vocab.Default()), []facts.Fact{own, other, call})[ClientCallKey(call)]
	if len(got) != 1 || got[0].Repo != "web" {
		t.Fatalf("matches = %+v, want the call's own route", got)
	}
}

// Two call sites of one verb and path are resolved and keyed separately.
func TestClientRouteMatches_KeysEachCallSite(t *testing.T) {
	server := servedIn("gateway", "/v1/catalog/imports", "POST", "gateway/a.ts")
	first := plainCall("web", "POST", "/v1/catalog/imports")
	first.File, first.Line = "web/a.ts", 3
	second := first
	second.File = "web/b.ts"

	got := ClientRouteMatches(routeindex.New(vocab.Default()), []facts.Fact{server, first, second})
	if len(got[ClientCallKey(first)]) != 1 || len(got[ClientCallKey(second)]) != 1 {
		t.Fatalf("each call site should carry its own match: %+v", got)
	}
}

func TestClientRouteMatches_SingleRepoIsNil(t *testing.T) {
	server := servedIn("web", "/v1/search/items", "GET", "web/server.ts")
	call := plainCall("web", "GET", "/v1/search/items")
	if got := ClientRouteMatches(routeindex.New(vocab.Default()), []facts.Fact{server, call}); got != nil {
		t.Fatalf("single-repo snapshot produced matches: %+v", got)
	}
}
