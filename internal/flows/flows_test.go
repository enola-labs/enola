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
	if got, known := a.Through("db/orders.load"); !known || !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("through db/orders.load = %v, known %v", got, known)
	}
	if got, known := a.Through("charge.sign"); !known || !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("through the short name charge.sign = %v, known %v", got, known)
	}
	// main is a symbol no flow reaches; nothing.here is no symbol.
	if got, known := a.Through("cmd/server.main"); !known || got != nil {
		t.Errorf("through a symbol outside every flow = %v, known %v", got, known)
	}
	if got, known := a.Through("nothing.here"); known || got != nil {
		t.Errorf("through an unknown symbol = %v, known %v", got, known)
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

// Only what an entry reaches is ever turned into a node. A symbol no flow
// arrives at costs nothing, which is what keeps a large graph affordable.
func TestAnalyze_LooksOnlyAtWhatTheEntriesReach(t *testing.T) {
	ff := shop()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		ff = append(ff, sym("lib/unused."+n, "lib/unused.py", "lib/unused.a"))
	}
	a := analyze(ff...)
	for _, n := range a.nodes {
		if strings.HasPrefix(n.fact.Name, "lib/unused.") {
			t.Errorf("%s is reached by no entry and was given a node", n.fact.Name)
		}
	}
	if _, known := a.Through("lib/unused.c"); !known {
		t.Errorf("a symbol outside every flow must still be known to --through")
	}
}

// A call resolves inside the caller's repository first, and into another only
// when one other repository declares the name.
func TestAnalyze_ResolvesACallAcrossRepositoriesOnlyWhenUnambiguous(t *testing.T) {
	in := func(repo string, f facts.Fact) facts.Fact { f.Repo = repo; return f }
	a := analyze(
		in("web", sym("cmd/web.main", "cmd/web.py", "lib/auth.check", "lib/log.write", "lib/own.helper")),
		in("web", sym("lib/own.helper", "lib/own.py")),
		in("auth", sym("lib/own.helper", "lib/own.py")),
		in("auth", sym("lib/auth.check", "lib/auth.py")),
		in("a", sym("lib/log.write", "lib/log.py")),
		in("b", sym("lib/log.write", "lib/log.py")),
	)
	if len(a.Flows) != 1 {
		t.Fatalf("flows = %v", names(a))
	}
	got := map[string]bool{}
	for _, id := range a.Flows[0].reach {
		got[a.nodes[id].fact.Repo+":"+a.nodes[id].fact.Name] = true
	}
	want := map[string]bool{"web:cmd/web.main": true, "web:lib/own.helper": true, "auth:lib/auth.check": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reach = %v, want %v", got, want)
	}
	if a.Flows[0].Metrics.UnresolvedCalls != 1 {
		t.Errorf("unresolved = %d, want 1 (lib/log.write is declared by two other repositories)", a.Flows[0].Metrics.UnresolvedCalls)
	}
}

// .NET spells its entry Main. The same name in Go is an exported function
// like any other.
func TestAnalyze_MainIsSpelledPerLanguage(t *testing.T) {
	in := func(language string, f facts.Fact) facts.Fact { f.Props["language"] = language; return f }
	method := func(f facts.Fact) facts.Fact { f.Props["symbol_kind"] = facts.SymbolMethod; return f }
	a := analyze(
		in("csharp", method(sym("Shop.Program.Main", "src/Program.cs", "Shop.Server.Run"))),
		in("csharp", method(sym("Shop.Server.Run", "src/Server.cs"))),
		in("go", sym("pkg/app.Main", "pkg/app.go", "pkg/app.run")),
		in("go", sym("pkg/app.run", "pkg/app.go")),
	)
	if got := names(a); !reflect.DeepEqual(got, []string{"Shop.Program.Main"}) {
		t.Errorf("flows = %v, want only the .NET Main", got)
	}
}

// An example that reaches more than the product does is still an example. The
// program's flows come first, whatever their size.
func TestAnalyze_SideProgramsRankBelowTheProgram(t *testing.T) {
	big := []string{}
	ff := []facts.Fact{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		ff = append(ff, sym("lib/x."+n, "lib/x.py"))
		big = append(big, "lib/x."+n)
	}
	ff = append(ff,
		sym("examples/demo.main", "examples/demo.py", big...),
		sym("tools/gen.main", "tools/gen.py", big[:4]...),
		sym("db/migrate/001.main", "db/migrate/001.py", big[:3]...),
		sym("cmd/app.main", "cmd/app.py", big[:2]...),
		// tools/ below the top of the repository is a feature, not tooling.
		sym("app/modules/tools/run.main", "app/modules/tools/run.py", big[:1]...),
	)
	a := analyze(ff...)
	got := []string{}
	for _, f := range a.Flows {
		mark := ""
		if f.Side {
			mark = " side"
		}
		got = append(got, f.Name+mark)
	}
	want := []string{
		"cmd/app.main", "app/modules/tools/run.main",
		"examples/demo.main side", "tools/gen.main side", "db/migrate/001.main side",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ranking = %q\nwant      %q", got, want)
	}
}

