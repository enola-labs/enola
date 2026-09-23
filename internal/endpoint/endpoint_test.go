package endpoint

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func endpointStore() *facts.Store {
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindRoute, Name: "/v1/candidates/:id", Props: map[string]any{
			"method": "GET", "handler": "api/v1/candidates#show"}},
		facts.Fact{Kind: facts.KindRoute, Name: "/v1/candidates/:id", Props: map[string]any{
			"method": "DELETE", "handler": "api/v1/candidates#destroy"}},
		// A mock and a client call site must never be followed: they describe
		// what something else does, not what this app serves.
		facts.Fact{Kind: facts.KindRoute, Name: "/v1/candidates/:id", Props: map[string]any{
			"method": "GET", "role": "client", "test_double": true}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Api::V1::CandidatesController", Props: map[string]any{
			"symbol_kind": facts.SymbolClass}, Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Candidate"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Candidate", Props: map[string]any{"symbol_kind": facts.SymbolClass}},
		facts.Fact{Kind: facts.KindStorage, Name: "Candidate", Props: map[string]any{
			"storage_kind": "model", "table": "candidates"}},
		facts.Fact{Kind: facts.KindStorage, Name: "JobApplication", Props: map[string]any{
			"storage_kind": "model", "table": "job_applications"}},
		facts.Fact{Kind: facts.KindAssociation, Name: "Candidate#job_applications", Props: map[string]any{
			"model": "Candidate", "target": "JobApplication", "macro": "has_many"}},
	)
	st.BuildGraph()
	return st
}

func TestAnalyzeWalksTheWholeChain(t *testing.T) {
	got := Analyze(endpointStore(), "GET /v1/candidates", 25, nil)
	if len(got.Routes) != 1 {
		t.Fatalf("one server route matches GET, got %d: %+v", len(got.Routes), got.Routes)
	}
	if len(got.Controllers) != 1 || got.Controllers[0] != "Api::V1::CandidatesController" {
		t.Errorf("controllers = %v", got.Controllers)
	}
	if len(got.Models) != 1 || got.Models[0] != "Candidate" {
		t.Errorf("models = %v", got.Models)
	}
	if len(got.Associated) != 1 || got.Associated[0] != "JobApplication" {
		t.Errorf("associated = %v — one association hop out from the model the controller reaches", got.Associated)
	}
	if len(got.Tables) != 2 {
		t.Errorf("tables = %v, want both the model's and the associated model's", got.Tables)
	}
	if got.StoppedAt != "" {
		t.Errorf("the chain completed, so nothing stopped it: %q", got.StoppedAt)
	}
}

// TestAnalyzeNamesTheHopThatRanOut is the property that keeps an empty
// answer honest: "touches nothing" and "I stopped here" are different claims.
func TestAnalyzeNamesTheHopThatRanOut(t *testing.T) {
	st := facts.NewStore()
	st.Add(facts.Fact{Kind: facts.KindRoute, Name: "/orphan", Props: map[string]any{
		"method": "GET", "handler": "nowhere#show"}})
	st.BuildGraph()

	got := Analyze(st, "/orphan", 25, nil)
	if len(got.Routes) != 1 {
		t.Fatalf("the route itself is still reported: %+v", got)
	}
	if got.StoppedAt != "controller" {
		t.Errorf("StoppedAt = %q, want controller", got.StoppedAt)
	}

	missing := Analyze(st, "/nothing-here", 25, nil)
	if missing.StoppedAt != "route" {
		t.Errorf("StoppedAt = %q, want route", missing.StoppedAt)
	}
}

// TestAnalyzeIgnoresMocksAndClients pins that the traversal answers
// about what this application serves.
func TestAnalyzeIgnoresMocksAndClients(t *testing.T) {
	got := Analyze(endpointStore(), "/v1/candidates", 25, nil)
	for _, route := range got.Routes {
		if route.Handler == "" {
			t.Errorf("a mock or client route was followed: %+v", route)
		}
	}
	if len(got.Routes) != 2 {
		t.Errorf("both server routes and neither double: %d", len(got.Routes))
	}
}

