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
