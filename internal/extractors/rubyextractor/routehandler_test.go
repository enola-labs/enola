package rubyextractor

import (
	"maps"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// routeIndex parses a route file and returns "<METHOD> <path>" -> fact.
func routeIndex(t *testing.T, src string) map[string]facts.Fact {
	t.Helper()
	out := map[string]facts.Fact{}
	for _, f := range parseRouteFileAST([]byte(src), "config/routes.rb") {
		m, _ := f.Props["method"].(string)
		out[m+" "+f.Name] = f
	}
	return out
}

func routeKeys(m map[string]facts.Fact) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, "\n  ")
}

// handledBy returns the target of a fact's handled_by relation, or "".
func handledBy(f facts.Fact) string {
	for _, r := range f.Relations {
		if r.Kind == facts.RelHandledBy {
			return r.Target
		}
	}
	return ""
}

// TestRouteHandler_ResourcesDeriveController is the core of the change: before it, a
// `resources` declaration produced routes with no handler at all, so a Rails route was
// an isolated graph node — impact analysis from a controller could not reach the
// endpoints it serves, and a controller reached only through the route table read as
// dead code.
func TestRouteHandler_ResourcesDeriveController(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  namespace :api do
    namespace :v2 do
      resources :posts, only: [:index, :show]
    end
  end
end
`)
	for _, tc := range []struct{ key, handler, symbol string }{
		{"GET /api/v2/posts", "api/v2/posts#index", "Api::V2::PostsController#index"},
		{"GET /api/v2/posts/:id", "api/v2/posts#show", "Api::V2::PostsController#show"},
	} {
		f, ok := idx[tc.key]
		if !ok {
			t.Fatalf("missing %q; have:\n  %s", tc.key, routeKeys(idx))
		}
		if got := f.Props["handler"]; got != tc.handler {
			t.Errorf("%s handler = %v, want %q", tc.key, got, tc.handler)
		}
		if got := handledBy(f); got != tc.symbol {
			t.Errorf("%s handled_by = %q, want %q", tc.key, got, tc.symbol)
		}
	}
}

// A `scope module:` contributes a controller namespace without changing the URL; a bare
// `scope '/path'` does the opposite. Conflating the two mis-resolves every handler
// underneath.
func TestRouteHandler_ScopeModuleVsPath(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  scope '/internal' do
    resources :jobs, only: [:index]
  end
  scope module: :admin do
    resources :users, only: [:index]
  end
end
`)
	if f := idx["GET /internal/jobs"]; f.Props["handler"] != "jobs#index" {
		t.Errorf("path-only scope leaked into the controller: %v", f.Props["handler"])
	}
	if f := idx["GET /users"]; f.Props["handler"] != "admin/users#index" {
		t.Errorf("module scope not applied: %v", f.Props["handler"])
	}
	if f := idx["GET /users"]; handledBy(f) != "Admin::UsersController#index" {
		t.Errorf("handled_by = %q", handledBy(idx["GET /users"]))
	}
}

// A bare verb inside a resources block names an action on that resource's controller.
// This is how most non-REST Rails endpoints are written.
func TestRouteHandler_MemberVerbUsesEnclosingController(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  resources :posts do
    member do
      post :publish
    end
    collection do
      get :drafts
    end
  end
end
`)
	if f, ok := idx["POST /posts/:id/publish"]; !ok {
		t.Fatalf("missing member route; have:\n  %s", routeKeys(idx))
	} else if f.Props["handler"] != "posts#publish" {
		t.Errorf("member handler = %v, want posts#publish", f.Props["handler"])
	}
	if f, ok := idx["GET /posts/drafts"]; !ok {
		t.Fatalf("missing collection route; have:\n  %s", routeKeys(idx))
	} else if f.Props["handler"] != "posts#drafts" {
		t.Errorf("collection handler = %v, want posts#drafts", f.Props["handler"])
	}
}

// An explicit `controller:` option and the `controller do` block form both override the
// name derived from the resource.
func TestRouteHandler_ExplicitControllerOverride(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  resources :posts, controller: 'articles', only: [:index]
  controller :photos do
    get 'search'
  end
end
`)
	if f := idx["GET /posts"]; f.Props["handler"] != "articles#index" {
		t.Errorf("controller: option ignored: %v", f.Props["handler"])
	}
	if f, ok := idx["GET /search"]; !ok {
		t.Fatalf("missing controller-block route; have:\n  %s", routeKeys(idx))
	} else if f.Props["handler"] != "photos#search" {
		t.Errorf("controller block handler = %v, want photos#search", f.Props["handler"])
	}
}

