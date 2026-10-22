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

// phpStore is a Laravel-shaped graph: handlers and methods are `Class::method`, and
// one action is inherited from a base controller.
func phpStore() *facts.Store {
	const (
		orders = `App\Http\Controllers\OrderController`
		base   = `App\Http\Controllers\Controller`
	)
	class := func(name string) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, Props: map[string]any{"symbol_kind": facts.SymbolClass}}
	}
	method := func(name string, calls ...string) facts.Fact {
		f := facts.Fact{Kind: facts.KindSymbol, Name: name, Props: map[string]any{"symbol_kind": facts.SymbolMethod}}
		for _, c := range calls {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: c})
		}
		return f
	}
	route := func(path, handler, handledBy string) facts.Fact {
		f := facts.Fact{Kind: facts.KindRoute, Name: path, Props: map[string]any{"method": "GET", "handler": handler}}
		if handledBy != "" {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: handledBy})
		}
		return f
	}
	st := facts.NewStore()
	st.Add(
		route("/v2/orders/{id}", orders+"::show", orders+"::show"),
		route("/v2/orders/{id}/audit", orders+"::audit", base+"::audit"),
		route("/v2/short", "OrderController::show", ""),
		route("/v2/unbound", orders+"::missing", ""),
		class(orders),
		method(orders+"::show", `App\Models\Order`),
		method(orders+"::export", `App\Models\Report`),
		class(`App\Models\Report`),
		facts.Fact{Kind: facts.KindStorage, Name: `App\Models\Report`, Props: map[string]any{
			"storage_kind": "model", "table": "reports"}},
		class(base),
		method(base+"::audit", `App\Models\AuditEntry`),
		class(`App\Models\Order`),
		class(`App\Models\AuditEntry`),
		facts.Fact{Kind: facts.KindStorage, Name: `App\Models\Order`, Props: map[string]any{
			"storage_kind": "model", "table": "orders"}},
		facts.Fact{Kind: facts.KindStorage, Name: `App\Models\AuditEntry`, Props: map[string]any{
			"storage_kind": "model", "table": "audit_entries"}},
	)
	st.BuildGraph()
	return st
}

func TestAnalyzeFollowsAScopedHandler(t *testing.T) {
	got := Analyze(phpStore(), "GET /v2/orders/{id}", 1, nil)
	if len(got.Controllers) != 1 || got.Controllers[0] != `App\Http\Controllers\OrderController` {
		t.Fatalf("controllers = %v, want the class `Class::method` names", got.Controllers)
	}
	if len(got.Models) != 1 || got.Models[0] != `App\Models\Order` {
		t.Errorf("models = %v, want the one the controller's method calls", got.Models)
	}
	if got.StoppedAt != "" {
		t.Errorf("the chain completed, so nothing stopped it: %q", got.StoppedAt)
	}
}

// TestAnalyzeFollowsAnInheritedHandler: the route names OrderController, the action
// is declared on its parent, and what it reaches is the parent method's.
func TestAnalyzeFollowsAnInheritedHandler(t *testing.T) {
	got := Analyze(phpStore(), "/audit", 25, nil)
	if len(got.Controllers) != 1 || got.Controllers[0] != `App\Http\Controllers\OrderController` {
		t.Fatalf("controllers = %v, want the class the route names", got.Controllers)
	}
	found := false
	for _, m := range got.Models {
		found = found || m == `App\Models\AuditEntry`
	}
	if !found {
		t.Errorf("models = %v, want the one the inherited action reaches", got.Models)
	}
}

// TestAnalyzeDoesNotGuessAScopedController: a short class name is not matched by
// spelling the way a Rails controller path is.
func TestAnalyzeDoesNotGuessAScopedController(t *testing.T) {
	got := Analyze(phpStore(), "/v2/short", 25, nil)
	if len(got.Controllers) != 0 || got.StoppedAt != "controller" {
		t.Errorf("controllers = %v, stopped at %q; want an unresolved handler to stop at the controller",
			got.Controllers, got.StoppedAt)
	}
}

