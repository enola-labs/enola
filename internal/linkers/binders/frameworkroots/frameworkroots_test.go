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

// A superclass written relative to the class's namespace (`< Base` inside
// Grape::Validations::Validators) resolves to the innermost enclosing namespace
// that declares it, so the registering base's contract reaches the subclass.
func TestBind_RelativeSuperclassResolvesThroughTheNamespace(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		symbol("Grape::Validations::Validators::Base", facts.SymbolClass, "lib/grape/validations/validators/base.rb", nil),
		symbol("Grape::Validations::Validators::Base.inherited", facts.SymbolFunc, "lib/grape/validations/validators/base.rb", nil, calls("Validations.register", "Validations")...),
		symbol("Grape::Validations::Validators::Base#validate_param!", facts.SymbolMethod, "lib/grape/validations/validators/base.rb", nil),
		symbol("Grape::Validations::Validators::PresenceValidator", facts.SymbolClass, "lib/grape/validations/validators/presence_validator.rb", map[string]any{"superclass": "Base"}),
		symbol("Grape::Validations::Validators::PresenceValidator#validate_param!", facts.SymbolMethod, "lib/grape/validations/validators/presence_validator.rb", nil),
		symbol("Grape::Validations::Validators::PresenceValidator#helper", facts.SymbolMethod, "lib/grape/validations/validators/presence_validator.rb", nil),
	)
	bind(t, store)

	for name, want := range map[string]string{
		"Grape::Validations::Validators::PresenceValidator#validate_param!": MechanismRegistry,
		"Grape::Validations::Validators::PresenceValidator#helper":          "",
	} {
		if got := propOf(store, facts.KindSymbol, name, RootProp); got != want {
			t.Errorf("%s: root = %q, want %q", name, got, want)
		}
	}
}

// Rails registers railties, engines and generators through their base class's
// own inherited hook and loads or runs them from there: the subclass is a root,
// and so is each public step of a generator. A class with an unrelated library
// superclass is not.
func TestBind_FrameworkLoadedSubclassesAreRoots(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		symbol("Devise::Engine", facts.SymbolClass, "lib/devise/rails.rb", map[string]any{"superclass": "::Rails::Engine"}),
		symbol("Devise::Generators::InstallGenerator", facts.SymbolClass, "lib/generators/devise/install_generator.rb", map[string]any{"superclass": "Rails::Generators::Base"}),
		symbol("Devise::Generators::InstallGenerator#copy_initializer", facts.SymbolMethod, "lib/generators/devise/install_generator.rb", nil),
		symbol("Devise::Generators::InstallGenerator#private_step", facts.SymbolMethod, "lib/generators/devise/install_generator.rb", map[string]any{"exported": false}),
		symbol("Devise::Generators::OrmGenerator", facts.SymbolClass, "lib/generators/devise/orm_generator.rb", map[string]any{"superclass": "Devise::Generators::InstallGenerator"}),
		symbol("Devise::Delivery", facts.SymbolClass, "lib/devise/delivery.rb", map[string]any{"superclass": "Struct"}),
	)
	bind(t, store)

	for name, want := range map[string]string{
		"Devise::Engine":                                        MechanismRegistry,
		"Devise::Generators::InstallGenerator":                  MechanismRegistry,
		"Devise::Generators::InstallGenerator#copy_initializer": MechanismRegistry,
		"Devise::Generators::InstallGenerator#private_step":     "",
		"Devise::Generators::OrmGenerator":                      MechanismRegistry,
		"Devise::Delivery":                                      "",
	} {
		if got := propOf(store, facts.KindSymbol, name, RootProp); got != want {
			t.Errorf("%s: root = %q, want %q", name, got, want)
		}
	}
}

func mixinOf(includer, module string) facts.Fact {
	return facts.Fact{Kind: facts.KindDependency, Name: includer + " -> " + module, File: "app/x.rb", Repo: "app",
		Props: map[string]any{"language": "ruby", "mixin_kind": "include"}, Relations: []facts.Relation{{Kind: facts.RelImplements, Target: module}}}
}

func instantiates(targets ...string) []facts.Relation {
	out := []facts.Relation{}
	for _, t := range targets {
		out = append(out, facts.Relation{Kind: facts.RelInstantiates, Target: t})
	}
	return out
}