// Rails serves a singular `resource` from the controller named by
// ActiveSupport's pluralize, which knows the irregulars this extractor's
// inflector does not: `resource :person` is served by people, and the naive
// rule answers persons. Every singular resource is decided by that inflector,
// so none of them may be answered from it — with no explicit `controller:` the
// declaration claims no handler at all. `controller:` states the answer
// outright and is read whether or not the name inflects.
func TestRouteHandler_SingularResourceDeclinesWithoutExplicitController(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  resource :person, only: [:show]
  resource :profile, only: [:show]
  resource :session, only: [:show], controller: 'sessions'
end
`)
	for _, key := range []string{"GET /person", "GET /profile"} {
		f, ok := idx[key]
		if !ok {
			t.Fatalf("missing %q; have:\n  %s", key, routeKeys(idx))
		}
		if got, present := f.Props["handler"]; present {
			t.Errorf("%s handler = %v, want none: the controller is decided by an inflector this extractor does not have", key, got)
		}
		if got := handledBy(f); got != "" {
			t.Errorf("%s handled_by = %q, want none", key, got)
		}
	}
	if f := idx["GET /session"]; f.Props["handler"] != "sessions#show" {
		t.Errorf("explicit controller: ignored: %v", f.Props["handler"])
	}
}

// TestRouteConcerns_ReplayedPerReference: a `concern` block serves nothing where it is
// defined and everything where it is referenced. Emitting its routes at the definition
// site (which the previous default-case descent did) puts them at the wrong path and
// misses every real one.
func TestRouteConcerns_ReplayedPerReference(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  concern :commentable do
    resources :comments, only: [:index]
  end

  resources :posts, only: [:index], concerns: :commentable
  resources :photos, only: [:index] do
    concerns :commentable
  end
end
`)
	for _, want := range []string{
		"GET /posts/:post_id/comments",
		"GET /photos/:photo_id/comments",
	} {
		if _, ok := idx[want]; !ok {
			t.Errorf("missing concern route %q; have:\n  %s", want, routeKeys(idx))
		}
	}
	// Nothing at the definition site.
	if _, ok := idx["GET /comments"]; ok {
		t.Errorf("concern emitted routes at its definition site; have:\n  %s", routeKeys(idx))
	}
}

