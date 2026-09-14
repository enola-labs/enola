package http

import (
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	"github.com/enola-labs/enola/internal/linkers/vocab"
)

func callerRepos(callers []facts.Fact) []string {
	var out []string
	for _, f := range callers {
		out = append(out, f.Repo+":"+f.Name)
	}
	sort.Strings(out)
	return out
}

func plainCall(repo, method, path string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: path, Repo: repo,
		Props: map[string]any{"role": "client", "method": method, "source": facts.RouteSourceTSHTTPClient}}
}

// A client writes the path its base URL does not already carry. The linker joins it to
// the full served path by suffix, and so must the callers.
func TestCallersOf_JoinsAcrossABaseURL(t *testing.T) {
	server := servedBy("api", "/v1/candidates/:id", "GET")
	call := plainCall("cli", "GET", "/candidates/{}")
	got := CallersOf(routeindex.New(vocab.Default()), []facts.Fact{server, call}, []facts.Fact{server})
	if len(got) != 1 || got[0].Repo != "cli" {
		t.Fatalf("a base-relative call was not named a caller: %v", callerRepos(got))
	}
}

// The endpoint's callers follow the linking vocabulary: a call reaching a parameterized
// route is a caller exactly when the linker would link it.
func TestCallersOf_FollowsTheParameterMatchingSetting(t *testing.T) {
	server := servedBy("gateway", "/v1/resources/:type/items", "GET")
	call := configuredCall("/v1/resources/catalog/items", "GET", "resource-api")
	all := []facts.Fact{server, call}

	if got := CallersOf(routeindex.New(paramVocab(false)), all, []facts.Fact{server}); len(got) != 0 {
		t.Errorf("with parameter matching off a caller was named: %v", callerRepos(got))
	}
	if got := CallersOf(routeindex.New(paramVocab(true)), all, []facts.Fact{server}); len(got) != 1 {
		t.Errorf("with parameter matching on the call was not named a caller: %v", callerRepos(got))
	}
}

// Two repositories serve the path. With no alias both routes count the call as a caller;
// with an alias only the aliased repository's route does.
func TestCallersOf_AliasNarrowsWhoIsCalled(t *testing.T) {
	gateway := servedBy("gateway", "/v1/catalog/imports", "POST")
	replica := servedBy("replica", "/v1/catalog/imports", "POST")
	call := configuredCall("/v1/catalog/imports", "POST", "resource-api")
	all := []facts.Fact{gateway, replica, call}

	plain := routeindex.New(vocab.Default())
	if len(CallersOf(plain, all, []facts.Fact{gateway})) != 1 || len(CallersOf(plain, all, []facts.Fact{replica})) != 1 {
		t.Error("with no alias, nothing says which repository is called, so both routes must name the caller")
	}
	aliased := routeindex.New(withAliases(map[string]string{"resource-api": "gateway"}))
	if got := CallersOf(aliased, all, []facts.Fact{gateway}); len(got) != 1 {
		t.Errorf("the aliased repository's route lost its caller: %v", callerRepos(got))
	}
	if got := CallersOf(aliased, all, []facts.Fact{replica}); len(got) != 0 {
		t.Errorf("a route the alias does not send the call to named it a caller: %v", callerRepos(got))
	}
}

// A frontend and the backend it calls in one repository carry no repository label. The
// cross-repo verdicts skip that case; an endpoint's callers must not.
func TestCallersOf_AnswersInASingleRepository(t *testing.T) {
	server := servedBy("", "/app/api/available_companies", "GET")
	call := plainCall("", "GET", "/app/api/available_companies")
	if got := CallersOf(routeindex.New(vocab.Default()), []facts.Fact{server, call}, []facts.Fact{server}); len(got) != 1 {
		t.Fatalf("a caller in the same unlabelled repository was not named: %v", callerRepos(got))
	}
}

func TestCallersOf_VerbAndGenericPathsAreNotCallers(t *testing.T) {
	get := servedBy("api", "/v1/orders/open", "GET")
	health := servedBy("api", "/health", "GET")
	all := []facts.Fact{get, health, plainCall("web", "DELETE", "/v1/orders/open"), plainCall("web", "GET", "/health")}
	m := routeindex.New(vocab.Default())
	if got := CallersOf(m, all, []facts.Fact{get}); len(got) != 0 {
		t.Errorf("a call with another verb was named a caller: %v", callerRepos(got))
	}
	if got := CallersOf(m, all, []facts.Fact{health}); len(got) != 0 {
		t.Errorf("a generic path the linker refuses to link produced a caller: %v", callerRepos(got))
	}
}

// "This route is used" and "this route has a caller" are one decision: over a mixed
// multi-repository set, every route the verdicts evaluate is used exactly when CallersOf
// names at least one caller for it.
func TestCallersOf_AgreesWithTheServerRouteVerdicts(t *testing.T) {
	servers := []facts.Fact{
		servedBy("gateway", "/v1/catalog/imports", "POST"),
		servedBy("replica", "/v1/catalog/imports", "POST"),
		servedBy("gateway", "/v1/resources/:type/items", "GET"),
		servedBy("api", "/v1/candidates/:id", "GET"),
		servedBy("api", "/v1/unused/thing", "GET"),
	}
	all := append([]facts.Fact{
		configuredCall("/v1/catalog/imports", "POST", "resource-api"),
		configuredCall("/v1/resources/catalog/items", "GET", "resource-api"),
		plainCall("cli", "GET", "/candidates/{}"),
		plainCall("cli", "DELETE", "/v1/unused/thing"),
	}, servers...)

	for _, v := range []*vocab.Set{vocab.Default(), paramVocab(true), withAliases(map[string]string{"resource-api": "gateway"})} {
		m := routeindex.New(v)
		evaluated, unused := ServerRouteVerdicts(m, all)
		for _, s := range servers {
			id := routeindex.RouteIdentity(s)
			if !evaluated[id] {
				continue
			}
			hasCaller := len(CallersOf(m, all, []facts.Fact{s})) > 0
			if hasCaller == unused[id] {
				t.Errorf("%s %s: unused=%v but CallersOf named %d caller(s)", s.Repo, s.Name, unused[id], len(CallersOf(m, all, []facts.Fact{s})))
			}
		}
	}
}