// A module's methods are looked up from its includers the way Ruby does, so a
// bare call from a reached controller action reaches the concern's method; the
// callbacks a concern registers in its included block root on every
// framework-bound includer and reach what they call; a module included only
// into plain classes roots nothing.
func TestBind_IncludedModulesAnswerTheirIncludersCalls(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		route("pages#show"),
		symbol("PagesController", facts.SymbolClass, "app/controllers/pages_controller.rb", map[string]any{"rails_component": "controller"}),
		symbol("PagesController#show", facts.SymbolMethod, "app/controllers/pages_controller.rb", nil, calls("from_cdn?")...),
		mixinOf("PagesController", "Cacheable"),
		symbol("Cacheable", facts.SymbolInterface, "app/concerns/cacheable.rb", nil, names("set_headers")...),
		symbol("Cacheable#set_headers", facts.SymbolMethod, "app/concerns/cacheable.rb", nil, calls("surrogate_keys")...),
		symbol("Cacheable#surrogate_keys", facts.SymbolMethod, "app/concerns/cacheable.rb", nil),
		symbol("Cacheable#from_cdn?", facts.SymbolMethod, "app/concerns/cacheable.rb", nil),
		symbol("Cacheable#unused", facts.SymbolMethod, "app/concerns/cacheable.rb", nil),
		symbol("Plain", facts.SymbolClass, "lib/plain.rb", nil),
		mixinOf("Plain", "Helpers"),
		symbol("Helpers", facts.SymbolInterface, "lib/helpers.rb", nil, names("tidy")...),
		symbol("Helpers#tidy", facts.SymbolMethod, "lib/helpers.rb", nil),
	)
	bind(t, store)
	for name, want := range map[string][2]string{
		"Cacheable#set_headers":    {MechanismCallback, ""},
		"Cacheable#surrogate_keys": {"", MechanismCallback},
		"Cacheable#from_cdn?":      {"", MechanismRoute},
		"Cacheable#unused":         {"", ""},
		"Helpers#tidy":             {"", ""},
	} {
		if got := propOf(store, facts.KindSymbol, name, RootProp); got != want[0] {
			t.Errorf("%s root = %q, want %q", name, got, want[0])
		}
		if got := propOf(store, facts.KindSymbol, name, ReachedFromProp); got != want[1] {
			t.Errorf("%s reached_from = %q, want %q", name, got, want[1])
		}
	}
}

// Classes a framework loads by their base class root the methods the library
// calls: a maintenance task's collection and process, an admin resource's
// fields and an admin action's handle, a filter's apply, and an MCP tool's
// singleton call, its base class named relative to its namespace. The same
// method names on a class of no such base root nothing.
func TestBind_FrameworkLoadedClassesRootWhatTheLibraryCalls(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		symbol("Maintenance::BackfillTask", facts.SymbolClass, "app/tasks/maintenance/backfill_task.rb", map[string]any{"superclass": "MaintenanceTasks::Task"}),
		symbol("Maintenance::BackfillTask#collection", facts.SymbolMethod, "app/tasks/maintenance/backfill_task.rb", nil, calls("scope")...),
		symbol("Maintenance::BackfillTask#process", facts.SymbolMethod, "app/tasks/maintenance/backfill_task.rb", nil),
		symbol("Maintenance::BackfillTask#scope", facts.SymbolMethod, "app/tasks/maintenance/backfill_task.rb", nil),
		symbol("Avo::Resources::Thing", facts.SymbolClass, "app/avo/resources/thing.rb", map[string]any{"superclass": "Avo::BaseResource"}),
		symbol("Avo::Resources::Thing#fields", facts.SymbolMethod, "app/avo/resources/thing.rb", nil),
		symbol("Avo::Resources::Thing#actions", facts.SymbolMethod, "app/avo/resources/thing.rb", nil),
		symbol("Avo::Actions::Recheck", facts.SymbolClass, "app/avo/actions/recheck.rb", map[string]any{"superclass": "Avo::BaseAction"}),
		symbol("Avo::Actions::Recheck#handle", facts.SymbolMethod, "app/avo/actions/recheck.rb", nil),
		symbol("Avo::Filters::Status", facts.SymbolClass, "app/avo/filters/status.rb", map[string]any{"superclass": "Avo::Filters::SelectFilter"}),
		symbol("Avo::Filters::Status#apply", facts.SymbolMethod, "app/avo/filters/status.rb", nil),
		symbol("Tools::Application", facts.SymbolClass, "app/tools/application.rb", map[string]any{"superclass": "::MCP::Tool"}),
		symbol("Tools::AddThing", facts.SymbolClass, "app/tools/add_thing.rb", map[string]any{"superclass": "Application"}),
		symbol("Tools::AddThing.call", facts.SymbolFunc, "app/tools/add_thing.rb", nil, calls("existing")...),
		symbol("Tools::AddThing.existing", facts.SymbolFunc, "app/tools/add_thing.rb", nil),
		symbol("Plain", facts.SymbolClass, "lib/plain.rb", nil),
		symbol("Plain#process", facts.SymbolMethod, "lib/plain.rb", nil),
		symbol("Plain.call", facts.SymbolFunc, "lib/plain.rb", nil),
	)
	bind(t, store)
	for name, want := range map[string][2]string{
		"Maintenance::BackfillTask#collection": {MechanismTask, ""},
		"Maintenance::BackfillTask#process":    {MechanismTask, ""},
		"Maintenance::BackfillTask#scope":      {"", MechanismTask},
		"Avo::Resources::Thing#fields":         {MechanismHook, ""},
		"Avo::Resources::Thing#actions":        {MechanismHook, ""},
		"Avo::Actions::Recheck#handle":         {MechanismHook, ""},
		"Avo::Filters::Status#apply":           {MechanismHook, ""},
		"Tools::AddThing.call":                 {MechanismHook, ""},
		"Tools::AddThing.existing":             {"", MechanismHook},
		"Plain#process":                        {"", ""},
		"Plain.call":                           {"", ""},
	} {
		if got := propOf(store, facts.KindSymbol, name, RootProp); got != want[0] {
			t.Errorf("%s root = %q, want %q", name, got, want[0])
		}
		if got := propOf(store, facts.KindSymbol, name, ReachedFromProp); got != want[1] {
			t.Errorf("%s reached_from = %q, want %q", name, got, want[1])
		}
	}
}