// Xcode keeps an app's preview apps under Previews/, and their assets under Preview
// Content/: each is a program a platform starts, and none is the app. A module
// that only has Preview in its name is a feature.
func TestSidePath_XcodePreviews(t *testing.T) {
	for file, want := range map[string]bool{
		"App/Previews/HomeFeaturePreview/HomeFeaturePreviewApp.swift":            true,
		"App/iOS/Preview Content/Sample.swift":                                   true,
		"Sources/CubePreview/CubePreview.swift":                                  false,
		"web/src/previews/LinkPreview.tsx":                                       false,
		"src/Files.App/ViewModels/UserControls/Previews/TextPreviewViewModel.cs": false,
		"App/iOS/App.swift":                                                      false,
	} {
		if got := sidePath(file); got != want {
			t.Errorf("sidePath(%q) = %v, want %v", file, got, want)
		}
	}
}

func TestAnalyze_AModulesRoleMakesItsEntriesSide(t *testing.T) {
	a := analyze(
		facts.Fact{Kind: facts.KindModule, Name: "internal/gen", Repo: "shop", Props: map[string]any{facts.PropModuleRole: facts.ModuleRoleTooling}},
		sym("internal/gen.main", "internal/gen/gen.py", "internal/gen.write"),
		sym("internal/gen.write", "internal/gen/gen.py"),
	)
	if len(a.Flows) != 1 || !a.Flows[0].Side {
		t.Errorf("flows = %+v", a.Flows)
	}
}

// Every C program in a directory declares a symbol with the same name. They
// are separate programs: one flow each, named apart, each reaching only what
// its own file calls.
func TestAnalyze_SameNamedEntriesAreOneFlowPerFile(t *testing.T) {
	a := analyze(
		sym("cmd/perf.main", "cmd/perf/record.c", "cmd/perf.record"),
		sym("cmd/perf.main", "cmd/perf/report.c", "cmd/perf.report", "cmd/perf.format"),
		sym("cmd/perf.record", "cmd/perf/record.c"),
		sym("cmd/perf.report", "cmd/perf/report.c"),
		sym("cmd/perf.format", "cmd/perf/report.c"),
	)
	got := map[string]int{}
	for _, f := range a.Flows {
		got[f.Name] = f.Metrics.Reach
	}
	want := map[string]int{"cmd/perf.main (record.c)": 2, "cmd/perf.main (report.c)": 3}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flows = %v, want %v", got, want)
	}
	if a.Flows[0].ID == a.Flows[1].ID {
		t.Errorf("two flows share the id %s", a.Flows[0].ID)
	}
	if through, known := a.Through("cmd/perf.main"); !known || len(through) != 2 {
		t.Errorf("through the shared entry name = %v, known %v", through, known)
	}
}

// A before_action runs inside a request a route already entered. Counting it as
// a way in put seventeen thousand of them ahead of the routes they belong to.
func TestAnalyze_ACallbackIsNotAnEntry(t *testing.T) {
	callback := sym("PostsController#load_post", "app/controllers/posts_controller.rb", "Post#find")
	callback.Props["root"] = "callback"
	callback.Props["symbol_kind"] = facts.SymbolMethod
	a := analyze(callback, sym("Post#find", "app/models/post.rb"))
	if len(a.Flows) != 0 || len(a.Roots) != 0 {
		t.Errorf("flows = %v, roots = %v", names(a), a.Roots)
	}
}

// method is sym for a method of a type, and iface for an interface.
func method(name, file string, calls ...string) facts.Fact {
	f := sym(name, file, calls...)
	f.Props["symbol_kind"] = facts.SymbolMethod
	return f
}

func typ(name, file, kind string, implements ...string) facts.Fact {
	f := facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "shop", File: file, Line: 1,
		Props: map[string]any{"symbol_kind": kind}}
	for _, i := range implements {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImplements, Target: i})
	}
	return f
}

