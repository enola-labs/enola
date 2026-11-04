package deadmethods

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func method(name, file string, calls ...string) facts.Fact {
	rels := make([]facts.Relation, 0, len(calls))
	for _, c := range calls {
		rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: c})
	}
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Repo: "app",
		Props: map[string]any{"language": "ruby", "symbol_kind": "method"}, Relations: rels}
}

func private_(name, file string) facts.Fact {
	f := method(name, file)
	f.Props["exported"] = false
	return f
}

func ref(kind, file string, calls ...string) facts.Fact {
	rels := make([]facts.Relation, 0, len(calls))
	for _, c := range calls {
		rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: c})
	}
	return facts.Fact{Kind: kind, Name: file, File: file, Repo: "app", Relations: rels}
}

func titles(t *testing.T, fs ...facts.Fact) string {
	t.Helper()
	s := facts.NewStore()
	for _, f := range fs {
		s.Add(f)
	}
	got, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, in := range got {
		b.WriteString(in.Title + "\n")
	}
	return b.String()
}

// The two shapes, and the references that keep a method out of both: a call
// from any symbol, a template, a spec (which makes it test-only rather than
// uncalled), and the surfaces and names the reading never judges.
func TestNothingCallsItAndOnlySpecsCallIt(t *testing.T) {
	out := titles(t,
		method("Exporter#export_meetings", "app/services/exporter.rb"),
		method("Exporter#export_users", "app/services/exporter.rb"),
		method("Exporter#run", "app/services/exporter.rb", "export_users"),
		method("Insights#role_attributes", "app/services/insights.rb"),
		ref(facts.KindTestRef, "spec/services/insights_spec.rb", "role_attributes"),
		method("Channels::Indeed::Presenter#country", "app/services/channels/indeed.rb"),
		ref(facts.KindFileRef, "app/views/channels/indeed.xml.erb", "country"),
		method("Exporter#perform", "app/services/exporter.rb"),
		method("Exporter#to_h", "app/services/exporter.rb"),
		method("Api::ThingsController#show", "app/controllers/api/things_controller.rb"),
		method("Api::ThingsController#legacy_action", "app/controllers/api/things_controller.rb"),
		private_("Api::ThingsController#stale_filter", "app/controllers/api/things_controller.rb"),
		facts.Fact{Kind: facts.KindRoute, Name: "GET /api/things/:id", Repo: "app", Props: map[string]any{"handler": "api/things#show"}},
		method("Maintenance::Backfill#process_row", "app/tasks/maintenance/backfill.rb"))
	for _, want := range []string{
		"Exporter#export_meetings has no caller the graph can see",
		"Insights#role_attributes is called only from specs",
		"Api::ThingsController#stale_filter has no caller the graph can see",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	for _, never := range []string{"export_users", "country", "Exporter#perform", "Exporter#to_h", "ThingsController#show", "ThingsController#legacy_action", "Backfill"} {
		if strings.Contains(out, never) {
			t.Fatalf("%q should not be reported:\n%s", never, out)
		}
	}
}

// A name that matches a dynamic-send prefix recorded anywhere is never
// reported: `send(:"export_#{part}")` is how Exporter reaches its export_*
// methods, and the graph holds the prefix.
func TestADynamicSendPrefixKeepsItsMethodsAlive(t *testing.T) {
	dispatcher := method("Exporter#run", "app/services/exporter.rb")
	dispatcher.Props["dynamic_send_prefixes"] = []string{"export_"}
	out := titles(t,
		dispatcher,
		method("Exporter#export_meetings", "app/services/exporter.rb"),
		method("Exporter#summarize", "app/services/exporter.rb"),
		method("Exporter#total", "app/services/exporter.rb"),
		method("Report#build", "app/services/report.rb", "total"))
	if strings.Contains(out, "export_meetings") {
		t.Fatalf("a prefixed name was reported:\n%s", out)
	}
	if !strings.Contains(out, "Exporter#summarize has no caller") {
		t.Fatalf("the unprefixed uncalled method was not reported:\n%s", out)
	}
}

// A method name handed to another call as a symbol is a reference: the
// deletion jobs pass `:requisitions_done` for a sibling to dispatch later.
func TestANameHandedAsASymbolKeepsItsMethodAlive(t *testing.T) {
	caller := method("CompanyDeletion::RequisitionsJob#perform", "app/jobs/company_deletion/requisitions_job.rb", "perform_async")
	caller.Relations = append(caller.Relations, facts.Relation{Kind: facts.RelNames, Target: "requisitions_done"})
	out := titles(t, caller,
		method("CompanyDeletion::DestroyCompanyJob#requisitions_done", "app/jobs/company_deletion/destroy_company_job.rb"),
		method("CompanyDeletion::DestroyCompanyJob#orphaned", "app/jobs/company_deletion/destroy_company_job.rb"))
	if strings.Contains(out, "requisitions_done") {
		t.Fatalf("a name handed as a symbol was reported:\n%s", out)
	}
	if !strings.Contains(out, "orphaned has no caller") {
		t.Fatalf("the genuinely unnamed method was not reported:\n%s", out)
	}
}

// `CACHED_ATTRIBUTES = CachedAttributes.instance_methods(false)` consumes every
// method of the class by reflection; the graph holds the call, and the reading
// treats the whole class as referenced.
func TestAClassReadByReflectionKeepsEveryMethodAlive(t *testing.T) {
	out := titles(t,
		method("Stats::CachedAttributes#wallet_id", "app/services/stats.rb"),
		method("Stats::CachedAttributes#cookie_setting_id", "app/services/stats.rb"),
		method("Stats#paths", "app/services/stats.rb", "CachedAttributes.instance_methods", "wallet_id"),
		method("Other#stray", "app/services/other.rb"))
	if strings.Contains(out, "CachedAttributes") {
		t.Fatalf("a reflected class's methods were reported:\n%s", out)
	}
	if !strings.Contains(out, "Other#stray has no caller") {
		t.Fatalf("the unreflected method was not reported:\n%s", out)
	}
}

// An engine's controllers live in its isolated namespace, which a route's handler
// string leaves out (`shelves#add` for `Shop::ShelvesController#add`). The route's
// handled_by target names the action as resolved, and a routed action is never
// reported, even where packwerk privacy reads its public methods as unexported.
func TestARoutedEngineActionIsNotReported(t *testing.T) {
	out := titles(t,
		private_("Shop::ShelvesController#add", "engines/shop/app/controllers/shop/shelves_controller.rb"),
		private_("Shop::ShelvesController#unrouted", "engines/shop/app/controllers/shop/shelves_controller.rb"),
		method("Shop::Shelves::Add#call", "engines/shop/app/domain/shop/shelves/add.rb", "save"),
		facts.Fact{Kind: facts.KindRoute, Name: "/shop/shelves/add", Repo: "app",
			Props:     map[string]any{"handler": "shelves#add", "method": "POST"},
			Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "Shop::ShelvesController#add"}}})
	if strings.Contains(out, "ShelvesController#add") {
		t.Fatalf("a routed action was reported:\n%s", out)
	}
	if !strings.Contains(out, "Shop::ShelvesController#unrouted has no caller the graph can see") {
		t.Fatalf("the unrouted method should still be reported:\n%s", out)
	}
}

// In a cluster snapshot each repository's methods are judged by its own callers:
// another repository naming the same word neither keeps a method alive nor counts as
// the spec that tests it.
func TestCallersAreCountedWithinTheirRepository(t *testing.T) {
	inOther := func(f facts.Fact) facts.Fact { f.Repo = "other"; return f }
	out := titles(t,
		method("Jobs::Cleanup#retry_later", "app/jobs/cleanup.rb"),
		inOther(ref(facts.KindTestRef, "spec/jobs/sync_spec.rb", "retry_later")),
		method("Jobs::Cleanup#flush", "app/jobs/cleanup.rb"),
		inOther(method("Worker#run", "app/services/worker.rb", "flush")))
	for _, want := range []string{
		"Jobs::Cleanup#retry_later has no caller the graph can see",
		"Jobs::Cleanup#flush has no caller the graph can see",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