// An instance built by reached code runs its initialize, and a view component
// it builds runs the lifecycle the library calls on render, inherited where
// the component does not define it; a component only named as a value is not
// rendered by that and reaches none of its hooks.
func TestBind_RenderedComponentsAreReachedFromTheirRenderer(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		route("cards#show"),
		symbol("CardsController", facts.SymbolClass, "app/controllers/cards_controller.rb", map[string]any{"rails_component": "controller"}),
		symbol("CardsController#show", facts.SymbolMethod, "app/controllers/cards_controller.rb", nil, append(instantiates("Cards::Card", "Report"), calls("Cards::Named")...)...),
		symbol("ApplicationComponent", facts.SymbolClass, "app/components/application_component.rb", map[string]any{"superclass": "ViewComponent::Base", "rails_component": "component"}),
		symbol("ApplicationComponent#render?", facts.SymbolMethod, "app/components/application_component.rb", nil),
		symbol("Cards::Card", facts.SymbolClass, "app/components/cards/card.rb", map[string]any{"superclass": "ApplicationComponent", "rails_component": "component"}),
		symbol("Cards::Card#initialize", facts.SymbolMethod, "app/components/cards/card.rb", nil),
		symbol("Cards::Card#before_render", facts.SymbolMethod, "app/components/cards/card.rb", nil, calls("title")...),
		symbol("Cards::Card#title", facts.SymbolMethod, "app/components/cards/card.rb", nil),
		symbol("Cards::Named", facts.SymbolClass, "app/components/cards/named.rb", map[string]any{"superclass": "ViewComponent::Base", "rails_component": "component"}),
		symbol("Cards::Named#before_render", facts.SymbolMethod, "app/components/cards/named.rb", nil),
		symbol("Report", facts.SymbolClass, "app/models/report.rb", nil),
		symbol("Report#initialize", facts.SymbolMethod, "app/models/report.rb", nil),
		symbol("Report#before_render", facts.SymbolMethod, "app/models/report.rb", nil),
	)
	bind(t, store)
	for name, want := range map[string]string{
		"Cards::Card#initialize":       MechanismRoute,
		"Cards::Card#before_render":    MechanismRoute,
		"Cards::Card#title":            MechanismRoute,
		"ApplicationComponent#render?": MechanismRoute,
		"Cards::Named#before_render":   "",
		"Report#initialize":            MechanismRoute,
		"Report#before_render":         "",
	} {
		if got := propOf(store, facts.KindSymbol, name, ReachedFromProp); got != want {
			t.Errorf("%s reached_from = %q, want %q", name, got, want)
		}
	}
}

// A constant a call names is found the way Ruby finds it, through the
// caller's enclosing namespaces outward, so a tool calling Schemas.build
// inside Tools reaches Tools::Schemas.build; a name no namespace holds stays
// unresolved.
func TestBind_QualifiedCallsResolveThroughTheCallersNamespace(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		symbol("Tools::Base", facts.SymbolClass, "app/tools/base.rb", map[string]any{"superclass": "MCP::Tool"}),
		symbol("Tools::Fetch", facts.SymbolClass, "app/tools/fetch.rb", map[string]any{"superclass": "Base"}),
		symbol("Tools::Fetch.call", facts.SymbolFunc, "app/tools/fetch.rb", nil, calls("Schemas.build", "Presenter", "Missing.thing")...),
		symbol("Tools::Schemas", facts.SymbolInterface, "app/tools/schemas.rb", nil),
		symbol("Tools::Schemas.build", facts.SymbolFunc, "app/tools/schemas.rb", nil),
		symbol("Tools::Presenter", facts.SymbolClass, "app/tools/presenter.rb", nil),
		symbol("Schemas.build", facts.SymbolFunc, "lib/schemas.rb", nil),
	)
	bind(t, store)
	for name, want := range map[string]string{
		"Tools::Schemas.build": MechanismHook,
		"Tools::Presenter":     MechanismHook,
		"Schemas.build":        "",
	} {
		if got := propOf(store, facts.KindSymbol, name, ReachedFromProp); got != want {
			t.Errorf("%s reached_from = %q, want %q", name, got, want)
		}
	}
}