func TestControllerSymbol(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"posts#index", "PostsController#index"},
		{"api/v2/posts#show", "Api::V2::PostsController#show"},
		{"admin/user_sessions#create", "Admin::UserSessionsController#create"},
		// Already a class path.
		{"Api::V2::PostsController#index", "Api::V2::PostsController#index"},
		// Not a controller action: no action, a redirect, a Rack app.
		{"posts", ""},
		{"", ""},
		{"redirect('/x')#call", ""},
		{"Sidekiq::Web#call", ""},
	} {
		if got := controllerSymbol(tc.in); got != tc.want {
			t.Errorf("controllerSymbol(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseMountShapes(t *testing.T) {
	for _, tc := range []struct{ name, src, wantConst, wantAt string }{
		{"at keyword", "mount Spree::Core::Engine, at: '/shop'", "Spree::Core::Engine", "/shop"},
		{"hash rocket", "mount Sidekiq::Web => '/sidekiq'", "Sidekiq::Web", "/sidekiq"},
		{"no path defaults to root", "mount API::API", "API::API", "/"},
		{"rack app built inline", "mount Coverband::Reporters::Web.new, at: '/coverage'", "Coverband::Reporters::Web", "/coverage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "Rails.application.routes.draw do\n  " + tc.src + "\nend\n"
			ff := parseRouteFileAST([]byte(src), "config/routes.rb")
			if len(ff) != 1 {
				t.Fatalf("want 1 mount fact, got %d: %+v", len(ff), ff)
			}
			if got := ff[0].Props["mounts"]; got != tc.wantConst {
				t.Errorf("constant = %v, want %q", got, tc.wantConst)
			}
			wantName := strings.TrimSuffix(tc.wantAt, "/") + "/"
			if ff[0].Name != wantName {
				t.Errorf("name = %q, want %q", ff[0].Name, wantName)
			}
			if ff[0].Props["method"] != "MOUNT" {
				t.Errorf("method = %v, want MOUNT", ff[0].Props["method"])
			}
		})
	}
}

// A mount inside a scope is served below that scope's prefix.
func TestMountInsideScopeComposesPrefix(t *testing.T) {
	ff := parseRouteFileAST([]byte(`
Rails.application.routes.draw do
  namespace :admin do
    mount Sidekiq::Web, at: '/sidekiq'
  end
end
`), "config/routes.rb")
	if len(ff) != 1 {
		t.Fatalf("want 1 fact, got %d", len(ff))
	}
	if ff[0].Name != "/admin/sidekiq/" {
		t.Errorf("name = %q, want /admin/sidekiq/", ff[0].Name)
	}
}

// TestRoutesInsideControlFlow covers the shape that silently emptied five route files
// across the corpus: a route file is Ruby, and real ones guard their contents with
// conditionals. Walking only direct `call` children skipped every branch body, and a
// file that parses cleanly and yields nothing is indistinguishable from a file with no
// routes.
func TestRoutesInsideControlFlow(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      int
	}{
		{
			// GitLab guards whole route files this way.
			name: "unless at file scope",
			src:  "unless @organization_scoped_routes\n  resources :organizations, only: [:index]\n  get '/x' => 'y#z'\nend\n",
			want: 2,
		},
		{
			name: "if inside draw",
			src:  "Rails.application.routes.draw do\n  if Rails.env.development?\n    get '/letter_opener' => 'x#y'\n  end\nend\n",
			want: 1,
		},
		{
			// Rails' own activestorage/config/routes.rb closes the draw block with one.
			name: "if modifier wrapping the whole draw",
			src:  "Rails.application.routes.draw do\n  get '/blobs/:id' => 'blobs#show'\nend if ActiveStorage.draw_routes\n",
			want: 1,
		},
		{
			name: "else branch is walked too",
			src:  "Rails.application.routes.draw do\n  if x?\n    get '/a' => 'c#a'\n  else\n    get '/b' => 'c#b'\n  end\nend\n",
			want: 2,
		},
		{
			// solidus wraps its admin routes in a conditional inside a constraints block.
			name: "conditional nested under constraints and scope",
			src: "SolidusPromotions::Engine.routes.draw do\n  if SolidusSupport.admin_available?\n" +
				"    constraints(->(r) { true }) do\n      scope :admin do\n" +
				"        resources :promotions, only: [:index]\n      end\n    end\n  end\nend\n",
			want: 1,
		},
		{
			// The condition must NOT be dispatched as a route DSL call.
			name: "condition is not a route",
			src:  "if Rails.env.development?\n  nil\nend\n",
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ff := parseRouteFileAST([]byte(tc.src), "config/routes.rb")
			if len(ff) != tc.want {
				t.Errorf("got %d routes, want %d:\n  %s", len(ff), tc.want, routeKeys(routeIndex(t, tc.src)))
			}
		})
	}
}

