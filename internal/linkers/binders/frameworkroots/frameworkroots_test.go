package frameworkroots

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func symbol(name, kind, file string, props map[string]any, relations ...facts.Relation) facts.Fact {
	p := map[string]any{"symbol_kind": kind, "language": "ruby", "framework": "rails", "exported": true}
	for k, v := range props {
		p[k] = v
	}
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Repo: "app", Props: p, Relations: relations}
}

func calls(targets ...string) []facts.Relation {
	out := []facts.Relation{}
	for _, t := range targets {
		out = append(out, facts.Relation{Kind: facts.RelCalls, Target: t})
	}
	return out
}

func names(targets ...string) []facts.Relation {
	out := []facts.Relation{}
	for _, t := range targets {
		out = append(out, facts.Relation{Kind: facts.RelNames, Target: t})
	}
	return out
}

func route(handler string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: "/things", File: "config/routes.rb", Repo: "app",
		Props: map[string]any{"framework": "rails", "language": "ruby", "method": "GET", "handler": handler}}
}

func propOf(store *facts.Store, kind, name, prop string) string {
	for _, f := range store.ByKind(kind) {
		if f.Name == name {
			return f.PropString(prop)
		}
	}
	return ""
}

func bind(t *testing.T, store *facts.Store) {
	t.Helper()
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
}

// A route roots the action it resolves to and binds to it; a handler naming
// no known action roots nothing.
func TestBind_RoutesRootTheActionsTheyResolveTo(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		route("admin/accounts#show"),
		route("vanished#index"),
		symbol("Admin::AccountsController", facts.SymbolClass, "app/controllers/admin/accounts_controller.rb", map[string]any{"rails_component": "controller"}),
		symbol("Admin::AccountsController#show", facts.SymbolMethod, "app/controllers/admin/accounts_controller.rb", nil),
	)
	bind(t, store)
	if got := propOf(store, facts.KindSymbol, "Admin::AccountsController#show", RootProp); got != MechanismRoute {
		t.Fatalf("action root = %q", got)
	}
	bound := 0
	for _, r := range store.ByKind(facts.KindRoute) {
		if r.HasRelation(facts.RelHandledBy, "Admin::AccountsController#show") {
			bound++
			if r.PropString(RootProp) != MechanismRoute {
				t.Fatal("bound route carries no root")
			}
		} else if r.PropString(RootProp) != "" {
			t.Fatalf("unresolved route %s carries a root", r.PropString("handler"))
		}
	}
	if bound != 1 {
		t.Fatalf("want one bound route, got %d", bound)
	}
}

// Each table row requires class-level evidence: a perform is a job root on a
// job component, an ActiveJob subclass or a Sidekiq includer, and nothing
// else; a service object's call is not a root.
func TestBind_FrameworkInvokedMethodsRootOnlyWithClassEvidence(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		symbol("ImportJob", facts.SymbolClass, "app/jobs/import_job.rb", map[string]any{"rails_component": "job"}),
		symbol("ImportJob#perform", facts.SymbolMethod, "app/jobs/import_job.rb", nil),
		symbol("Legacy::Worker", facts.SymbolClass, "lib/legacy/worker.rb", nil),
		symbol("Legacy::Worker#perform", facts.SymbolMethod, "lib/legacy/worker.rb", nil),
		facts.Fact{Kind: facts.KindDependency, Name: "Legacy::Worker -> Sidekiq::Worker", File: "lib/legacy/worker.rb", Repo: "app",
			Props: map[string]any{"language": "ruby", "mixin_kind": "include"}, Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "Sidekiq::Worker"}}},
		symbol("Reports::Build", facts.SymbolClass, "app/services/reports/build.rb", map[string]any{"rails_component": "service"}),
		symbol("Reports::Build#call", facts.SymbolMethod, "app/services/reports/build.rb", nil),
		symbol("Reports::Build#perform", facts.SymbolMethod, "app/services/reports/build.rb", nil),
		symbol("CandidateMailer", facts.SymbolClass, "app/mailers/candidate_mailer.rb", map[string]any{"rails_component": "mailer"}),
		symbol("CandidateMailer#welcome", facts.SymbolMethod, "app/mailers/candidate_mailer.rb", nil),
		symbol("CandidateMailer#headers_for", facts.SymbolMethod, "app/mailers/candidate_mailer.rb", map[string]any{"exported": false}),
		symbol("ChatChannel", facts.SymbolClass, "app/channels/chat_channel.rb", map[string]any{"rails_component": "channel"}),
		symbol("ChatChannel#subscribed", facts.SymbolMethod, "app/channels/chat_channel.rb", nil),
		symbol("AddIndexToThings", facts.SymbolClass, "db/migrate/20260821000000_add_index_to_things.rb", map[string]any{"superclass": "ActiveRecord::Migration[8.0]"}),
		symbol("AddIndexToThings#change", facts.SymbolMethod, "db/migrate/20260821000000_add_index_to_things.rb", nil),
	)
	bind(t, store)
	want := map[string]string{
		"ImportJob#perform":           MechanismJob,
		"Legacy::Worker#perform":      MechanismJob,
		"Reports::Build#call":         "",
		"Reports::Build#perform":      "",
		"CandidateMailer#welcome":     MechanismMailer,
		"CandidateMailer#headers_for": "",
		"ChatChannel#subscribed":      MechanismHook,
		"AddIndexToThings#change":     MechanismTask,
	}
	for name, mechanism := range want {
		if got := propOf(store, facts.KindSymbol, name, RootProp); got != mechanism {
			t.Errorf("%s root = %q, want %q", name, got, mechanism)
		}
	}
}

