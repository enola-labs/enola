package releaseaddresses

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func application(repo, name, cluster, ns, env string, serves, addresses []string) facts.Fact {
	p := map[string]any{
		"language": "kubernetes", "k8s_kind": "Application",
		"destination_cluster": cluster, "destination_namespace": ns,
		"serves": serves,
	}
	if env != "" {
		p["environment"] = env
	}
	if addresses != nil {
		// A store read back from JSON holds lists as []any.
		l := make([]any, len(addresses))
		for i, a := range addresses {
			l[i] = a
		}
		p["addresses"] = l
	}
	return facts.Fact{Kind: facts.KindSymbol, Repo: repo, Name: name, Props: p}
}

func deps(store *facts.Store, repo, name string) []string {
	var out []string
	for _, f := range store.ByName(name) {
		if f.Repo != repo {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelDependsOn {
				out = append(out, r.Target)
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestAnAddressNamesTheApplicationServingIt(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		// The services' deployments live in a generated repository.
		application("gen-deploy", "cart.Application.cart-api-eu1prod1", "eu1prod1", "cart", "production", []string{"cart-api"}, nil),
		application("gen-deploy", "cart.Application.cart-api-us1prod1", "us1prod1", "cart", "production", []string{"cart-api"}, nil),
		application("gen-deploy", "search.Application.search-eu1apps1", "eu1apps1", "search", "production", []string{"search"}, nil),
		application("gen-deploy", "search.Application.search-eu1appsb1", "eu1appsb1", "search", "sandbox", []string{"search"}, nil),
		application("gen-deploy", "search.Application.search-us1apps1", "us1apps1", "search", "production", []string{"search"}, nil),
		application("gen-deploy", "ledger.Application.ledger-ap1prod1", "ap1prod1", "ledger", "production", []string{"ledger"}, nil),
		application("gen-deploy", "ledger.Application.ledger-ap2prod1", "ap2prod1", "ledger", "production", []string{"ledger"}, nil),
		application("gen-deploy", "pricing.Application.pricing-ap1prod1", "ap1prod1", "pricing", "", []string{"pricing-api"}, nil),
		// The caller lives in the platform repository.
		application("platform-deploy", "web.Application.web-eu1prod1", "eu1prod1", "web", "production", []string{"web"}, []string{
			"cart-api.cart:8080",  // its own cluster's, not us1's
			"search.search",       // none on eu1prod1: eu1's production neighbour
			"ledger.ledger:80",    // two, neither nearby: no guess
			"pricing-api.pricing", // one, far away: the one there is
			"nobody.cart",         // a name nothing in cart serves
		}),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"cart.Application.cart-api-eu1prod1",
		"pricing.Application.pricing-ap1prod1",
		"search.Application.search-eu1apps1",
	}
	if got := deps(store, "platform-deploy", "web.Application.web-eu1prod1"); !reflect.DeepEqual(got, want) {
		t.Errorf("depends_on = %v\nwant         %v", got, want)
	}
	// Binders run again on every append: no duplicate edges.
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if got := deps(store, "platform-deploy", "web.Application.web-eu1prod1"); len(got) != len(want) {
		t.Errorf("after a second bind depends_on = %v", got)
	}
}

func TestParse(t *testing.T) {
	for in, want := range map[string][2]string{
		"cart-api.cart:8080": {"cart-api", "cart"},
		"cart-api.cart":      {"cart-api", "cart"},
	} {
		if n, ns := parse(in); n != want[0] || ns != want[1] {
			t.Errorf("parse(%q) = %q, %q", in, n, ns)
		}
	}
}