// TestRouteHandler_HashRocketForm: `get 'path' => 'ctrl#action'` puts the handler in the
// pair's VALUE, not in a `to:` keyword. Discourse and lobsters write nearly every route
// this way, so reading only `to:` left thousands of routes with no handler.
func TestRouteHandler_HashRocketForm(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  get 'about' => 'static#about'
  post '/u/:username/preferences' => 'users/preferences#update'
  get 'legacy' => redirect('/new')
  get 'rack' => SomeRackApp
  get 'both', to: 'wins#here'
end
`)
	if f := idx["GET /about"]; f.Props["handler"] != "static#about" {
		t.Errorf("handler = %v, want static#about", f.Props["handler"])
	}
	if f := idx["GET /about"]; handledBy(f) != "StaticController#about" {
		t.Errorf("handled_by = %q", handledBy(f))
	}
	if f := idx["POST /u/:username/preferences"]; f.Props["handler"] != "users/preferences#update" {
		t.Errorf("handler = %v", f.Props["handler"])
	}
	// A redirect and a Rack app name no controller action; inventing one would create a
	// handled_by edge to a node that never exists.
	for _, k := range []string{"GET /legacy", "GET /rack"} {
		f, ok := idx[k]
		if !ok {
			t.Fatalf("missing %q; have:\n  %s", k, routeKeys(idx))
		}
		if h := f.Props["handler"]; h != nil {
			t.Errorf("%s got handler %v, want none", k, h)
		}
	}
	if f := idx["GET /both"]; f.Props["handler"] != "wins#here" {
		t.Errorf("explicit to: should win: %v", f.Props["handler"])
	}
}

// TestRouteDSL_StringArguments: Rails accepts a symbol or a string for `namespace`,
// `resources` and `resource`. Reading only the symbol form made a string namespace's
// whole block invisible — four openproject module route files declared 15 routes and
// produced none.
func TestRouteDSL_StringArguments(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  namespace "recaptcha" do
    get :settings, to: "admin#show"
    post :verify, to: "request#verify"
  end
  resources "widgets", only: [:index]
end
`)
	for _, want := range []string{
		"GET /recaptcha/settings",
		"POST /recaptcha/verify",
		"GET /widgets",
	} {
		if _, ok := idx[want]; !ok {
			t.Errorf("missing %q; have:\n  %s", want, routeKeys(idx))
		}
	}
}

// A plural `resources` takes its name verbatim — Rails' `Resource` sets
// `@controller = options[:controller] || @name` and does not inflect it. The
// name that tells the rule apart is an irregular plural: `resources :people` is
// served by people, and a channel that pluralizes here answers peoples. "posts"
// cannot tell them apart, because it pluralizes to itself.
func TestRouteHandler_PluralResourcesTakeTheNameVerbatim(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  resources :people, only: [:index]
  resources :meeting_self_schedule, only: [:index]
end
`)
	for _, tc := range []struct{ key, handler, symbol string }{
		{"GET /people", "people#index", "PeopleController#index"},
		{"GET /meeting_self_schedule", "meeting_self_schedule#index", ""},
	} {
		f, ok := idx[tc.key]
		if !ok {
			t.Fatalf("missing %q; have:\n  %s", tc.key, routeKeys(idx))
		}
		if got := f.Props["handler"]; got != tc.handler {
			t.Errorf("%s handler = %v, want %q", tc.key, got, tc.handler)
		}
		if tc.symbol != "" {
			if got := handledBy(f); got != tc.symbol {
				t.Errorf("%s handled_by = %q, want %q", tc.key, got, tc.symbol)
			}
		}
	}
}

// Rails composes the controller namespace where the ROUTE is created, not where the
// resource is declared, so a module scope opened in between belongs to the routes after
// it. Snapshotting the namespace onto the resource loses it.
func TestRouteHandler_ModuleComposesAtTheRouteSite(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  resources :reports, only: [] do
    scope module: :audit do
      get :history
    end
  end
end
`)
	f, ok := idx["GET /reports/:report_id/history"]
	if !ok {
		t.Fatalf("missing nested route; have:\n  %s", routeKeys(idx))
	}
	if got := f.Props["handler"]; got != "audit/reports#history" {
		t.Errorf("handler = %v, want audit/reports#history", got)
	}
}

