package rubyextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A draw inside a namespace serves the drawn file's routes from that namespace's
// controllers; a verb inside a singular resource is served by the resource's
// pluralized controller when that controller is declared, and by nothing otherwise.
func TestRailsRoutes_DrawModuleAndSingularVerbs(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Gemfile":               "gem 'rails'\n",
		"config/application.rb": "module App\n  class Application < Rails::Application\n  end\nend\n",
		"config/routes.rb": `Rails.application.routes.draw do
  draw(:admin)
end
`,
		"config/routes/admin.rb": `namespace :admin do
  draw(:billing)
end
`,
		"config/routes/billing.rb": `namespace :payments do
  resources :refunds, only: %i(index) do
    collection do
      post :confirm
    end
  end
end

resource :phone_verifications, only: [] do
  post :reset
  get :state, on: :member
end

resource :gadget, only: [] do
  post :spin
end
`,
		"app/controllers/admin/payments/refunds_controller.rb":    "class Admin::Payments::RefundsController\n  def index; end\n  def confirm; end\nend\n",
		"app/controllers/admin/phone_verifications_controller.rb": "class Admin::PhoneVerificationsController\n  def reset; end\n  def state; end\nend\n",
	})
	handled := map[string]string{}
	for _, f := range ff {
		if f.Kind != facts.KindRoute || f.PropString("framework") != "rails" {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				handled[f.PropString("method")+" "+f.Name] = r.Target
			}
		}
		if _, ok := handled[f.PropString("method")+" "+f.Name]; !ok {
			handled[f.PropString("method")+" "+f.Name] = ""
		}
	}
	for route, want := range map[string]string{
		"GET /admin/payments/refunds":           "Admin::Payments::RefundsController#index",
		"POST /admin/payments/refunds/confirm":  "Admin::Payments::RefundsController#confirm",
		"POST /admin/phone_verifications/reset": "Admin::PhoneVerificationsController#reset",
		"GET /admin/phone_verifications/state":  "Admin::PhoneVerificationsController#state",
		"POST /admin/gadget/spin":               "",
	} {
		got, ok := handled[route]
		if !ok {
			t.Errorf("no route %s (have %v)", route, handled)
			continue
		}
		if got != want {
			t.Errorf("%s handled_by %q, want %q", route, got, want)
		}
	}
}
