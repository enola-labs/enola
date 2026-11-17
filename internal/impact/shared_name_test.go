package impact

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/resolve"
)

// twoAppsStore is two repositories that each hold a package app/models with a
// class in it, each imported and used by its own controllers.
func twoAppsStore() *facts.Store {
	s := facts.NewStore()
	for _, repo := range []string{"shop", "ledger"} {
		s.Add(
			facts.Fact{Kind: facts.KindModule, Name: "app/models", File: repo + "/app/models", Repo: repo, RepoPrefixed: true},
			facts.Fact{Kind: facts.KindModule, Name: "app/controllers", File: repo + "/app/controllers", Repo: repo, RepoPrefixed: true},
			facts.Fact{Kind: facts.KindSymbol, Name: "Entry", File: repo + "/app/models/entry.rb", Repo: repo, RepoPrefixed: true,
				Props: map[string]any{"symbol_kind": facts.SymbolClass}},
			facts.Fact{Kind: facts.KindSymbol, Name: repo + "Controller", File: repo + "/app/controllers/c.rb", Repo: repo, RepoPrefixed: true,
				Relations: []facts.Relation{{Kind: facts.RelNames, Target: "Entry"}}},
			facts.Fact{Kind: facts.KindDependency, Name: "app/controllers -> app/models", File: repo + "/app/controllers/c.rb", Repo: repo, RepoPrefixed: true,
				Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/models"}}},
		)
	}
	s.BuildGraph()
	return s
}

func TestAnalyze_PackageSharedAcrossReposStaysInTheNamedRepo(t *testing.T) {
	store := twoAppsStore()
	r := resolve.Resolver{Store: store, RepoPaths: map[string]string{"shop": "/x/shop", "ledger": "/x/ledger"}}
	report, err := Analyze(r, Request{Target: "repo:ledger app/models"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range report.Modules {
		if m.Repo != "ledger" {
			t.Fatalf("ledger's app/models listed a dependent package in %s: %+v", m.Repo, report.Modules)
		}
	}
	if len(report.Modules) != 1 || report.Modules[0].Module != "app/controllers" {
		t.Fatalf("dependent packages = %+v, want ledger's app/controllers", report.Modules)
	}
	var deps []string
	for _, nodes := range report.ByDepth {
		for _, n := range nodes {
			deps = append(deps, n.Repo+":"+n.Name)
		}
	}
	if slices.Contains(deps, "shop:shopController") {
		t.Fatalf("the other repository's controller is a dependent: %v", deps)
	}
	if !slices.Contains(deps, "ledger:ledgerController") {
		t.Fatalf("the repository's own controller is missing: %v", deps)
	}
	if len(report.CrossRepoImpact) != 0 {
		t.Fatalf("cross-repo impact = %v, want none", report.CrossRepoImpact)
	}
}

func TestAnalyze_UnscopedSharedNameAsksWhichRepo(t *testing.T) {
	store := twoAppsStore()
	r := resolve.Resolver{Store: store, RepoPaths: map[string]string{"shop": "/x/shop", "ledger": "/x/ledger"}}
	report, err := Analyze(r, Request{Target: "app/models"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Resolved() {
		t.Fatalf("an unscoped name in two repositories resolved to %q", report.Resolution.Matched)
	}
	want := []string{facts.ScopedName("ledger", "app/models"), facts.ScopedName("shop", "app/models")}
	got := slices.Clone(report.Resolution.Alternatives)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("alternatives = %v, want %v", got, want)
	}
}