// The `controller ... do` block form follows the same rule, so the two spellings of one
// route agree about who serves it. Composing at the block instead of at the route both
// loses a module opened inside the block and repeats the one already applied.
func TestRouteHandler_ControllerBlockComposesAtTheRouteSite(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  scope module: "ops" do
    controller "audit/exports" do
      get :daily
    end

    controller :reconciliations do
      scope module: :nightly do
        get :rollup
      end
    end
  end
end
`)
	for _, tc := range []struct{ key, handler string }{
		{"GET /daily", "ops/audit/exports#daily"},
		{"GET /rollup", "ops/nightly/reconciliations#rollup"},
	} {
		f, ok := idx[tc.key]
		if !ok {
			t.Fatalf("missing %q; have:\n  %s", tc.key, routeKeys(idx))
		}
		if got := f.Props["handler"]; got != tc.handler {
			t.Errorf("%s handler = %v, want %q", tc.key, got, tc.handler)
		}
	}
}

// `action:` is written both ways and the symbol form is the commoner one. Reading only
// the string form leaves the route with NO handler whenever the path is not itself a
// bare action name, which is exactly when the option is written.
func TestRouteHandler_ActionOptionInEitherSpelling(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  resources :invoices, only: [] do
    get "summary/:period", action: :summary, on: :collection
    get "ledger/:period", action: "ledger", on: :collection
  end
end
`)
	for _, tc := range []struct{ key, handler string }{
		{"GET /invoices/summary/:period", "invoices#summary"},
		{"GET /invoices/ledger/:period", "invoices#ledger"},
	} {
		f, ok := idx[tc.key]
		if !ok {
			t.Fatalf("missing %q; have:\n  %s", tc.key, routeKeys(idx))
		}
		if got := f.Props["handler"]; got != tc.handler {
			t.Errorf("%s handler = %v, want %q", tc.key, got, tc.handler)
		}
	}
}

// A verb may name its own controller, and Rails reads that name off the call
// before falling back to the scope: `map_match(..., controller: nil, ...)` then
// `controller ||= @scope[:controller]`. Reading `action:` while leaving
// `controller:` unread is worse than reading neither — the route resolves to
// whichever controller encloses it, which exists and does not serve it.
func TestRouteHandler_VerbLevelControllerOption(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  namespace :insights do
    resources :groups, only: [] do
      get :group_analytics, controller: "group_analytics/stage_types", action: :index, on: :collection
      get :retention, controller: :retention_reports, action: :show, on: :collection
    end
  end
end
`)
	for _, tc := range []struct{ key, handler string }{
		{"GET /insights/groups/group_analytics", "insights/group_analytics/stage_types#index"},
		{"GET /insights/groups/retention", "insights/retention_reports#show"},
	} {
		f, ok := idx[tc.key]
		if !ok {
			t.Fatalf("missing %q; have:\n  %s", tc.key, routeKeys(idx))
		}
		if got := f.Props["handler"]; got != tc.handler {
			t.Errorf("%s handler = %v, want %q", tc.key, got, tc.handler)
		}
	}
}

// add_controller_module's first branch: a controller written with a leading
// slash is stripped of the slash and NOT composed with the module
// (`if controller&.start_with?("/") then -controller[1..-1]`). Trimming the
// slash where the option is read erases the marker, and the route is then
// reported under a namespace Rails never applies.
func TestRouteHandler_LeadingSlashEscapesTheModule(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  namespace :api do
    resources :exports, only: [:index], controller: "/admin/exports"
    get :audit, controller: "/admin/audits", action: :index
    resources :jobs, only: [:index], controller: "admin/jobs"
  end
end
`)
	for _, tc := range []struct{ key, handler string }{
		{"GET /api/exports", "admin/exports#index"},
		{"GET /api/audit", "admin/audits#index"},
		{"GET /api/jobs", "api/admin/jobs#index"},
	} {
		f, ok := idx[tc.key]
		if !ok {
			t.Fatalf("missing %q; have:\n  %s", tc.key, routeKeys(idx))
		}
		if got := f.Props["handler"]; got != tc.handler {
			t.Errorf("%s handler = %v, want %q", tc.key, got, tc.handler)
		}
	}
}

