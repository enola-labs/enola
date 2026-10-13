package rubyextractor

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// boundRoutes extracts a repository written from files and returns
// "<METHOD> <path>" -> the route's handled_by target, "" when it has none.
func boundRoutes(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	repo := t.TempDir()
	rels := make([]string, 0, len(files))
	for rel, content := range files {
		writeFile(t, repo, rel, content)
		rels = append(rels, rel)
	}
	ff, err := New().Extract(context.Background(), repo, rels)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range ff {
		if f.Kind != facts.KindRoute {
			continue
		}
		if _, leaked := f.Props[singularCandidateProp]; leaked {
			t.Errorf("%s carries %s", f.Name, singularCandidateProp)
		}
		m, _ := f.Props["method"].(string)
		out[m+" "+f.Name] = handledBy(f)
	}
	return out
}

func wantBound(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	for key, target := range want {
		have, ok := got[key]
		if !ok {
			t.Errorf("no route %q; have %v", key, got)
			continue
		}
		if have != target {
			t.Errorf("%s handled_by = %q, want %q", key, have, target)
		}
	}
}

// An engine that isolates a namespace serves its controllers from inside it, whichever
// engine of the repository the route file draws on.
func TestRouteBinding_EngineIsolatedNamespace(t *testing.T) {
	got := boundRoutes(t, map[string]string{
		"core/lib/shop/core/engine.rb": `
module Shop
  module Core
    class Engine < ::Rails::Engine
      isolate_namespace Shop
    end
  end
end
`,
		"api/lib/shop/api/engine.rb": `
module Shop
  module Api
    class Engine < Rails::Engine
      isolate_namespace Shop
    end
  end
end
`,
		"api/config/routes.rb": `
Shop::Core::Engine.routes.draw do
  namespace :api do
    resources :orders, only: [:create]
  end
end
`,
		"api/app/controllers/shop/api/orders_controller.rb": `
module Shop
  module Api
    class OrdersController < ActionController::API
      def create; end
    end
  end
end
`,
	})
	wantBound(t, got, map[string]string{
		"POST /api/orders": "Shop::Api::OrdersController#create",
	})
}

// A relation names a symbol the repository declares, or it is not emitted.
func TestRouteBinding_UndeclaredHandlerIsNotBound(t *testing.T) {
	got := boundRoutes(t, map[string]string{
		"config/application.rb": "",
		"config/routes.rb": `
Rails.application.routes.draw do
  resources :posts, only: [:index, :show]
  resources :ghosts, only: [:index]
  mount Sidekiq::Web => "/sidekiq"
end
`,
		"app/controllers/posts_controller.rb": `
class PostsController < ApplicationController
  def index; end
end
`,
		"app/controllers/application_controller.rb": `
class ApplicationController < ActionController::Base
end
`,
	})
	wantBound(t, got, map[string]string{
		"GET /posts":      "PostsController#index",
		"GET /posts/:id":  "",
		"GET /ghosts":     "",
		"MOUNT /sidekiq/": "",
	})
}

// An inherited action is declared on an ancestor, and that method serves the route.
func TestRouteBinding_InheritedAction(t *testing.T) {
	got := boundRoutes(t, map[string]string{
		"config/application.rb": "",
		"config/routes.rb": `
Rails.application.routes.draw do
  namespace :admin do
    resources :zones, only: [:index, :update]
  end
end
`,
		"app/controllers/admin/resource_controller.rb": `
module Admin
  class ResourceController < ApplicationController
    def index; end
    def update; end
  end
end
`,
		"app/controllers/admin/zones_controller.rb": `
module Admin
  class ZonesController < ResourceController
    def index; end
  end
end
`,
	})
	wantBound(t, got, map[string]string{
		"GET /admin/zones":     "Admin::ZonesController#index",
		"PUT /admin/zones/:id": "Admin::ResourceController#update",
	})
}

// A name the application inflects as an acronym is found by the one class declared
// under it in another casing, and by none when two classes differ only in casing.
func TestRouteBinding_AcronymNamespace(t *testing.T) {
	routes := `
Rails.application.routes.draw do
  namespace :activitypub do
    resources :outboxes, only: [:show]
  end
end
`
	controller := `
class ActivityPub::OutboxesController < ApplicationController
  def show; end
end
`
	got := boundRoutes(t, map[string]string{
		"config/application.rb": "",
		"config/routes.rb":      routes,
		"app/controllers/activitypub/outboxes_controller.rb": controller,
	})
	wantBound(t, got, map[string]string{
		"GET /activitypub/outboxes/:id": "ActivityPub::OutboxesController#show",
	})

	got = boundRoutes(t, map[string]string{
		"config/application.rb": "",
		"config/routes.rb":      routes,
		"app/controllers/activitypub/outboxes_controller.rb": controller,
		"lib/legacy/outboxes_controller.rb": `
class ActivityPUB::OutboxesController
  def show; end
end
`,
	})
	wantBound(t, got, map[string]string{"GET /activitypub/outboxes/:id": ""})
}

// A singular resource names its controller through a pluralization this extractor can
// only guess. The guess is kept when the controller it names is declared.
func TestRouteBinding_SingularResourceBoundWhenDeclared(t *testing.T) {
	got := boundRoutes(t, map[string]string{
		"config/application.rb": "",
		"config/routes.rb": `
Rails.application.routes.draw do
  resource :inbox, only: [:create], module: :activitypub
  resource :person, only: [:show]
end
`,
		"app/controllers/activitypub/inboxes_controller.rb": `
class ActivityPub::InboxesController < ApplicationController
  def create; end
end
`,
		"app/controllers/people_controller.rb": `
class PeopleController < ApplicationController
  def show; end
end
`,
	})
	wantBound(t, got, map[string]string{
		"POST /inbox": "ActivityPub::InboxesController#create",
		"GET /person": "",
	})
}
