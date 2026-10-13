package flows

import (
	"reflect"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func sym(name, file string, calls ...string) facts.Fact {
	f := facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "shop", File: file, Line: 1,
		Props: map[string]any{"symbol_kind": facts.SymbolFunc}}
	f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDeclares, Target: dirOf(file)})
	for _, c := range calls {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: c})
	}
	return f
}

func dirOf(file string) string {
	return file[:strings.LastIndex(file, "/")]
}

func route(method, name, handler string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: name, Repo: "shop", File: "api/routes.py", Line: 1,
		Props:     map[string]any{"method": method},
		Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: handler}}}
}

func analyze(ff ...facts.Fact) *Analysis {
	s := facts.NewStore()
	s.Add(ff...)
	return Analyze(s, Options{})
}

func names(a *Analysis) []string {
	out := make([]string, len(a.Flows))
	for i, f := range a.Flows {
		out[i] = f.Name
	}
	return out
}

// The shop: an order route that validates, charges and stores, a smaller status
// route, and a main that only starts the server.
func shop() []facts.Fact {
	return []facts.Fact{
		route("POST", "/orders", "api/orders.create"),
		route("GET", "/orders/{id}", "api/orders.status"),
		sym("api/orders.create", "api/orders.py", "core/order.place", "fmt.Println"),
		sym("api/orders.status", "api/orders.py", "db/orders.load"),
		sym("core/order.place", "core/order.py", "core/order.validate", "pay/charge.charge", "db/orders.save"),
		sym("core/order.validate", "core/order.py"),
		sym("pay/charge.charge", "pay/charge.py", "pay/charge.sign", "pay/charge.send"),
		sym("pay/charge.sign", "pay/charge.py"),
		sym("pay/charge.send", "pay/charge.py"),
		sym("db/orders.save", "db/orders.py"),
		sym("db/orders.load", "db/orders.py"),
		sym("cmd/server.main", "cmd/server.py"),
	}
}

func TestAnalyze_RanksEntriesByWhatTheyReach(t *testing.T) {
	a := analyze(shop()...)
	if got, want := names(a), []string{"POST /orders", "GET /orders/{id}"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("flows = %v, want %v", got, want)
	}
	top := a.Flows[0]
	if top.Metrics.Reach != 7 || top.Metrics.Modules != 4 || top.Metrics.MaxDepth != 3 {
		t.Errorf("metrics = %+v, want reach 7 over 4 modules at depth 3", top.Metrics)
	}
	if top.Mechanism != MechanismRoute || top.Entry.Name != "api/orders.create" || len(top.Routes) != 1 {
		t.Errorf("entry = %+v via %s, routes %v", top.Entry, top.Mechanism, top.Routes)
	}
	if top.Rank != 1 || a.Flows[1].Rank != 2 {
		t.Errorf("ranks = %d, %d", top.Rank, a.Flows[1].Rank)
	}
}

// A call to something that is no symbol in the graph is where the walk stops,
// and the flow has to say how often that happened.
func TestAnalyze_CountsTheCallsItCouldNotFollow(t *testing.T) {
	a := analyze(shop()...)
	if got := a.Flows[0].Metrics.UnresolvedCalls; got != 1 {
		t.Errorf("unresolved calls = %d, want 1 (fmt.Println)", got)
	}
}

// main is an entry, but one that calls nothing the graph follows has no path
// to describe. It is counted as a root and as a leaf, never listed as a flow.
func TestAnalyze_AnEntryReachingNothingIsALeaf(t *testing.T) {
	a := analyze(shop()...)
	if a.Roots[MechanismMain] != 1 || a.Roots[MechanismRoute] != 2 {
		t.Errorf("roots = %v", a.Roots)
	}
	if a.Leaves != 1 {
		t.Errorf("leaves = %d, want 1", a.Leaves)
	}
}

func TestAnalyze_TestCodeIsNeverAnEntry(t *testing.T) {
	a := analyze(
		sym("tests/test_orders.main", "tests/test_orders.py", "core/order.place"),
		sym("core/order.place", "core/order.py"),
	)
	if len(a.Flows) != 0 || len(a.Roots) != 0 {
		t.Errorf("flows = %v, roots = %v", names(a), a.Roots)
	}
}

func TestAnalyze_ClientRoutesAreNotEntries(t *testing.T) {
	client := route("GET", "/v1/rates", "pay/rates.fetch")
	client.Props["role"] = "client"
	a := analyze(client, sym("pay/rates.fetch", "pay/rates.py", "pay/rates.parse"), sym("pay/rates.parse", "pay/rates.py"))
	if len(a.Flows) != 0 {
		t.Errorf("flows = %v", names(a))
	}
}