// A singular resource whose controller could not be named must stop the search
// rather than let it walk outward to an enclosing resource. Rails serves
// /companies/:company_id/profile/settings from profiles#settings; answering
// companies#settings names a controller that exists and serves other routes,
// which is the one error a consumer cannot detect.
func TestRouteHandler_UndecidableControllerDoesNotInheritOuterResource(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  resources :companies, only: [] do
    resource :profile, only: [] do
      get :settings
    end
    resource :billing, only: [], controller: "billing_accounts" do
      get :invoices
    end
  end
end
`)
	f, ok := idx["GET /companies/:company_id/profile/settings"]
	if !ok {
		t.Fatalf("missing profile route; have:\n  %s", routeKeys(idx))
	}
	if got, present := f.Props["handler"]; present {
		t.Errorf("handler = %v, want none: companies does not serve this route", got)
	}
	b, ok := idx["GET /companies/:company_id/billing/invoices"]
	if !ok {
		t.Fatalf("missing billing route; have:\n  %s", routeKeys(idx))
	}
	if got := b.Props["handler"]; got != "billing_accounts#invoices" {
		t.Errorf("handler = %v, want billing_accounts#invoices", got)
	}
}

// `scope controller:` writes the same @scope[:controller] the `controller ... do`
// form does — merge_controller_scope keeps the child and discards the parent — and
// mapper.rb's `controller ||= @scope[:controller]` reads it. Leaving it unread does
// not leave the routes inside without a controller: the search walks outward to the
// enclosing resource and names one that exists and serves entirely different routes,
// which is the shape 26 of the monolith's routes were in.
func TestRouteHandler_ScopeControllerOption(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  namespace :assistant do
    scope controller: "copilot" do
      post :chart_block
    end
    scope controller: :diagnostics do
      get :ping
    end
    scope controller: "/toolbox" do
      get :diagnose
    end
    scope controller: "reporting" do
      scope module: "beta" do
        get :usage
      end
      resources :things, only: [:index]
      controller "innermost" do
        get :nested
      end
    end
  end
end
`)
	for _, tc := range []struct{ key, handler string }{
		{"POST /assistant/chart_block", "assistant/copilot#chart_block"},
		{"GET /assistant/ping", "assistant/diagnostics#ping"},
		{"GET /assistant/diagnose", "toolbox#diagnose"},
		{"GET /assistant/usage", "assistant/beta/reporting#usage"},
		{"GET /assistant/things", "assistant/things#index"},
		{"GET /assistant/nested", "assistant/innermost#nested"},
	} {
		f, ok := idx[tc.key]
		if !ok {
			t.Fatalf("missing %q; have:\n  %s", tc.key, routeKeys(idx))
		}
		if got := f.Props["handler"]; got != tc.handler {
			t.Errorf("%s handler = %v, want %q", tc.key, got, tc.handler)
		}
	}
}

// A `scope controller:` written with a value this extractor cannot read stops the
// search rather than falling through it. Rails serves /briefings/:briefing_id/digest
// from whatever the local names; briefings is not a second-best answer to that
// question but a different controller.
func TestRouteHandler_UnreadableScopeControllerDeclines(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  chosen = "copilot"
  resources :briefings, only: [] do
    scope controller: chosen do
      get :digest
    end
  end
end
`)
	f, ok := idx["GET /briefings/:briefing_id/digest"]
	if !ok {
		t.Fatalf("missing digest route; have:\n  %s", routeKeys(idx))
	}
	if got, present := f.Props["handler"]; present {
		t.Errorf("handler = %v, want none: briefings does not serve this route", got)
	}
}

// add_controller_module's first branch applies to the controller Rails splits out of
// a `to:` string exactly as it does to a `controller:` option: the slash is stripped
// and the name returned UNCOMPOSED. Honouring the marker for one spelling and not the
// other put every `to: "/x#y"` under a namespace Rails does not apply — and joined the
// module onto a name still carrying its slash, which no application has.
func TestRouteHandler_LeadingSlashEscapesTheModuleForTo(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  namespace :ledger do
    get "exports_by_hand", to: "/admin/exports#index"
    get "jobs_by_hand", to: "admin/jobs#index"
    get "rocket_by_hand" => "/admin/rockets#index"
    root to: "/admin/home#index"
  end
end
`)
	for _, tc := range []struct{ key, handler string }{
		{"GET /ledger/exports_by_hand", "admin/exports#index"},
		{"GET /ledger/jobs_by_hand", "ledger/admin/jobs#index"},
		{"GET /ledger/rocket_by_hand", "admin/rockets#index"},
		{"GET /ledger/", "admin/home#index"},
	} {
		f, ok := idx[tc.key]
		if !ok {
			t.Fatalf("missing %q; have:\n  %s", tc.key, routeKeys(idx))
		}
		if got := f.Props["handler"]; got != tc.handler {
			t.Errorf("%s handler = %v, want %q", tc.key, got, tc.handler)
		}
	}
}