// A method its own class body names is a callback root; a name the class
// body uses that is not one of its methods roots nothing; and the walk from
// a root reaches a bare-named method only through the caller's own class.
func TestBind_CallbacksAndReachabilityFollowTheClass(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		route("things#index"),
		symbol("ThingsController", facts.SymbolClass, "app/controllers/things_controller.rb", map[string]any{"rails_component": "controller"}, names("load_thing", "things")...),
		symbol("ThingsController#index", facts.SymbolMethod, "app/controllers/things_controller.rb", nil, calls("render_list", "Things::Query.run", "helper_elsewhere")...),
		symbol("ThingsController#load_thing", facts.SymbolMethod, "app/controllers/things_controller.rb", map[string]any{"exported": false}),
		symbol("ThingsController#render_list", facts.SymbolMethod, "app/controllers/things_controller.rb", map[string]any{"exported": false}),
		symbol("Things::Query", facts.SymbolClass, "app/queries/things/query.rb", nil),
		symbol("Things::Query.run", facts.SymbolMethod, "app/queries/things/query.rb", nil, calls("scope_for")...),
		symbol("Things::Query.scope_for", facts.SymbolMethod, "app/queries/things/query.rb", nil),
		symbol("Other#helper_elsewhere", facts.SymbolMethod, "app/models/other.rb", nil),
		symbol("Other#idle", facts.SymbolMethod, "app/models/other.rb", nil),
	)
	for i := 0; i < 2; i++ {
		bind(t, store)
	}
	if got := propOf(store, facts.KindSymbol, "ThingsController#load_thing", RootProp); got != MechanismCallback {
		t.Fatalf("callback root = %q", got)
	}
	for name, want := range map[string]string{
		"ThingsController#render_list": MechanismRoute,
		"Things::Query.run":            MechanismRoute,
		"Things::Query.scope_for":      MechanismRoute,
		"Other#helper_elsewhere":       "",
		"Other#idle":                   "",
	} {
		if got := propOf(store, facts.KindSymbol, name, ReachedFromProp); got != want {
			t.Errorf("%s reached_from = %q, want %q", name, got, want)
		}
	}
	if got := propOf(store, facts.KindSymbol, "ThingsController#index", ReachedFromProp); got != "" {
		t.Fatalf("a root is not also reached: %q", got)
	}
}