// TestAnalyzeReachesAModelThroughItsMethod: `Order::where` is the framework's query
// builder, so no symbol answers to it, and calling it still reaches Order. A class
// nested under a model's name does not.
func TestAnalyzeReachesAModelThroughItsMethod(t *testing.T) {
	const controller = `App\Http\Controllers\InvoiceController`
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindRoute, Name: "/invoices", Props: map[string]any{
			"method": "GET", "handler": controller + "::index"}},
		facts.Fact{Kind: facts.KindSymbol, Name: controller, Props: map[string]any{"symbol_kind": facts.SymbolClass}},
		facts.Fact{Kind: facts.KindSymbol, Name: controller + "::index", Props: map[string]any{
			"symbol_kind": facts.SymbolMethod},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: `App\Models\Order::where`}}},
		facts.Fact{Kind: facts.KindSymbol, Name: `App\Models\Order`, Props: map[string]any{"symbol_kind": facts.SymbolClass}},
		facts.Fact{Kind: facts.KindStorage, Name: `App\Models\Order`, Props: map[string]any{
			"storage_kind": "model", "table": "orders"}},
	)
	st.BuildGraph()
	got := Analyze(st, "/invoices", 25, nil)
	if len(got.Models) != 1 || got.Models[0] != `App\Models\Order` {
		t.Errorf("models = %v, want the model whose method the controller calls", got.Models)
	}
	if len(got.Tables) != 1 || got.Tables[0] != "orders" {
		t.Errorf("tables = %v", got.Tables)
	}

	nested := facts.NewStore()
	nested.Add(
		facts.Fact{Kind: facts.KindRoute, Name: "/settings", Props: map[string]any{
			"method": "GET", "handler": "settings#show"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "SettingsController", Props: map[string]any{
			"symbol_kind": facts.SymbolClass},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Company::Setting"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Company::Setting", Props: map[string]any{"symbol_kind": facts.SymbolClass}},
		facts.Fact{Kind: facts.KindStorage, Name: "Company", Props: map[string]any{
			"storage_kind": "model", "table": "companies"}},
	)
	nested.BuildGraph()
	if got := Analyze(nested, "/settings", 25, nil); len(got.Models) != 0 {
		t.Errorf("models = %v, want none: a class nested under a model's name is not the model", got.Models)
	}
}

// TestAnalyzeWalksFromTheHandlerNotItsSiblings: the route is bound to show, so what
// export reaches is another endpoint's. A route that names the class without a bound
// method is walked from all of them, because nothing says which one serves it.
func TestAnalyzeWalksFromTheHandlerNotItsSiblings(t *testing.T) {
	got := Analyze(phpStore(), "GET /v2/orders/{id}", 1, nil)
	if len(got.Models) != 1 || got.Models[0] != `App\Models\Order` {
		t.Errorf("models = %v, want only what the bound method reaches", got.Models)
	}
	unbound := Analyze(phpStore(), "/v2/unbound", 25, nil)
	if len(unbound.Controllers) != 1 || len(unbound.Models) != 2 {
		t.Errorf("controllers = %v, models = %v; want the whole class walked when no method is bound",
			unbound.Controllers, unbound.Models)
	}
}

// TestAnalyzeFollowsADottedHandler: TypeScript, Java and C# name a method
// `<owner>.<method>`, and a NestJS route's handler prop is the bare method name, so
// the class is only in the handled_by target. Reading only `#` and `::` reported
// "no class in this repository answers" for a controller the graph declares.
func TestAnalyzeFollowsADottedHandler(t *testing.T) {
	const controller = "gateway/src/catalog.CatalogController"
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindRoute, Name: "/v1/catalog/imports", Props: map[string]any{
			"method": "POST", "handler": "startImport", "framework": "nestjs"},
			Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: controller + ".startImport"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: controller, Props: map[string]any{"symbol_kind": facts.SymbolClass}},
		facts.Fact{Kind: facts.KindSymbol, Name: controller + ".startImport", Props: map[string]any{
			"symbol_kind": facts.SymbolMethod},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "gateway/src/catalog.CatalogImport"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: controller + ".listImports", Props: map[string]any{
			"symbol_kind": facts.SymbolMethod},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "gateway/src/catalog.Report"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "gateway/src/catalog.CatalogImport", Props: map[string]any{"symbol_kind": facts.SymbolClass}},
		facts.Fact{Kind: facts.KindStorage, Name: "gateway/src/catalog.CatalogImport", Props: map[string]any{
			"storage_kind": "model", "table": "catalog_imports"}},
		facts.Fact{Kind: facts.KindStorage, Name: "gateway/src/catalog.Report", Props: map[string]any{
			"storage_kind": "model", "table": "reports"}},
	)
	st.BuildGraph()

	got := Analyze(st, "/v1/catalog/imports", 25, nil)
	if len(got.Controllers) != 1 || got.Controllers[0] != controller {
		t.Fatalf("controllers = %v, stopped at %q; want the class the handled_by method belongs to",
			got.Controllers, got.StoppedAt)
	}
	// Walked from the bound method, not the whole class: listImports' model is
	// another endpoint's.
	if len(got.Models) != 1 || got.Models[0] != "gateway/src/catalog.CatalogImport" {
		t.Errorf("models = %v, want only what the bound method reaches", got.Models)
	}
	if got.StoppedAt != "" {
		t.Errorf("the chain completed, so nothing stopped it: %q", got.StoppedAt)
	}
}