// Ruby writes a hash key two ways and Rails reads both. The grammar gives a label key
// text with a trailing colon and a hash-rocket key text with a LEADING one, so trimming
// only the trailing colon matched `controller: "x"` and left `:controller => "x"`
// invisible — along with `:on => :collection`, which decides the path the route is
// served at, and every other option.
func TestRouteHandler_HashRocketOptionSpelling(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  resources :widgets, :only => [] do
    get :gadgets, :controller => "gizmos", :action => :list, :on => :collection
    get :audit, :action => "review", :on => :member
  end
  resources :gauges, :only => [:index], :controller => "meters"
  scope :module => "rocket" do
    get :five, :to => "inner#five"
  end
end
`)
	for _, tc := range []struct{ key, handler string }{
		{"GET /widgets/gadgets", "gizmos#list"},
		{"GET /widgets/:id/audit", "widgets#review"},
		{"GET /gauges", "meters#index"},
		{"GET /five", "rocket/inner#five"},
	} {
		f, ok := idx[tc.key]
		if !ok {
			t.Fatalf("missing %q; have:\n  %s", tc.key, routeKeys(idx))
		}
		if got := f.Props["handler"]; got != tc.handler {
			t.Errorf("%s handler = %v, want %q", tc.key, got, tc.handler)
		}
	}
}

// get_to_from_path: a String path of two or more plain segments that names no endpoint
// of its own IS the endpoint, and the name it derives is handed on as the `to:` — so it
// outranks the enclosing controller rather than deferring to it. Rails serves
// /shorthand/reports/monthly from reports#monthly; legacy serves neither that path nor
// an action called reports/monthly.
func TestRouteHandler_MatchShorthand(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  namespace :shorthand do
    controller :legacy do
      get "reports/monthly"
      get "single"
      get "audits/summary", action: :recap
      get "exports/nightly", to: "batches#run"
      get "with/param/:id", action: :fetch
    end
    get "my-billing/monthly-summary"
    get "a/b/c/d"
    get "fmt/seg(.:format)"
    scope module: "inner" do
      get "billing/invoices"
    end
  end
  resources :books, only: [] do
    get "chapters/list", on: :collection
  end
end
`)
	for _, tc := range []struct{ key, handler string }{
		{"GET /shorthand/reports/monthly", "shorthand/reports#monthly"},
		{"GET /shorthand/single", "shorthand/legacy#single"},
		{"GET /shorthand/audits/summary", "shorthand/legacy#recap"},
		{"GET /shorthand/exports/nightly", "shorthand/batches#run"},
		{"GET /shorthand/my-billing/monthly-summary", "shorthand/my_billing#monthly_summary"},
		{"GET /shorthand/a/b/c/d", "shorthand/a/b/c#d"},
		{"GET /shorthand/with/param/:id", "shorthand/legacy#fetch"},
		{"GET /shorthand/fmt/seg", "shorthand/fmt#seg"},
		{"GET /shorthand/billing/invoices", "shorthand/inner/billing#invoices"},
		{"GET /books/chapters/list", "chapters#list"},
	} {
		f, ok := idx[tc.key]
		if !ok {
			t.Fatalf("missing %q; have:\n  %s", tc.key, routeKeys(idx))
		}
		if got := f.Props["handler"]; got != tc.handler {
			t.Errorf("%s handler = %v, want %q", tc.key, got, tc.handler)
		}
	}
}

