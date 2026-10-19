package rubyextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A gemspec at the root publishes every module of the repository; one under
// engines/ is a gem the repository consumes itself and publishes nothing.
func TestPublishedFollowsRootGemspec(t *testing.T) {
	gem := extractRepo(t, map[string]string{
		"acme.gemspec":       "Gem::Specification.new { |s| s.name = 'acme' }\n",
		"lib/acme/client.rb": "module Acme\n  class Client\n  end\nend\n",
	})
	app := extractRepo(t, map[string]string{
		"engines/billing/billing.gemspec":       "Gem::Specification.new { |s| s.name = 'billing' }\n",
		"engines/billing/lib/billing/charge.rb": "module Billing\n  class Charge\n  end\nend\n",
		"app/models/order.rb":                   "class Order\nend\n",
	})
	for _, c := range []struct {
		ff     []facts.Fact
		module string
		want   bool
	}{
		{gem, "lib/acme", true},
		{app, "engines/billing/lib/billing", false},
		{app, "app/models", false},
	} {
		found := false
		for _, f := range c.ff {
			if f.Kind == facts.KindModule && f.Name == c.module {
				found = true
				if got := f.PropBool(facts.PropPublished); got != c.want {
					t.Errorf("%s: published = %v, want %v", c.module, got, c.want)
				}
			}
		}
		if !found {
			t.Errorf("no module %s", c.module)
		}
	}
}