// TestAnalyzeTakesAHandlerThatIsTheClass: a Grape route is served by the API class
// itself, so handled_by names the class and no method. Nothing says which part of the
// class serves it, so the whole class is walked.
func TestAnalyzeTakesAHandlerThatIsTheClass(t *testing.T) {
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindRoute, Name: "/api/v1/hello", Props: map[string]any{
			"method": "GET", "handler": "BenchAPI", "framework": "grape"},
			Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "BenchAPI"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "BenchAPI", Props: map[string]any{"symbol_kind": facts.SymbolClass},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Greeting"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Greeting", Props: map[string]any{"symbol_kind": facts.SymbolClass}},
		facts.Fact{Kind: facts.KindStorage, Name: "Greeting", Props: map[string]any{
			"storage_kind": "model", "table": "greetings"}},
	)
	st.BuildGraph()

	got := Analyze(st, "/api/v1/hello", 25, nil)
	if len(got.Controllers) != 1 || got.Controllers[0] != "BenchAPI" {
		t.Fatalf("controllers = %v, stopped at %q; want the class handled_by names", got.Controllers, got.StoppedAt)
	}
	if len(got.Models) != 1 || got.Models[0] != "Greeting" {
		t.Errorf("models = %v, want what the class reaches", got.Models)
	}

	// A handled_by target that is neither a class nor a method of one is still not a
	// controller: a Go handler is a package function, and no class serves it.
	fn := facts.NewStore()
	fn.Add(
		facts.Fact{Kind: facts.KindRoute, Name: "/api/v2/orders/{id}", Props: map[string]any{
			"method": "GET", "handler": "getOrder"},
			Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "api.getOrder"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "api.getOrder", Props: map[string]any{"symbol_kind": facts.SymbolFunc}},
	)
	fn.BuildGraph()
	if got := Analyze(fn, "/api/v2/orders", 25, nil); len(got.Controllers) != 0 || got.StoppedAt != "controller" {
		t.Errorf("controllers = %v, stopped at %q; a function handler has no class", got.Controllers, got.StoppedAt)
	}
}

// A caller whose every call to the endpoint sits in client code no page reaches is
// listed and marked as never running; one call in the file that runs is enough for it
// to count as calling, and a call with no verdict counts as running.
func TestAnalyzeMarksCallersThatNeverRun(t *testing.T) {
	st := facts.NewStore()
	client := func(file string, reach any) facts.Fact {
		p := map[string]any{"method": "GET", "role": "client"}
		if reach != nil {
			p[facts.PropClientReachable] = reach
		}
		return facts.Fact{Kind: facts.KindRoute, Name: "/api/items/{id}", File: file, Props: p}
	}
	st.Add(
		facts.Fact{Kind: facts.KindRoute, Name: "/api/items/{id}", Props: map[string]any{"method": "GET", "handler": "items#show"}},
		client("web/src/lib/api/items.ts", false),
		client("web/src/lib/api/legacy.ts", false),
		client("web/src/lib/api/legacy.ts", true),
		client("web/src/lib/api/plain.ts", nil),
	)
	st.BuildGraph()
	all := func([]facts.Fact) []facts.Fact {
		var out []facts.Fact
		for _, f := range st.ByKind(facts.KindRoute) {
			if f.Props["role"] == "client" {
				out = append(out, f)
			}
		}
		return out
	}
	got := Analyze(st, "GET /api/items/{id}", 25, all)
	never := map[string]bool{}
	for _, c := range got.Callers {
		never[c.File] = c.NeverRuns
	}
	want := map[string]bool{"web/src/lib/api/items.ts": true, "web/src/lib/api/legacy.ts": false, "web/src/lib/api/plain.ts": false}
	for f, w := range want {
		if v, ok := never[f]; !ok || v != w {
			t.Errorf("%s: never runs = %v (listed %v), want %v", f, v, ok, w)
		}
	}
}