// The shorthand fires on the paths Rails fires it on and no others. A path that
// already names where it goes keeps that name, and a path Rails would refuse to
// derive an action from gets no handler here rather than a guessed one.
func TestRouteHandler_MatchShorthandDoesNotOverfire(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  get "widget/:name", to: redirect { |params, _req| "/w/#{params[:name]}" }
  namespace :quiet do
    get :plain
    get "one"
  end
end
`)
	if f, ok := idx["GET /quiet/one"]; !ok {
		t.Fatalf("missing single-segment route; have:\n  %s", routeKeys(idx))
	} else if got, present := f.Props["handler"]; present {
		t.Errorf("handler = %v, want none: one segment is not the shorthand", got)
	}
	f, ok := idx["GET /widget/:name"]
	if !ok {
		t.Fatalf("missing redirect route; have:\n  %s", routeKeys(idx))
	}
	if got, _ := f.Props["handler"].(string); strings.Contains(got, "#") {
		t.Errorf("handler = %v, want no controller action: a to: was given and the path carries a parameter", got)
	}
}

// `resources :widgets, module: :dashboards` is served by dashboards/widgets: Rails
// lifts the option into a scope around the declaration, so it reaches the
// resource's own routes and everything declared inside its block. Dropping it
// left 664 of a measured application's 1,435 resolvable handlers naming controllers
// that do not exist.
func TestRouteHandler_ResourceModuleOption(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  namespace :admin do
    resource :dashboard, only: :show do
      resources :widgets, only: [:show], module: :dashboards do
        post :refresh, on: :member
      end
    end
    resources :employees, only: [:index] do
      resources :notes, only: [:create], module: "employees"
    end
  end
end
`)
	for path, want := range map[string]string{
		"GET /admin/dashboard/widgets/:id":          "admin/dashboards/widgets#show",
		"POST /admin/dashboard/widgets/:id/refresh": "admin/dashboards/widgets#refresh",
		"POST /admin/employees/:employee_id/notes":  "admin/employees/notes#create",
		"GET /admin/employees":                      "admin/employees#index",
	} {
		f, ok := idx[path]
		if !ok {
			t.Errorf("%s not emitted; have %v", path, slices.Sorted(maps.Keys(idx)))
			continue
		}
		if f.Props["handler"] != want {
			t.Errorf("%s handler = %v, want %s", path, f.Props["handler"], want)
		}
	}
}

// Inside a member/collection scope Rails serves a canonical action declared by
// SYMBOL at the scope's own path (path_for_action / CANONICAL_ACTIONS):
// GET /orders/:order_id/review, not .../index.
// A string path or an explicit `path:` keeps its segment, and a non-canonical
// symbol keeps its own name.
func TestRoute_CanonicalActionInMethodScopeUsesScopePath(t *testing.T) {
	idx := routeIndex(t, `
Rails.application.routes.draw do
  resources :orders, only: [] do
    resources :review, only: [] do
      collection do
        get :index, to: 'reviews#show'
        put :update, to: 'reviews#update'
        get :notes, to: 'review_notes#show'
        get 'show', to: 'reviews#legacy'
        get :new, path: 'new', to: 'reviews#fresh'
      end
    end
  end
  resources :posts do
    get :show, on: :member, to: 'posts#preview'
    delete :destroy, on: :collection, to: 'posts#purge'
  end
end
`)
	for key, handler := range map[string]string{
		"GET /orders/:order_id/review":       "reviews#show",
		"PUT /orders/:order_id/review":       "reviews#update",
		"GET /orders/:order_id/review/notes": "review_notes#show",
		"GET /orders/:order_id/review/show":  "reviews#legacy",
		"GET /orders/:order_id/review/new":   "reviews#fresh",
		"GET /posts/:id":                     "posts#preview",
		"DELETE /posts":                      "posts#purge",
	} {
		f, ok := idx[key]
		if !ok {
			t.Errorf("missing %s; have:\n  %s", key, routeKeys(idx))
			continue
		}
		if f.Props["handler"] != handler {
			t.Errorf("%s handler = %v, want %s", key, f.Props["handler"], handler)
		}
	}
	for _, key := range []string{
		"GET /orders/:order_id/review/index",
		"PUT /orders/:order_id/review/update",
	} {
		if _, ok := idx[key]; ok {
			t.Errorf("%s emitted; Rails serves it at the collection path", key)
		}
	}
}