// TestAnalyzeFindsTheFrontendScreen covers the cross-stack half: the
// models say what an endpoint writes, the callers say who notices. The screen
// is matched on the Ember route NAME rather than its URL, because a route that
// overrides its path — which is most of the interesting ones — has a name the
// file layout mirrors and a URL it does not.
func TestAnalyzeFindsTheFrontendScreen(t *testing.T) {
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindRoute, Name: "/app/api/available_companies", Props: map[string]any{
			"method": "GET", "handler": "app/api/available_companies#index"}},
		facts.Fact{Kind: facts.KindRoute, Name: "/admin/company-linking", Props: map[string]any{
			"method": "GET", "framework": "ember", "ember_route_name": "admin-company-linking"}},
		facts.Fact{Kind: facts.KindRoute, Name: "/app/api/available_companies",
			File:  "ember_app/app/routes/admin-company-linking.ts",
			Props: map[string]any{"method": "GET", "role": "client"}},
		facts.Fact{Kind: facts.KindRoute, Name: "/app/api/available_companies",
			File:  "ember_app/app/components/picker.ts",
			Props: map[string]any{"method": "GET", "role": "client"}},
		// A mock calling the same endpoint is not a caller that notices.
		facts.Fact{Kind: facts.KindRoute, Name: "/app/api/available_companies",
			File:  "ember_app/app/mirage/routes.js",
			Props: map[string]any{"method": "GET", "role": "client", "test_double": true}},
	)
	st.BuildGraph()

	// The finder hands back every client call site, the mock included: which calls reach
	// the endpoint is the linker's question, tested with it; what this package owns is
	// dropping the mock, one caller per file, and naming the screen.
	allClients := func([]facts.Fact) []facts.Fact {
		var out []facts.Fact
		for _, f := range st.ByKind(facts.KindRoute) {
			if f.Props["role"] == "client" {
				out = append(out, f)
			}
		}
		return out
	}
	got := Analyze(st, "GET /app/api/available_companies", 25, allClients)
	if len(got.Callers) != 2 {
		t.Fatalf("two real callers and no mock, got %d: %+v", len(got.Callers), got.Callers)
	}
	byFile := map[string]string{}
	for _, caller := range got.Callers {
		byFile[caller.File] = caller.Screen
	}
	if screen := byFile["ember_app/app/routes/admin-company-linking.ts"]; screen != "/admin/company-linking" {
		t.Errorf("route module resolves to its screen, got %q", screen)
	}
	// A component may be used by many screens, so claiming one would be a guess.
	if screen := byFile["ember_app/app/components/picker.ts"]; screen != "" {
		t.Errorf("a component names no single screen, got %q", screen)
	}
}

// TestAnalyzeAsksForCallersOfTheFollowedRoutes: callers are asked for exactly the
// routes the answer follows, after the verb filter, the sort and the cap, so a route the
// answer does not report cannot contribute a caller.
func TestAnalyzeAsksForCallersOfTheFollowedRoutes(t *testing.T) {
	var asked []facts.Fact
	record := func(servers []facts.Fact) []facts.Fact {
		asked = servers
		return nil
	}
	got := Analyze(endpointStore(), "/v1/candidates", 1, record)
	if len(got.Routes) != 1 || len(asked) != 1 {
		t.Fatalf("want one followed route and one asked about, got routes %+v, asked %+v", got.Routes, asked)
	}
	if asked[0].Name != got.Routes[0].Path || asked[0].Props["method"] != got.Routes[0].Method {
		t.Errorf("asked about %s %v, but followed %s %s", asked[0].Name, asked[0].Props["method"], got.Routes[0].Method, got.Routes[0].Path)
	}
	for _, f := range asked {
		if f.Props["role"] == "client" || f.Props["test_double"] == true {
			t.Errorf("a client or mock was passed as a served route: %+v", f)
		}
	}

	if none := Analyze(endpointStore(), "GET /v1/candidates", 25, nil); len(none.Callers) != 0 {
		t.Errorf("no finder must report no callers, got %+v", none.Callers)
	}
}
