package http

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	"github.com/enola-labs/enola/internal/linkers/vocab"
)

func withAliases(aliases map[string]string) *vocab.Set {
	v := vocab.Default()
	v.ServiceAliases = aliases
	return v
}

func runSignalUnder(t *testing.T, v *vocab.Set, ff ...facts.Fact) []recordedEdge {
	t.Helper()
	sink := &fakeSink{}
	New(v).Contribute(fakeInput{facts: ff}, sink)
	return sink.edges
}

// configuredCall is a client route read through a declared in-house client, naming its
// service by a string the config may alias.
func configuredCall(path, method, service string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: path, Repo: "sdk",
		Props: map[string]any{"role": "client", "method": method,
			"source": facts.RouteSourceConfiguredHTTPClient, facts.PropClientSpec: "sdk-http",
			"target_hint": service}}
}

func servedBy(repo, path, method string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: path, Repo: repo,
		Props: map[string]any{"role": "server", "method": method}}
}

// Two repositories serve the literal route and the service name matches neither label.
// Without an alias nothing may be drawn; with one, only the aliased repository.
func TestServiceAlias_ChoosesAmongProviders(t *testing.T) {
	call := configuredCall("/v1/catalog/imports", "POST", "resource-api")
	gateway := servedBy("gateway", "/v1/catalog/imports", "POST")
	replica := servedBy("replica", "/v1/catalog/imports", "POST")

	if edges := runSignalUnder(t, vocab.Default(), call, gateway, replica); len(edges) != 0 {
		t.Fatalf("with no alias an ambiguous call must draw nothing: %+v", edges)
	}
	edges := runSignalUnder(t, withAliases(map[string]string{"resource-api": "gateway"}), call, gateway, replica)
	if len(edges) != 1 || edges[0].from != "sdk" || edges[0].to != "gateway" {
		t.Fatalf("the alias must choose gateway and only gateway: %+v", edges)
	}
}

// The alias is a constraint. When the aliased repository does not serve the path, the
// one repository that does is not used instead: that edge would contradict the config.
func TestServiceAlias_IsAConstraintNotAPreference(t *testing.T) {
	v := withAliases(map[string]string{"resource-api": "gateway"})
	call := configuredCall("/v1/catalog/imports", "POST", "resource-api")
	replica := servedBy("replica", "/v1/catalog/imports", "POST")

	if edges := runSignalUnder(t, v, call, replica); len(edges) != 0 {
		t.Fatalf("an alias naming a repo that does not serve the path drew an edge elsewhere: %+v", edges)
	}
	reasons := UnmatchedClientRouteKeys(routeindex.New(v), []facts.Fact{call, replica})
	if got := reasons[routeindex.RouteIdentity(call)]; got != ReasonAliasNotServing {
		t.Errorf("unmatched_reason = %q, want %q", got, ReasonAliasNotServing)
	}
}

// An alias maps the service name a declared client passes. A route from any other pass
// carrying the same target_hint string did not pass that name, and is not aliased.
func TestServiceAlias_OnlyForConfiguredClients(t *testing.T) {
	v := withAliases(map[string]string{"resource-api": "gateway"})
	call := configuredCall("/v1/catalog/imports", "POST", "resource-api")
	delete(call.Props, facts.PropClientSpec)
	call.Props["source"] = facts.RouteSourceTSHTTPClient

	edges := runSignalUnder(t, v, call,
		servedBy("gateway", "/v1/catalog/imports", "POST"),
		servedBy("replica", "/v1/catalog/imports", "POST"))
	if len(edges) != 0 {
		t.Fatalf("an alias was applied to a route no declared client read: %+v", edges)
	}
}

// A single-segment path demands an outright named provider. An alias names one.
func TestServiceAlias_NamesTheProviderOfASingleSegmentPath(t *testing.T) {
	call := configuredCall("/mcp", "POST", "agent-backend")
	edges := runSignalUnder(t, withAliases(map[string]string{"agent-backend": "backend"}), call,
		servedBy("backend", "/mcp", "POST"), servedBy("other", "/mcp", "POST"))
	if len(edges) != 1 || edges[0].to != "backend" {
		t.Fatalf("an aliased provider must satisfy the single-segment carve-out: %+v", edges)
	}
}

// An exact label match beats a substring match, every time. Before, the first match in
// map order won, so the same snapshot could link to gateway on one run and
// gateway-mirror on the next.
func TestPickProvider_ExactLabelBeatsSubstring(t *testing.T) {
	call := facts.Fact{Kind: facts.KindRoute, Name: "/v1/orders/open", Repo: "web",
		Props: map[string]any{"role": "client", "method": "GET",
			"source": facts.RouteSourceTSHTTPClient, "target_hint": "gateway"}}
	matches := []routeindex.RouteRef{
		{Repo: "gateway-mirror", Method: "GET", Path: "/v1/orders/open"},
		{Repo: "gateway", Method: "GET", Path: "/v1/orders/open"},
	}
	m := routeindex.New(vocab.Default())
	for i := 0; i < 50; i++ {
		if got, _ := pickProvider(m, call, matches); got != "gateway" {
			t.Fatalf("run %d picked %q, want the exact label gateway", i, got)
		}
	}
}

// Several substring matches and no exact one is ambiguity, not a coin toss.
func TestPickProvider_SeveralSubstringMatchesPickNothing(t *testing.T) {
	call := facts.Fact{Kind: facts.KindRoute, Name: "/v1/orders/open", Repo: "web",
		Props: map[string]any{"role": "client", "method": "GET",
			"source": facts.RouteSourceTSHTTPClient, "target_hint": "gate"}}
	matches := []routeindex.RouteRef{
		{Repo: "gateway-mirror", Method: "GET", Path: "/v1/orders/open"},
		{Repo: "gateway", Method: "GET", Path: "/v1/orders/open"},
	}
	if got, _ := pickProvider(routeindex.New(vocab.Default()), call, matches); got != "" {
		t.Fatalf("picked %q from two substring matches, want no provider", got)
	}
	matches = matches[1:]
	matches = append(matches, routeindex.RouteRef{Repo: "billing", Method: "GET", Path: "/v1/orders/open"})
	if got, _ := pickProvider(routeindex.New(vocab.Default()), call, matches); got != "gateway" {
		t.Fatalf("a single substring match must still pick: got %q", got)
	}
}

// A call two repositories serve at its own verb is ambiguous. It is not a verb
// mismatch, and a call whose verb no server serves still is one.
func TestUnmatched_AmbiguousProviderIsNotAVerbMismatch(t *testing.T) {
	call := configuredCall("/v1/catalog/imports", "POST", "resource-api")
	wrongVerb := configuredCall("/v1/catalog/imports", "DELETE", "resource-api")
	all := []facts.Fact{call, wrongVerb,
		servedBy("gateway", "/v1/catalog/imports", "POST"),
		servedBy("replica", "/v1/catalog/imports", "POST")}

	reasons := UnmatchedClientRouteKeys(routeindex.New(vocab.Default()), all)
	if got := reasons[routeindex.RouteIdentity(call)]; got != ReasonAmbiguousProvider {
		t.Errorf("ambiguous call: unmatched_reason = %q, want %q", got, ReasonAmbiguousProvider)
	}
	if got := reasons[routeindex.RouteIdentity(wrongVerb)]; got != ReasonMethodMismatch {
		t.Errorf("wrong verb: unmatched_reason = %q, want %q", got, ReasonMethodMismatch)
	}
}