// A call through an interface goes on into the class implementing it, and stops at
// an interface with more implementations than one path describes.
func TestAnalyze_CallThroughAnInterfaceReachesItsImplementation(t *testing.T) {
	ff := []facts.Fact{
		route("POST", "/login", "api.UserController.Login"),
		method("api.UserController.Login", "api/user.cs", "core.ISessions.Authenticate", "core.ILog.Write"),
		typ("core.ISessions", "core/isessions.cs", facts.SymbolInterface),
		method("core.ISessions.Authenticate", "core/isessions.cs"),
		typ("impl.Sessions", "impl/sessions.cs", facts.SymbolClass, "core.ISessions"),
		method("impl.Sessions.Authenticate", "impl/sessions.cs", "impl.Sessions.Store"),
		method("impl.Sessions.Store", "impl/sessions.cs"),
		typ("core.ILog", "core/ilog.cs", facts.SymbolInterface),
		method("core.ILog.Write", "core/ilog.cs"),
	}
	for _, n := range []string{"A", "B", "C", "D"} {
		ff = append(ff, typ("logs."+n, "logs/"+n+".cs", facts.SymbolClass, "core.ILog"),
			method("logs."+n+".Write", "logs/"+n+".cs"))
	}
	a := analyze(ff...)
	if len(a.Flows) != 1 {
		t.Fatalf("flows = %v", names(a))
	}
	if got := a.Flows[0].Metrics.Reach; got != 5 {
		t.Errorf("reach = %d, want 5: the handler, both interface methods, the implementation and what it calls; no logger", got)
	}
}

// A command of a command-line program is an entry named the way a user types it.
func TestAnalyze_ACommandIsAnEntryNamedByItsCommand(t *testing.T) {
	check := sym("pkg/command.Runner.Check", "pkg/command/check.go", "pkg/command.verdict")
	check.Props["symbol_kind"] = facts.SymbolMethod
	check.Props["command"] = "enola check"
	a := analyze(
		check,
		sym("pkg/command.verdict", "pkg/command/check.go"),
	)
	if got := names(a); !reflect.DeepEqual(got, []string{"enola check"}) {
		t.Fatalf("flows = %v, want [enola check]", got)
	}
	if a.Flows[0].Mechanism != MechanismCommand {
		t.Errorf("mechanism = %q, want %q", a.Flows[0].Mechanism, MechanismCommand)
	}
}

// In a program that serves routes, a command ranks after the routes however much it
// reaches. In one that serves none, commands rank by reach like any entry.
func TestAnalyze_CommandsRankAfterTheRoutesOfAServer(t *testing.T) {
	web := sym("cmd.runWeb", "cmd/web.go", "web.a", "web.b", "web.c")
	web.Props["command"] = "app web"
	a := analyze(
		web,
		sym("web.a", "web/a.go"), sym("web.b", "web/b.go"), sym("web.c", "web/c.go"),
		route("GET", "/items", "api.list"),
		sym("api.list", "api/list.go", "db.query"),
		sym("db.query", "db/query.go"),
	)
	if got := names(a); !reflect.DeepEqual(got, []string{"GET /items", "app web"}) {
		t.Errorf("flows = %v, want the route before the larger command", got)
	}

	check := sym("cmd.check", "cmd/check.go", "core.a", "core.b")
	check.Props["command"] = "tool check"
	list := sym("cmd.list", "cmd/list.go", "core.a")
	list.Props["command"] = "tool list"
	a = analyze(check, list, sym("core.a", "core/a.go"), sym("core.b", "core/b.go"))
	if got := names(a); !reflect.DeepEqual(got, []string{"tool check", "tool list"}) {
		t.Errorf("flows = %v, want commands by reach in a program with no routes", got)
	}
}

func widget(name, file, role string, rels ...facts.Relation) facts.Fact {
	f := typ(name, file, facts.SymbolClass)
	f.Props["flutter_role"] = role
	f.Relations = append(f.Relations, rels...)
	return f
}

func instantiates(target string) facts.Relation {
	return facts.Relation{Kind: facts.RelInstantiates, Target: target}
}

