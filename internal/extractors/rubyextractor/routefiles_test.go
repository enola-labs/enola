package rubyextractor

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestIsRouteFile_EngineAndPluginPaths pins the shape rule: a Rails route file is any
// <dir>/config/routes.rb or any .rb below a config/routes/ directory, at any depth.
//
// The previous rule matched only the repository root plus a packwerk `packages/*`
// pattern, which is how solidus (six engines, no root config/) reported ZERO Rails
// routes while declaring 195, discourse's 25 plugin route files went unread, and
// GitLab's 38 ee/config/routes files were skipped.
func TestIsRouteFile_EngineAndPluginPaths(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		// The application's own.
		{"config/routes.rb", true},
		{"config/routes/api.rb", true},
		{"config/routes/directs/promo.rb", true},
		// Engine and plugin route files — the whole point of the change.
		{"api/config/routes.rb", true},
		{"backend/config/routes.rb", true},
		{"plugins/chat/config/routes.rb", true},
		{"ee/config/routes.rb", true},
		{"ee/config/routes/project.rb", true},
		{"packages/billing/config/routes/web.rb", true},
		// Not route files.
		{"config/application.rb", false},
		{"app/models/route.rb", false},
		{"routes.rb", false},
		{"lib/routes/helper.rb", false},
		{"config/routes.yml", false},
		// Generator templates and dummy apps LOOK like route files and are served by
		// nobody; solidus ships one of each.
		{"core/lib/generators/spree/dummy/templates/rails/routes.rb", false},
		{"core/lib/spree/testing_support/dummy_app/routes.rb", false},
		{"spec/dummy_app/config/routes.rb", false},
	} {
		if got := isRouteFile(tc.path); got != tc.want {
			t.Errorf("isRouteFile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestIndexRouteFiles_Classification checks that the index separates the application
// root from engine roots and draw targets, and that a draw key is the path tail below
// config/routes/ so that the CE and EE copy of a name both answer to one `draw`.
func TestIndexRouteFiles_Classification(t *testing.T) {
	idx := indexRouteFiles([]string{
		"config/routes.rb",
		"config/routes/project.rb",
		"config/routes/directs/promo.rb",
		"ee/config/routes/project.rb",
		"api/config/routes.rb",
		"plugins/chat/config/routes.rb",
		"app/models/user.rb",
	})

	if idx.appRoot != "config/routes.rb" {
		t.Errorf("appRoot = %q, want config/routes.rb", idx.appRoot)
	}
	if got := idx.engineRoots["api"]; got != "api/config/routes.rb" {
		t.Errorf("engineRoots[api] = %q", got)
	}
	if got := idx.engineRoots["plugins/chat"]; got != "plugins/chat/config/routes.rb" {
		t.Errorf("engineRoots[plugins/chat] = %q", got)
	}
	// One draw key, two files: GitLab's `draw` override loads both.
	got := append([]string{}, idx.drawTargets["project"]...)
	sort.Strings(got)
	want := []string{"config/routes/project.rb", "ee/config/routes/project.rb"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("drawTargets[project] = %v, want %v", got, want)
	}
	if got := idx.drawTargets["directs/promo"]; len(got) != 1 {
		t.Errorf("nested draw key not indexed: %v", got)
	}
	if len(idx.all) != 6 {
		t.Errorf("all = %d files, want 6", len(idx.all))
	}
}

func TestEngineClassNames(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{
			name: "nested modules",
			src:  "module Spree\n  module Core\n    class Engine < ::Rails::Engine\n      isolate_namespace Spree\n    end\n  end\nend\n",
			want: []string{"Spree::Core::Engine"},
		},
		{
			name: "single module, unqualified superclass",
			src:  "module DiscourseAi\n  class Engine < Rails::Engine\n  end\nend\n",
			want: []string{"DiscourseAi::Engine"},
		},
		{
			name: "a plain class is not an engine",
			src:  "module Foo\n  class Bar < Base\n  end\nend\n",
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := engineClassNames([]byte(tc.src))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("engineClassNames = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestExtractAllRoutes_MountedEngineGetsMountPrefix is the solidus case end to end: an
// engine-only repository with no root config/routes.rb, whose engine is mounted from a
// parent route file, must serve that engine's routes below the mount path.
func TestExtractAllRoutes_MountedEngineGetsMountPrefix(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("config/routes.rb", "Rails.application.routes.draw do\n"+
		"  mount Spree::Core::Engine, at: '/shop'\n"+
		"  mount Sidekiq::Web => '/sidekiq'\n"+
		"end\n")
	write("core/lib/spree/core/engine.rb", "module Spree\n  module Core\n    class Engine < ::Rails::Engine\n    end\n  end\nend\n")
	write("core/config/routes.rb", "Spree::Core::Engine.routes.draw do\n"+
		"  resources :products, only: [:index, :show]\n"+
		"end\n")

	files := []string{
		"config/routes.rb",
		"core/lib/spree/core/engine.rb",
		"core/config/routes.rb",
	}
	got, _ := extractAllRoutes(repo, files)

	byName := map[string]facts.Fact{}
	for _, f := range got {
		// extractAllRoutes also returns the route-macro coverage fact, which is an
		// extraction fact and carries no method.
		if f.Kind != facts.KindRoute {
			continue
		}
		key := f.Props["method"].(string) + " " + f.Name
		byName[key] = f
	}
	for _, want := range []string{
		"GET /shop/products",
		"GET /shop/products/:id",
		"MOUNT /shop/",
		"MOUNT /sidekiq/",
	} {
		if _, ok := byName[want]; !ok {
			t.Errorf("missing route %q; got %v", want, sortedRouteKeys(byName))
		}
	}
	// The engine route file must NOT also be emitted un-prefixed by the catch-all pass.
	if _, ok := byName["GET /products"]; ok {
		t.Errorf("engine routes emitted twice — once mounted, once bare: %v", sortedRouteKeys(byName))
	}
	// Sidekiq::Web resolves to no engine directory in this repo; the mount is still
	// recorded, and nothing is invented for it.
	if f := byName["MOUNT /sidekiq/"]; f.Props["mounts"] != "Sidekiq::Web" {
		t.Errorf("mount constant not recorded: %#v", f.Props)
	}
}

// TestExtractAllRoutes_UnmountedEngineStillContributes covers pass 4: a route file that
// no mount or draw in the snapshot names is still parsed, at the root prefix. Discourse
// mounts its plugin engines from the plugin loader, not from config/routes.rb, so
// dropping unreferenced files would drop 25 real route files.
func TestExtractAllRoutes_UnmountedEngineStillContributes(t *testing.T) {
	repo := t.TempDir()
	p := filepath.Join(repo, "plugins", "chat", "config")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "routes.rb"),
		[]byte("Chat::Engine.routes.draw do\n  get '/messages' => 'chat/messages#index'\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var routes []facts.Fact
	extracted, _ := extractAllRoutes(repo, []string{"plugins/chat/config/routes.rb"})
	for _, f := range extracted {
		// The route-macro coverage fact rides along and is not a route.
		if f.Kind == facts.KindRoute {
			routes = append(routes, f)
		}
	}
	if len(routes) != 1 {
		t.Fatalf("want 1 route, got %d: %+v", len(routes), routes)
	}
	if routes[0].Name != "/messages" {
		t.Errorf("name = %q, want /messages", routes[0].Name)
	}
}

func sortedRouteKeys(m map[string]facts.Fact) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestDetectRailsProject_EngineOnlyRepo covers solidus's shape: no root config/, no
// bin/rails, but engines that are Rails by construction.
func TestDetectRailsProject_EngineOnlyRepo(t *testing.T) {
	repo := t.TempDir()
	if detectRailsProject(repo) {
		t.Fatal("an empty dir is not a Rails project")
	}
	mk := func(rel string) {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A gem with routes but no engine class is not enough on its own.
	mk("core/config/routes.rb")
	if detectRailsProject(repo) {
		t.Error("config/routes.rb alone should not identify a Rails engine")
	}
	mk("core/lib/spree/core/engine.rb")
	if !detectRailsProject(repo) {
		t.Error("config/routes.rb + lib/**/engine.rb is a Rails engine")
	}
}

// A plugin's engine is mounted by the application from a file no route table reaches:
// the plugin's own config/routes.rb drawing into the application's routes, or its
// plugin.rb appending to them. Its routes are served under that mount, not at the
// root, where two plugins' /vote read as one endpoint registered twice. An engine
// the application mounts at two paths keeps its routes bare.
func TestExtractAllRoutes_ApplicationMountsAPluginEngine(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("config/routes.rb", "Discourse::Application.routes.draw do\n  get '/about' => 'about#index'\nend\n")
	// Mounted from plugin.rb.
	write("plugins/poll/plugin.rb", "after_initialize do\n"+
		"  Discourse::Application.routes.append { mount ::DiscoursePoll::Engine, at: \"/polls\" }\n"+
		"end\n")
	write("plugins/poll/config/routes.rb", "DiscoursePoll::Engine.routes.draw do\n  put \"/vote\" => \"polls#vote\"\nend\n")
	// Mounted from its own route file, below the engine's draw.
	write("plugins/assign/config/routes.rb", "DiscourseAssign::Engine.routes.draw do\n  put \"/claim/:topic_id\" => \"assign#claim\"\nend\n\n"+
		"Discourse::Application.routes.draw do\n  mount ::DiscourseAssign::Engine, at: \"/assign\"\nend\n")
	// Mounted twice: which path a route is served at is not one answer.
	write("plugins/votes/plugin.rb", "Discourse::Application.routes.append do\n"+
		"  mount ::DiscourseVotes::Engine, at: \"/voting\"\n  mount ::DiscourseVotes::Engine, at: \"/votes\"\nend\n")
	write("plugins/votes/config/routes.rb", "DiscourseVotes::Engine.routes.draw do\n  post \"/vote\" => \"votes#vote\"\nend\n")

	files := []string{"config/routes.rb", "plugins/poll/plugin.rb", "plugins/poll/config/routes.rb",
		"plugins/assign/config/routes.rb", "plugins/votes/plugin.rb", "plugins/votes/config/routes.rb"}
	got, _ := extractAllRoutes(repo, files)
	byName := map[string]bool{}
	for _, f := range got {
		if f.Kind == facts.KindRoute {
			byName[f.Props["method"].(string)+" "+f.Name] = true
		}
	}
	for _, want := range []string{"GET /about", "PUT /polls/vote", "PUT /assign/claim/:topic_id", "MOUNT /assign/", "POST /vote"} {
		if !byName[want] {
			t.Errorf("missing %q; got %v", want, byName)
		}
	}
	for _, not := range []string{"PUT /vote", "PUT /claim/:topic_id"} {
		if byName[not] {
			t.Errorf("%q is served under its engine's mount, not at the root", not)
		}
	}
}

// An application's own helper naming a route file by symbol draws it in the scope
// it is called from, and the files that file draws follow; a helper naming no route
// file draws nothing.
func TestExtractAllRoutes_ApplicationHelperDrawsARouteFile(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("config/routes.rb", "Rails.application.routes.draw do\n"+
		"  scope(path: '*namespace_id/:project_id') do\n    draw_all :project\n  end\n"+
		"  draw_all :nothing_here\nend\n")
	write("config/routes/project.rb", "scope '-' do\n  get '/issues' => 'issues#index'\n  draw :repository\nend\n")
	write("config/routes/repository.rb", "get '/refs/switch' => 'refs#switch'\n")
	files := []string{"config/routes.rb", "config/routes/project.rb", "config/routes/repository.rb"}
	got, _ := extractAllRoutes(repo, files)
	byName := map[string]bool{}
	for _, f := range got {
		if f.Kind == facts.KindRoute {
			byName[f.Props["method"].(string)+" "+f.Name] = true
		}
	}
	for _, want := range []string{"GET /*namespace_id/:project_id/-/issues", "GET /*namespace_id/:project_id/-/refs/switch"} {
		if !byName[want] {
			t.Errorf("missing %q; got %v", want, byName)
		}
	}
	for _, not := range []string{"GET /-/issues", "GET /refs/switch"} {
		if byName[not] {
			t.Errorf("%q is served under the helper's scope, not at the root", not)
		}
	}
}

// A route table written in a method of the route file is declared where the method
// is called, under each caller's scope; a method never called declares nothing.
func TestParseRouteFile_MethodIsDeclaredWhereItIsCalled(t *testing.T) {
	src := []byte(`Rails.application.routes.draw do
  def draw_all_routes
    get '/help' => 'help#index'
  end
  def unused_routes
    get '/never' => 'never#index'
  end
  draw_all_routes
  scope(path: '/o/:organization_path') do
    draw_all_routes
  end
end
`)
	ff, _ := parseRouteFile(src, "config/routes.rb", "", "", jsonapiContext{}, nil)
	got := map[string]bool{}
	for _, f := range ff {
		got[f.Name] = true
	}
	if !got["/help"] || !got["/o/:organization_path/help"] || got["/never"] || len(got) != 2 {
		t.Errorf("routes = %v, want /help and /o/:organization_path/help", got)
	}
}

// A file drawn from two scopes is served under both. A route-file method replayed
// under a further scope declares its own routes there, but draws no file again.
func TestExtractAllRoutes_FileDrawnFromTwoScopes(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("config/routes.rb", "Rails.application.routes.draw do\n"+
		"  def all_routes\n    scope '/-/secondary' do\n      draw :git_http\n    end\n    draw :git_http\n    get '/help' => 'help#index'\n  end\n"+
		"  all_routes\n  scope '/o/:org' do\n    all_routes\n  end\nend\n")
	write("config/routes/git_http.rb", "get '/info/refs' => 'git#refs'\n")
	got, _ := extractAllRoutes(repo, []string{"config/routes.rb", "config/routes/git_http.rb"})
	byName := map[string]bool{}
	for _, f := range got {
		if f.Kind == facts.KindRoute {
			byName[f.Props["method"].(string)+" "+f.Name] = true
		}
	}
	want := []string{"GET /info/refs", "GET /-/secondary/info/refs", "GET /help", "GET /o/:org/help"}
	for _, w := range want {
		if !byName[w] {
			t.Errorf("missing %q; got %v", w, byName)
		}
	}
	if byName["GET /o/:org/info/refs"] || len(byName) != len(want) {
		t.Errorf("routes = %v, want exactly %v", byName, want)
	}
}
