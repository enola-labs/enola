package workloadsource

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func module(repo, name, file, lang string, prefixed bool) facts.Fact {
	return facts.Fact{Kind: facts.KindModule, Name: name, File: file, Repo: repo, RepoPrefixed: prefixed, Props: map[string]any{"language": lang}}
}

func workload(repo, name, dir string, contexts ...string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindSymbol, Name: name, File: dir + "/app.yaml", Repo: repo,
		Props:     map[string]any{"language": "kubernetes", facts.PropBuildContexts: contexts},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
	}
}

func edges(store *facts.Store) []string {
	var out []string
	for _, f := range store.ByKind(facts.KindDependency) {
		if f.PropString("source") == SourceLabel {
			out = append(out, f.Repo+":"+f.Name)
		}
	}
	sort.Strings(out)
	return out
}

func TestAWorkloadDependsOnTheModulesItsImageIsBuiltFrom(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		module("shop", "kubernetes-manifests", "kubernetes-manifests", "kubernetes", false),
		module("shop", "example.com/shop/src/checkout", "src/checkout", "go", false),
		module("shop", "example.com/shop/src/checkout/money", "src/checkout/money", "go", false),
		module("shop", "src/checkoutextra", "src/checkoutextra", "python", false),
		module("shop", "src/cart", "src/cart", "dotnet", false),
		module("shop", "helm-chart", "helm-chart", "helm", false),
		workload("shop", "kubernetes-manifests.Deployment.checkout", "kubernetes-manifests", "src/checkout"),
		// Another repository's module in a directory of the same name is not this one.
		module("other", "example.com/other/src/checkout", "other/src/checkout", "go", true),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"shop:kubernetes-manifests -> example.com/shop/src/checkout",
		"shop:kubernetes-manifests -> example.com/shop/src/checkout/money",
	}
	if got := edges(store); !reflect.DeepEqual(got, want) {
		t.Errorf("edges = %v, want %v", got, want)
	}
	// Running again over the same store, as every append does, adds nothing.
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if got := edges(store); !reflect.DeepEqual(got, want) {
		t.Errorf("after a second pass edges = %v, want %v", got, want)
	}
}

func TestAPrefixedRepositoryIsComparedByItsOwnPaths(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		module("demo", "src/web", "demo/src/web", "typescript", true),
		workload("demo", "deploy.Deployment.web", "deploy", "src/web"),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if got := edges(store); !reflect.DeepEqual(got, []string{"demo:deploy -> src/web"}) {
		t.Errorf("edges = %v", got)
	}
}