// A page widget a route names is entered through its methods. A stateful one builds
// its state class, and a widget built inside another is walked too; a plain class
// that is constructed is not.
func TestAnalyze_AWidgetPageIsWalkedThroughItsMethods(t *testing.T) {
	build := method("lib/screens.SettingsScreen.createState", "lib/screens/settings.dart")
	build.Relations = append(build.Relations, instantiates("lib/screens._SettingsState"))
	stateBuild := method("lib/screens._SettingsState.build", "lib/screens/settings.dart", "lib/core.load")
	stateBuild.Relations = append(stateBuild.Relations, instantiates("lib/widgets.Toggle"), instantiates("lib/core.Options"))
	a := analyze(
		route("GET", "/settings", "lib/screens.SettingsScreen"),
		widget("lib/screens.SettingsScreen", "lib/screens/settings.dart", "widget"),
		build,
		widget("lib/screens._SettingsState", "lib/screens/settings.dart", "widget_state"),
		stateBuild,
		sym("lib/core.load", "lib/core/load.dart"),
		widget("lib/widgets.Toggle", "lib/widgets/toggle.dart", "widget"),
		method("lib/widgets.Toggle.build", "lib/widgets/toggle.dart"),
		typ("lib/core.Options", "lib/core/options.dart", facts.SymbolClass),
		method("lib/core.Options.parse", "lib/core/options.dart"),
	)
	if len(a.Flows) != 1 {
		t.Fatalf("flows = %v", names(a))
	}
	// The page, createState, the state class, its build, load, Toggle and its build.
	if got := a.Flows[0].Metrics.Reach; got != 7 {
		t.Errorf("reach = %d, want 7: through the state class into Toggle, and not into Options", got)
	}
}

// A type a platform starts is an entry, walked through its own calls and its methods.
// A class implementing some other type is not.
func TestAnalyze_ATypeAPlatformStartsIsAnEntry(t *testing.T) {
	activity := typ("app.MainActivity", "app/MainActivity.kt", facts.SymbolClass, "ComponentActivity")
	activity.Props["language"] = "kotlin"
	onCreate := method("app.MainActivity.onCreate", "app/MainActivity.kt", "ui.render")
	swiftApp := typ("App/iOS.IsowordsApp", "App/iOS/App.swift", facts.SymbolStruct, "App")
	swiftApp.Props["language"] = "swift"
	swiftApp.Relations = append(swiftApp.Relations, facts.Relation{Kind: facts.RelCalls, Target: "ui.render"})
	other := typ("app.Repo", "app/Repo.kt", facts.SymbolClass, "Repository")
	other.Props["language"] = "kotlin"
	a := analyze(
		activity, onCreate, swiftApp, other,
		method("app.Repo.load", "app/Repo.kt", "ui.render"),
		sym("ui.render", "ui/render.kt", "ui.draw"),
		sym("ui.draw", "ui/draw.kt"),
	)
	got := map[string]string{}
	for _, f := range a.Flows {
		got[f.Name] = f.Mechanism
	}
	for _, want := range []string{"app.MainActivity", "App/iOS.IsowordsApp"} {
		if got[want] != MechanismApp {
			t.Errorf("flows = %v, want %s as an app entry", got, want)
		}
	}
	if _, ok := got["app.Repo"]; ok {
		t.Errorf("flows = %v: a class implementing a repository type is no entry", got)
	}
}

// A call into a package the calling file imports from outside the repository is
// counted as external, inside the unresolved count. A call on a value of unknown type
// is unresolved and not external.
func TestAnalyze_CountsTheCallsThatLeaveTheRepository(t *testing.T) {
	handler := sym("api.create", "api/create.go", "json.Marshal", "github.com/acme/kit.Send", "svc.Save", "db.save")
	dep := func(target, source string) facts.Fact {
		return facts.Fact{Kind: facts.KindDependency, Name: "api -> " + target, File: "api/create.go", Repo: "shop",
			Props:     map[string]any{"source": source},
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: target}}}
	}
	a := analyze(
		route("POST", "/orders", "api.create"),
		handler,
		sym("db.save", "db/save.go"),
		dep("encoding/json", "stdlib"),
		dep("github.com/acme/kit", "external"),
		dep("shop/db", "internal"),
	)
	m := a.Flows[0].Metrics
	if m.UnresolvedCalls != 3 || m.ExternalCalls != 2 {
		t.Errorf("unresolved = %d, external = %d; want 3 and 2: json.Marshal and kit.Send leave, svc.Save is a gap", m.UnresolvedCalls, m.ExternalCalls)
	}
}

// The document holds every flow, and describes only the first.
func TestDocument_HoldsEveryFlowAndDescribesTheFirst(t *testing.T) {
	var ff []facts.Fact
	for _, n := range []string{"a", "b", "c"} {
		ff = append(ff, route("GET", "/"+n, "api."+n), sym("api."+n, "api/"+n+".go", "core."+n), sym("core."+n, "core/"+n+".go"))
	}
	doc := analyze(ff...).Document(1)
	if doc.Total != 3 || len(doc.Flows) != 3 || doc.Described != 1 {
		t.Fatalf("total %d, flows %d, described %d; want 3, 3, 1", doc.Total, len(doc.Flows), doc.Described)
	}
	if doc.Flows[0].Summary == "" || doc.Flows[2].Summary != "" {
		t.Errorf("summaries %q / %q: want the first described and the last not", doc.Flows[0].Summary, doc.Flows[2].Summary)
	}
}
