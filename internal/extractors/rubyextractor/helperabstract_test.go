package rubyextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A view helper defines instance methods like a mixin, but Rails includes it in
// views on its own: it is concrete for abstractness. A mixin elsewhere stays
// abstract.
func TestRailsViewHelperIsNotAnAbstraction(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Gemfile":               "gem 'rails'\n",
		"config/application.rb": "module App\n  class Application < Rails::Application\n  end\nend\n",
		"packs/billing/app/helpers/billing/invoice_helper.rb": "module Billing\n  module InvoiceHelper\n    def total_label(i)\n      i.to_s\n    end\n  end\nend\n",
		"app/models/concerns/priced.rb":                       "module Priced\n  def price_label\n    price.to_s\n  end\nend\n",
	})
	got := map[string]any{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			got[f.Name] = f.PropAny("abstract")
		}
	}
	if got["Billing::InvoiceHelper"] != false {
		t.Errorf("view helper abstract = %v, want false", got["Billing::InvoiceHelper"])
	}
	if got["Priced"] != true {
		t.Errorf("mixin abstract = %v, want true", got["Priced"])
	}
}