// One handler behind two routes is one flow that lists both.
func TestAnalyze_OneHandlerIsOneFlowHoweverManyRoutes(t *testing.T) {
	a := analyze(
		route("PUT", "/orders/{id}", "api/orders.update"),
		route("PATCH", "/orders/{id}", "api/orders.update"),
		sym("api/orders.update", "api/orders.py", "db/orders.save"),
		sym("db/orders.save", "db/orders.py"),
	)
	if len(a.Flows) != 1 || a.Flows[0].Name != "PATCH /orders/{id}" || len(a.Flows[0].Routes) != 2 {
		t.Fatalf("flows = %+v", a.Flows)
	}
}

// Two entries with the same downstream are one flow seen from two doors.
func TestAnalyze_FoldsEntriesThatReachTheSameSymbols(t *testing.T) {
	a := analyze(
		route("POST", "/a", "api/h.a"),
		route("POST", "/b", "api/h.b"),
		sym("api/h.a", "api/h.py", "core/work.run"),
		sym("api/h.b", "api/h.py", "core/work.run"),
		sym("core/work.run", "core/work.py"),
	)
	if len(a.Flows) != 1 {
		t.Fatalf("flows = %v", names(a))
	}
	if got := a.Flows[0]; got.Name != "POST /a" || !reflect.DeepEqual(got.SameReachAs, []string{"POST /b"}) {
		t.Errorf("kept %q with same_reach_as %v", got.Name, got.SameReachAs)
	}
}

func TestAnalyze_NamesTheLargerFlowASmallerOneSitsIn(t *testing.T) {
	ff := []facts.Fact{
		route("POST", "/all", "api/h.all"),
		route("POST", "/part", "api/h.part"),
		sym("api/h.all", "api/h.py", "api/h.part", "core/x.extra"),
		sym("core/x.extra", "core/x.py"),
	}
	// part reaches ten steps; all reaches those through part, plus one more.
	calls := []string{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		ff = append(ff, sym("core/s."+n, "core/s.py"))
		calls = append(calls, "core/s."+n)
	}
	ff = append(ff, sym("api/h.part", "api/h.py", calls...))
	a := analyze(ff...)
	if got := names(a); !reflect.DeepEqual(got, []string{"POST /all", "POST /part"}) {
		t.Fatalf("flows = %v", got)
	}
	if !reflect.DeepEqual(a.Flows[1].NestedIn, []string{"POST /all"}) || a.Flows[0].NestedIn != nil {
		t.Errorf("nested_in = %v / %v", a.Flows[0].NestedIn, a.Flows[1].NestedIn)
	}
}

func TestAnalyze_FrameworkEntriesKeepTheirMechanism(t *testing.T) {
	job := sym("Jobs::Export#perform", "app/jobs/export.rb", "Exporter#run")
	job.Props["root"] = "job"
	job.Props["symbol_kind"] = facts.SymbolMethod
	registered := sym("tasks/sync.nightly", "tasks/sync.py", "tasks/sync.pull")
	registered.Props["framework_registered"] = true
	a := analyze(job, sym("Exporter#run", "app/exporter.rb"), registered, sym("tasks/sync.pull", "tasks/sync.py"))
	got := map[string]string{}
	for _, f := range a.Flows {
		got[f.Name] = f.Mechanism
	}
	want := map[string]string{"Jobs::Export#perform": "job", "tasks/sync.nightly": MechanismRegistered}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mechanisms = %v, want %v", got, want)
	}
}

func TestAnalyze_WalkStopsAtTheReachBound(t *testing.T) {
	ff := []facts.Fact{}
	calls := []string{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		ff = append(ff, sym("core/s."+n, "core/s.py"))
		calls = append(calls, "core/s."+n)
	}
	ff = append(ff, sym("cmd/tool.main", "cmd/tool.py", calls...))
	s := facts.NewStore()
	s.Add(ff...)
	a := Analyze(s, Options{MaxReach: 4})
	if len(a.Flows) != 1 || a.Flows[0].Metrics.Reach != 4 || !a.Flows[0].Metrics.Truncated {
		t.Fatalf("flows = %+v", a.Flows)
	}
}

func stageNames(f Flow) []string {
	out := make([]string, len(f.Stages))
	for i, s := range f.Stages {
		out[i] = strings.Repeat(" ", s.Depth-1) + ShortName(s.Name)
	}
	return out
}

