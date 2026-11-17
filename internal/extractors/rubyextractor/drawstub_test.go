package rubyextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// draw(:billing) delegates to config/routes/billing.rb. Once that file is parsed
// its routes are the delegation's surface and the placeholder goes; a draw whose
// file is missing keeps its placeholder.
func TestDrawPlaceholderGoesOnceTheDrawnFileIsParsed(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Gemfile":                                "gem 'rails'\n",
		"config/application.rb":                  "module App\n  class Application < Rails::Application\n  end\nend\n",
		"app/controllers/invoices_controller.rb": "class InvoicesController < ApplicationController\n  def index; end\nend\n",
		"config/routes.rb": `Rails.application.routes.draw do
  scope "/api/v2" do
    draw(:billing)
    draw(:missing)
  end
end
`,
		"config/routes/billing.rb": `resources :invoices, only: [:index]
`,
	})
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute {
			got[f.Name] = f.PropString("method")
		}
	}
	if got["/api/v2/invoices"] != "GET" {
		t.Errorf("the drawn file's route is missing: %v", got)
	}
	if _, ok := got["/api/v2/billing"]; ok {
		t.Errorf("the resolved draw kept its placeholder: %v", got)
	}
	if got["/api/v2/missing"] != "DRAW" {
		t.Errorf("an unresolved draw lost its placeholder: %v", got)
	}
}
