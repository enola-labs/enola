package rubyextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// extractRepo writes files into a temp repository and runs the full extractor,
// so constant resolution sees the class symbols the AST pass really emits.
func extractRepo(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	repo := t.TempDir()
	var rels []string
	for rel, body := range files {
		abs := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	ff, err := New().Extract(context.Background(), repo, rels)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

// clientRouteSet renders client routes as sorted "VERB path @file hint derived".
func clientRouteSet(ff []facts.Fact) []string {
	var out []string
	for _, f := range clientRoutes(ff) {
		derived, _ := f.Props["derived"].(string)
		out = append(out, strings.TrimSpace(strings.Join([]string{
			f.Props["method"].(string), f.Name, "@" + f.File, f.Props["target_hint"].(string), derived,
		}, " ")))
	}
	sort.Strings(out)
	return out
}

func assertRoutes(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("client routes:\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
}

// A module-level accessor (`Billing.client`) instead of a `*Client` constant, a literal path, and a call whose argument list opens at
// the end of the line.
func TestRubyHTTPClient_ConstantAccessorReceiver(t *testing.T) {
	src := `class Billing::Accounts::FetchBalance
  def response
    @response ||= Billing.client.post('/api/v1/accounts/balance', request_body)
  end

  def available
    Billing.client.get(
      '/api/v1/accounts/limits',
      { account_code: account_code },
    )
  end
end
`
	got := clientRoutes(extractRubyHTTPClientFacts([]byte(src), "app/providers/billing/fetch_balance.rb"))
	if len(got) != 2 {
		t.Fatalf("got %d client routes, want 2: %+v", len(got), got)
	}
	want := map[string]string{
		"/api/v1/accounts/balance": "POST",
		"/api/v1/accounts/limits":  "GET",
	}
	for _, f := range got {
		if want[f.Name] != f.Props["method"] {
			t.Errorf("%s method = %v, want %s", f.Name, f.Props["method"], want[f.Name])
		}
		if f.Props["target_hint"] != "billing" {
			t.Errorf("%s target_hint = %v, want billing", f.Name, f.Props["target_hint"])
		}
	}
	if got[1].Line != 7 {
		t.Errorf("multi-line call located at line %d, want the call's line 7", got[1].Line)
	}
}

// An accessor read off a variable is a model attribute, not a client.
func TestRubyHTTPClient_VariableAccessorIgnored(t *testing.T) {
	src := `def sync
  user.client.get('profile/settings')
  record.client.post("orders/new")
end
`
	if got := clientRoutes(extractRubyHTTPClientFacts([]byte(src), "app/models/user.rb")); len(got) != 0 {
		t.Errorf("got %d client routes from variable accessors, want 0: %+v", len(got), got)
	}
}

// One HTTP call in a base class, one PATH per subclass in its own file — the
// template-method shape of a provider hierarchy. Each subclass's
// path is a route, located in the subclass that owns it; a subclass that
// overrides the call with a bare PATH resolves through its own class.
func TestResolveRubyClientConstants_TemplateMethod(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/providers/billing/read_provider.rb": `class Billing::ReadProvider
  def run
    Billing.client.post(self.class::PATH, payload)
  end
end
`,
		"app/providers/billing/products/fetch_details.rb": `class Billing::Products::FetchDetails < Billing::ReadProvider
  PATH = '/api/v1/products/details'
  RESPONSE_KEY = 'items'
end
`,
		"app/providers/billing/regions/fetch_by_code.rb": `class Billing::Regions::FetchByCode < Billing::ReadProvider
  PATH = "/api/v1/regions/by_code".freeze
end
`,
		// Two levels down, written relative to its namespace.
		"app/providers/billing/payments/method_restriction.rb": `module Billing
  module Payments
    class MethodRestriction < ReadProvider
      PATH = '/api/v1/payment_methods/restricted'
    end
  end
end
`,
		"app/providers/billing/orders/ensure_order.rb": `class Billing::Orders::EnsureOrder
  PATH = '/api/v1/orders/ensure'

  def run
    response = Billing.client.post(PATH, command_payload, headers)
  end
end
`,
		// Abstract: no PATH anywhere below it.
		"app/providers/billing/abstract_command.rb": `class Billing::AbstractCommand
  def run
    Billing.client.post(self.class::PATH, payload)
  end
end
`,
	})
	// MethodRestriction writes `< ReadProvider` inside `module Billing`; Ruby
	// resolves that lexically to Billing::ReadProvider, and so must the resolver.
	assertRoutes(t, clientRouteSet(ff), []string{
		"POST /api/v1/orders/ensure @app/providers/billing/orders/ensure_order.rb billing constant",
		"POST /api/v1/payment_methods/restricted @app/providers/billing/payments/method_restriction.rb billing inherited-constant",
		"POST /api/v1/products/details @app/providers/billing/products/fetch_details.rb billing inherited-constant",
		"POST /api/v1/regions/by_code @app/providers/billing/regions/fetch_by_code.rb billing inherited-constant",
	})

	var cov facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindExtraction && f.Name == "ruby:http-client-constants" {
			cov = f
		}
	}
	if cov.Name == "" {
		t.Fatal("no ruby:http-client-constants coverage fact")
	}
	if s, _ := cov.Props["unresolved_macros"].(string); s != "constant-unresolved=1" {
		t.Errorf("unresolved = %q, want constant-unresolved=1 (the abstract command)", s)
	}
}

// A subclass that inherits PATH without assigning it adds no route of its own,
// and an explicitly scoped constant reads that class (or its ancestors).
func TestResolveRubyClientConstants_ScopedAndInherited(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"lib/paths.rb": `class Paths
  BALANCE = '/api/v1/accounts/balance'
end
`,
		"lib/base.rb": `class Base
  PATH = '/api/v2/things'

  def run
    Billing.client.get(self.class::PATH)
  end
end
`,
		"lib/child.rb": `class Child < Base
end
`,
		"lib/scoped.rb": `class Scoped
  def run
    Billing.client.post(Paths::BALANCE, body)
  end
end
`,
	})
	assertRoutes(t, clientRouteSet(ff), []string{
		"GET /api/v2/things @lib/base.rb billing inherited-constant",
		"POST /api/v1/accounts/balance @lib/paths.rb billing constant",
	})
}

// A wrapper whose first argument is a metrics label and whose second is the
// path: the label is not a route, whether the call is on one line or spread
// over several.
func TestRubyHTTPClient_LabelThenPathNotARoute(t *testing.T) {
	src := `def fetch
  Catalog::ApiClient.get(
    'product lookup',
    'products/graph/attributes',
  )
  Catalog::ApiClient.get('pricing', 'categories/pricing')
  Catalog::ApiClient.post(
    'listing',
    PATH,
    { attributes: graph_params },
  )
end
`
	if got := clientRoutes(extractRubyHTTPClientFacts([]byte(src), "app/domain/catalog/fetch.rb")); len(got) != 0 {
		t.Errorf("got %d client routes from (label, path) calls, want 0: %+v", len(got), got)
	}
}