func TestDescribe_StagesAreTheStepsThatAddTheMost(t *testing.T) {
	a := analyze(shop()...)
	a.Describe(1)
	want := []string{"order.place", " charge.charge", "  charge.send", "  charge.sign", " order.validate", " orders.save"}
	if got := stageNames(a.Flows[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("stages = %q, want %q", got, want)
	}
	for i, s := range a.Flows[0].Stages {
		if s.Order != i+1 {
			t.Errorf("stage %q has order %d at position %d", s.Name, s.Order, i+1)
		}
	}
	if a.Flows[0].Stages[1].Parent != a.Flows[0].Stages[0].ID {
		t.Errorf("charge is not placed under place")
	}
	if a.Flows[1].Stages != nil {
		t.Errorf("a flow nobody asked about was described")
	}
}

// A helper that calls back into the entry reaches the whole flow through it.
// Measured that way it would outrank the step that does the work.
func TestDescribe_AHelperThatLoopsBackIsNotTheMainStep(t *testing.T) {
	ff := []facts.Fact{
		sym("cmd/tool.main", "cmd/tool.py", "core/retry.again", "core/work.run"),
		sym("core/retry.again", "core/retry.py", "cmd/tool.main"),
		sym("core/work.run", "core/work.py", "core/work.a", "core/work.b", "core/work.c"),
		sym("core/work.a", "core/work.py"),
		sym("core/work.b", "core/work.py"),
		sym("core/work.c", "core/work.py"),
	}
	a := analyze(ff...)
	a.Describe(1)
	if got := stageNames(a.Flows[0]); len(got) == 0 || got[0] != "work.run" {
		t.Fatalf("stages = %q, want work.run first", got)
	}
	for _, s := range a.Flows[0].Stages {
		if s.Name == "core/retry.again" && s.Reach != 1 {
			t.Errorf("retry.again adds only itself and was credited with %d", s.Reach)
		}
	}
}

func TestDescribe_SummaryReadsFromTheFlowAlone(t *testing.T) {
	ff := shop()
	ff[2].Props["doc"] = "Place an order."
	a := analyze(ff...)
	a.Describe(1)
	want := `HTTP POST /orders enters at orders.create ("Place an order."). ` +
		`It hands off to order.place, which calls charge.charge, order.validate and orders.save. ` +
		`Most of its 7 symbols sit in pay (3 symbols), core (2) and api (1).`
	if got := a.Flows[0].Summary; got != want {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}
}

func TestThrough_ListsTheFlowsThatReachASymbol(t *testing.T) {
	a := analyze(shop()...)
	if got := a.Through("db/orders.load"); !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("through db/orders.load = %v", got)
	}
	if got := a.Through("charge.sign"); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("through the short name charge.sign = %v", got)
	}
	if got := a.Through("nothing.here"); got != nil {
		t.Errorf("through an unknown symbol = %v", got)
	}
}

func TestFind_RefusesToPickBetweenSeveral(t *testing.T) {
	a := analyze(shop()...)
	if i, _ := a.Find("POST /orders"); i != 0 {
		t.Errorf("exact name found at %d", i)
	}
	if i, _ := a.Find("{id}"); i != 1 {
		t.Errorf("unique substring found at %d", i)
	}
	if i, candidates := a.Find("/orders"); i != -1 || len(candidates) != 2 {
		t.Errorf("ambiguous name gave %d, %v", i, candidates)
	}
}

// Two runs over the same facts, added in a different order, must agree byte for
// byte: the ranking is written to an artifact that is compared across runs.
func TestAnalyze_IsIndependentOfFactOrder(t *testing.T) {
	ff := shop()
	forward := analyze(ff...)
	forward.Describe(len(forward.Flows))
	reversed := make([]facts.Fact, len(ff))
	for i, f := range ff {
		reversed[len(ff)-1-i] = f
	}
	backward := analyze(reversed...)
	backward.Describe(len(backward.Flows))
	if len(forward.Flows) != len(backward.Flows) {
		t.Fatalf("%d flows forward, %d reversed", len(forward.Flows), len(backward.Flows))
	}
	for i := range forward.Flows {
		x, y := forward.Flows[i], backward.Flows[i]
		if x.Name != y.Name || x.Summary != y.Summary || !reflect.DeepEqual(x.Stages, y.Stages) || !reflect.DeepEqual(x.Metrics, y.Metrics) {
			t.Errorf("flow %d differs:\n%+v\n%+v", i, x, y)
		}
	}
}