// A library a class mixes in calls methods of its own naming on that class:
// Sidekiq's iterable jobs drive build_enumerator and each_iteration, Pundit
// asks pundit_user. The row applies through the mixin fact and nowhere else.
func TestBind_MixedInLibrariesRootTheHooksTheyCall(t *testing.T) {
	store := facts.NewStore()
	mixin := func(includer, module string) facts.Fact {
		return facts.Fact{Kind: facts.KindDependency, Name: includer + " -> " + module, File: "app/x.rb", Repo: "app",
			Props: map[string]any{"language": "ruby", "mixin_kind": "include"}, Relations: []facts.Relation{{Kind: facts.RelImplements, Target: module}}}
	}
	store.Add(
		symbol("TrackEventsWorker", facts.SymbolClass, "app/workers/track_events_worker.rb", nil),
		symbol("TrackEventsWorker#build_enumerator", facts.SymbolMethod, "app/workers/track_events_worker.rb", nil),
		symbol("TrackEventsWorker#each_iteration", facts.SymbolMethod, "app/workers/track_events_worker.rb", nil),
		symbol("TrackEventsWorker#helper", facts.SymbolMethod, "app/workers/track_events_worker.rb", nil),
		mixin("TrackEventsWorker", "Sidekiq::Job::Iterable"),
		symbol("Api::BaseController", facts.SymbolClass, "app/controllers/api/base_controller.rb", map[string]any{"rails_component": "controller"}),
		symbol("Api::BaseController#pundit_user", facts.SymbolMethod, "app/controllers/api/base_controller.rb", nil),
		mixin("Api::BaseController", "Pundit::Authorization"),
		symbol("Plain", facts.SymbolClass, "lib/plain.rb", nil),
		symbol("Plain#each_iteration", facts.SymbolMethod, "lib/plain.rb", nil),
	)
	bind(t, store)
	for name, want := range map[string]string{
		"TrackEventsWorker#build_enumerator": MechanismHook,
		"TrackEventsWorker#each_iteration":   MechanismHook,
		"TrackEventsWorker#helper":           "",
		"Api::BaseController#pundit_user":    MechanismHook,
		"Plain#each_iteration":               "",
	} {
		if got := propOf(store, facts.KindSymbol, name, RootProp); got != want {
			t.Errorf("%s root = %q, want %q", name, got, want)
		}
	}
}

// What a superclass includes or is, its subclasses inherit: a controller whose
// base class includes Pundit answers pundit_params_for for the library, a
// worker whose base class is a job runs perform for the queue, and a
// library base class names the hooks it calls on every subclass.
func TestBind_HooksAndJobsAreInheritedThroughTheSuperclassChain(t *testing.T) {
	store := facts.NewStore()
	mixin := func(includer, module string) facts.Fact {
		return facts.Fact{Kind: facts.KindDependency, Name: includer + " -> " + module, File: "app/x.rb", Repo: "app",
			Props: map[string]any{"language": "ruby", "mixin_kind": "include"}, Relations: []facts.Relation{{Kind: facts.RelImplements, Target: module}}}
	}
	store.Add(
		symbol("App::BaseController", facts.SymbolClass, "app/controllers/app/base_controller.rb", map[string]any{"rails_component": "controller"}),
		mixin("App::BaseController", "Pundit::Authorization"),
		symbol("App::Api::NotesController", facts.SymbolClass, "app/controllers/app/api/notes_controller.rb", map[string]any{"rails_component": "controller", "superclass": "App::BaseController"}),
		symbol("App::Api::NotesController#pundit_params_for", facts.SymbolMethod, "app/controllers/app/api/notes_controller.rb", map[string]any{"exported": false}),
		symbol("BaseImportJob", facts.SymbolClass, "app/jobs/base_import_job.rb", map[string]any{"rails_component": "job"}),
		symbol("Imports::CsvJob", facts.SymbolClass, "lib/imports/csv_job.rb", map[string]any{"superclass": "BaseImportJob"}),
		symbol("Imports::CsvJob#perform", facts.SymbolMethod, "lib/imports/csv_job.rb", nil),
		symbol("Api::Scim::V2::GroupsController", facts.SymbolClass, "app/controllers/api/scim/v2/groups_controller.rb", map[string]any{"rails_component": "controller", "superclass": "Scimitar::ActiveRecordBackedResourcesController"}),
		symbol("Api::Scim::V2::GroupsController#storage_class", facts.SymbolMethod, "app/controllers/api/scim/v2/groups_controller.rb", map[string]any{"exported": false}),
		symbol("Loop::A", facts.SymbolClass, "lib/loop/a.rb", map[string]any{"superclass": "Loop::B"}),
		symbol("Loop::B", facts.SymbolClass, "lib/loop/b.rb", map[string]any{"superclass": "Loop::A"}),
		symbol("Loop::A#perform", facts.SymbolMethod, "lib/loop/a.rb", nil),
	)
	bind(t, store)
	for name, want := range map[string]string{
		"App::Api::NotesController#pundit_params_for":   MechanismHook,
		"Imports::CsvJob#perform":                       MechanismJob,
		"Api::Scim::V2::GroupsController#storage_class": MechanismHook,
		"Loop::A#perform":                               "",
	} {
		if got := propOf(store, facts.KindSymbol, name, RootProp); got != want {
			t.Errorf("%s root = %q, want %q", name, got, want)
		}
	}
}

