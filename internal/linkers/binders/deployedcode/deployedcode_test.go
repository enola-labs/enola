package deployedcode

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func app(name, ns string, images []any, serves ...string) facts.Fact {
	p := map[string]any{"language": "kubernetes", "k8s_kind": "Application", "destination_namespace": ns}
	if images != nil {
		p["images"] = images
	}
	if serves != nil {
		p["serves"] = serves
	}
	return facts.Fact{Kind: facts.KindSymbol, Repo: "shop-deploy", Name: name, Props: p}
}

func TestAnApplicationDeploysTheRepositoryItsImageIsBuiltFrom(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindModule, Repo: "shop_web", Name: "web"},
		facts.Fact{Kind: facts.KindModule, Repo: "cart-api", Name: "cart"},
		facts.Fact{Kind: facts.KindModule, Repo: "shop-deploy", Name: "web"},
		// The registry host is skipped; the first segment naming a repo answers.
		app("by-image", "storefront", []any{"registry.example.com:5000/shop-web/app:1.2", "acme/tools/migrate:3"}),
		// No image names a repo: the namespace does.
		app("by-namespace", "cart-api", []any{"acme/cart:9"}),
		// Two repos in the images: the namespace picks one of them.
		app("tie-broken", "cart-api", []any{"shop-web/app:1", "cart-api/app:1"}),
		// Two repos and a namespace naming neither: none.
		app("ambiguous", "other", []any{"shop-web/app:1", "cart-api/app:1"}),
		// Nothing but a served name.
		app("by-serves", "misc", nil, "nothing-here", "Cart_API"),
		// The deployment repository's own name never answers.
		app("own", "shop-deploy", []any{"shop-deploy/app:1"}),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"by-image": "shop_web", "by-namespace": "cart-api", "tie-broken": "cart-api",
		"ambiguous": "", "by-serves": "cart-api", "own": "",
	}
	for name, repo := range want {
		f := store.ByName(name)[0]
		if got := f.PropString(PropDeploysRepo); got != repo {
			t.Errorf("%s deploys_repo = %q, want %q", name, got, repo)
		}
	}
}

func TestImageSegments(t *testing.T) {
	for in, want := range map[string]string{
		"registry.example.com/team/web:1.2@sha256:ab": "team/web",
		"localhost/web": "web",
		"team/web:1":    "team/web",
		"web":           "web",
	} {
		got := imageSegments(in)
		joined := ""
		for i, s := range got {
			if i > 0 {
				joined += "/"
			}
			joined += s
		}
		if joined != want {
			t.Errorf("imageSegments(%q) = %v, want %s", in, got, want)
		}
	}
}