// A mixin whose included hook hands the includer to another constant enrols it
// in a registry, and the registry calls what the mixin declares. A base class's
// inherited hook does the same for subclasses. Each row names a member and the
// root it should end up with ("" for none).
func TestBind_RegisteringHooksRootTheirContract(t *testing.T) {
	store := facts.NewStore()
	mixin := func(includer, module string) facts.Fact {
		return facts.Fact{Kind: facts.KindDependency, Name: includer + " -> " + module, File: "app/x.rb", Repo: "app",
			Props: map[string]any{"language": "ruby", "mixin_kind": "include"}, Relations: []facts.Relation{{Kind: facts.RelImplements, Target: module}}}
	}
	store.Add(
		// The registering mixin: its hook calls a method on another constant.
		symbol("Audit::Trackable", facts.SymbolInterface, "app/audit/trackable.rb", nil),
		symbol("Audit::Trackable.included", facts.SymbolFunc, "app/audit/trackable.rb", nil, calls("Audit::Registry.register", "Audit::Registry")...),
		symbol("Audit::Trackable.run", facts.SymbolFunc, "app/audit/trackable.rb", nil),
		symbol("Audit::Registry", facts.SymbolClass, "app/audit/registry.rb", nil),
		symbol("Audit::Registry.register", facts.SymbolFunc, "app/audit/registry.rb", nil),

		// A singleton implementation, recorded as a function.
		symbol("Orders::Cleanup", facts.SymbolClass, "app/orders/cleanup.rb", nil),
		symbol("Orders::Cleanup.run", facts.SymbolFunc, "app/orders/cleanup.rb", nil),
		symbol("Orders::Cleanup.helper", facts.SymbolFunc, "app/orders/cleanup.rb", nil),
		mixin("Orders::Cleanup", "Audit::Trackable"),

		// An instance implementation of the same contract.
		symbol("Billing::Cleanup", facts.SymbolClass, "app/billing/cleanup.rb", nil),
		symbol("Billing::Cleanup#run", facts.SymbolMethod, "app/billing/cleanup.rb", nil),
		mixin("Billing::Cleanup", "Audit::Trackable"),

		// A mixin whose hook only configures the class registers nothing.
		symbol("Audit::Helpers", facts.SymbolInterface, "app/audit/helpers.rb", nil),
		symbol("Audit::Helpers.included", facts.SymbolFunc, "app/audit/helpers.rb", nil, calls("base.extend")...),
		symbol("Audit::Helpers.run", facts.SymbolFunc, "app/audit/helpers.rb", nil),
		symbol("Shipping::Cleanup", facts.SymbolClass, "app/shipping/cleanup.rb", nil),
		symbol("Shipping::Cleanup.run", facts.SymbolFunc, "app/shipping/cleanup.rb", nil),
		mixin("Shipping::Cleanup", "Audit::Helpers"),

		// A registering base class enrols its subclasses, not itself.
		symbol("Plugins::Base", facts.SymbolClass, "app/plugins/base.rb", nil),
		symbol("Plugins::Base.inherited", facts.SymbolFunc, "app/plugins/base.rb", nil, calls("Plugins::Catalog.add")...),
		symbol("Plugins::Base#call", facts.SymbolMethod, "app/plugins/base.rb", nil),
		symbol("Plugins::Export", facts.SymbolClass, "app/plugins/export.rb", map[string]any{"superclass": "Plugins::Base"}),
		symbol("Plugins::Export#call", facts.SymbolMethod, "app/plugins/export.rb", nil),
		symbol("Plugins::Export#format", facts.SymbolMethod, "app/plugins/export.rb", nil),
	)
	bind(t, store)

	cases := []struct {
		name, want string
	}{
		{"Orders::Cleanup.run", MechanismRegistry},
		{"Orders::Cleanup.helper", ""},
		{"Billing::Cleanup#run", MechanismRegistry},
		{"Audit::Trackable.run", ""}, // the contract's declaration, not an implementation
		{"Audit::Trackable.included", ""},
		{"Shipping::Cleanup.run", ""},
		{"Plugins::Export#call", MechanismRegistry},
		{"Plugins::Export#format", ""},
		{"Plugins::Base#call", ""},
	}
	for _, c := range cases {
		if got := propOf(store, facts.KindSymbol, c.name, RootProp); got != c.want {
			t.Errorf("%s: root = %q, want %q", c.name, got, c.want)
		}
	}
}
