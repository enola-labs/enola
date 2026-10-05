package moduleedges

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func module(name string) facts.Fact {
	return facts.Fact{Kind: facts.KindModule, Name: name, File: name, Repo: "app",
		Props: map[string]any{"language": "ruby"}}
}

func symbol(name, file string, relations ...facts.Relation) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Repo: "app",
		Props: map[string]any{"language": "ruby", "symbol_kind": "method"}, Relations: relations}
}

func calls(targets ...string) []facts.Relation {
	out := make([]facts.Relation, 0, len(targets))
	for _, t := range targets {
		out = append(out, facts.Relation{Kind: facts.RelCalls, Target: t})
	}
	return out
}

func derivedEdges(store *facts.Store) map[string]int {
	out := map[string]int{}
	for _, f := range store.ByKind(facts.KindDependency) {
		if f.PropString(DerivedProp) != derivedFromSymbols {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports {
				weight, _ := f.Props[WeightProp].(int)
				out[strings.TrimPrefix(f.Name, "module-edge: ")] = weight
				_ = r
			}
		}
	}
	return out
}

// A call whose target resolves to a symbol in another directory is a module
// edge, weighted by how many symbol edges stand behind it. A call inside one
// directory is not an edge, a call naming no known symbol derives nothing,
// and a test directory contributes none.
func TestBind_ResolvedSymbolEdgesBecomeWeightedModuleEdges(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		module("app/services"), module("app/models"), module("app/jobs"), module("spec/services"),
		symbol("Reports::Build#call", "app/services/reports/build.rb", calls("Order#total", "Order#lines", "Reports::Build#collect", "vanished_helper")...),
		symbol("Reports::Build#collect", "app/services/reports/build.rb"),
		symbol("Order#total", "app/models/order.rb", calls("ImportJob#perform")...),
		symbol("Order#lines", "app/models/order.rb"),
		symbol("ImportJob#perform", "app/jobs/import_job.rb"),
		symbol("Reports::BuildSpec#test", "spec/services/reports/build_spec.rb", calls("Reports::Build#call")...),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	got := derivedEdges(store)
	if got["app/services -> app/models"] != 2 {
		t.Errorf("two calls into app/models are one edge of weight two: %v", got)
	}
	if got["app/models -> app/jobs"] != 1 {
		t.Errorf("single call edge missing: %v", got)
	}
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) != 2 {
		t.Fatalf("a same-directory call, an unresolved name and a spec directory derive nothing: %v", names)
	}
}

// A pair an extractor already connected keeps the extractor's edge: this
// fills the gap where nobody emitted one, it does not restate what was read.
func TestBind_PairsAnExtractorConnectedAreLeftAlone(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		module("src/api"), module("src/core"),
		facts.Fact{Kind: facts.KindDependency, Name: "src/api/client.ts", File: "src/api/client.ts", Repo: "app",
			Props:     map[string]any{"language": "typescript"},
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "src/core"}}},
		symbol("ApiClient.fetch", "src/api/client.ts", calls("CoreThing.read")...),
		symbol("CoreThing.read", "src/core/thing.ts"),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if got := derivedEdges(store); len(got) != 0 {
		t.Fatalf("the extractor already connected this pair: %v", got)
	}
}

// A directory name is repo-relative and names collide across repositories, so
// a call whose target sits in another repository derives nothing: that seam is
// the cross-repo linker's, measured rather than inferred from a shared name.
func TestBind_CallsIntoAnotherRepositoryDeriveNothing(t *testing.T) {
	store := facts.NewStore()
	other := func(f facts.Fact) facts.Fact { f.Repo = "sibling"; return f }
	store.Add(
		module("app/services"),
		other(module("app/models")),
		symbol("Reports::Build#call", "app/services/reports/build.rb", calls("Order#total")...),
		other(symbol("Order#total", "app/models/order.rb")),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if got := derivedEdges(store); len(got) != 0 {
		t.Fatalf("a cross-repository call must derive no module edge: %v", got)
	}
}

func injects(targets ...string) []facts.Relation {
	out := make([]facts.Relation, 0, len(targets))
	for _, t := range targets {
		out = append(out, facts.Relation{Kind: facts.RelInjects, Target: t})
	}
	return out
}

// A constructor-injected collaborator is how a dependency is declared under a DI
// container, and there is frequently no call or import edge beside it: the
// container does the constructing. The pair must reach the module layer, under the
// same guards as every other relation — a target naming no known symbol derives
// nothing, and one inside the same directory is not an edge.
func TestBind_InjectedCollaboratorsBecomeModuleEdges(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		module("app/controllers"), module("app/services"), module("app/models"),
		symbol("UsersController", "app/controllers/users_controller.rb",
			injects("UserService", "AuditService", "UsersController#helper", "ContainerRegisteredThing")...),
		symbol("UsersController#helper", "app/controllers/users_controller.rb"),
		symbol("UserService", "app/services/user_service.rb"),
		symbol("AuditService", "app/services/audit_service.rb"),
		symbol("Order", "app/models/order.rb"),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	edges := derivedEdges(store)
	if got, ok := edges["app/controllers -> app/services"]; !ok || got != 2 {
		t.Errorf("injection edges = %v, want app/controllers -> app/services weighted 2", edges)
	}
	if _, ok := edges["app/controllers -> app/controllers"]; ok {
		t.Error("an injection inside one directory is not a module edge")
	}
	if _, ok := edges["app/controllers -> app/models"]; ok {
		t.Error("derived an edge for an injected name no symbol declares")
	}
}

// The pair an extractor already stated stays the extractor's. An injection that
// merely restates an import must not appear twice in the module graph.
func TestBind_InjectionDoesNotRestateAnExtractorEdge(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		module("app/controllers"), module("app/services"),
		facts.Fact{Kind: facts.KindDependency, Name: "app/controllers/users_controller.rb", File: "app/controllers/users_controller.rb", Repo: "app",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/services"}}},
		symbol("UsersController", "app/controllers/users_controller.rb", injects("UserService")...),
		symbol("UserService", "app/services/user_service.rb"),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if edges := derivedEdges(store); len(edges) != 0 {
		t.Errorf("restated an edge the extractor already carries: %v", edges)
	}
}

func TestClassifyUnresolvedTarget(t *testing.T) {
	known := map[string]bool{
		"src.Widget::run":  true,
		"src.Other::run":   true,
		"spec.Helper#call": true,
		"src.Unique::save": true,
	}
	short := map[string]int{}
	for name := range known {
		short[symbolShortName(name)]++
	}

	tests := []struct {
		target, want string
	}{
		{"spec.Helper#call", reasonKnownUnplaced},
		{"wrong.Scope::save", reasonUniqueShort},
		{"wrong.Scope::run", reasonAmbiguousShort},
		{"external::send", reasonUnknown},
	}
	for _, tt := range tests {
		if got := classifyUnresolvedTarget(tt.target, known, short); got != tt.want {
			t.Errorf("classifyUnresolvedTarget(%q) = %q, want %q", tt.target, got, tt.want)
		}
	}
}

func TestFormatBreakdown_IsStableAndLeadsWithLargestGap(t *testing.T) {
	got := formatBreakdown(map[string]int{"cpp/calls/no_symbol_match": 9, "cpp/calls/unique_short_name_match": 3, "c/calls/no_symbol_match": 9})
	want := "c/calls/no_symbol_match=9, cpp/calls/no_symbol_match=9, cpp/calls/unique_short_name_match=3"
	if got != want {
		t.Fatalf("formatBreakdown = %q, want %q", got, want)
	}
}

func TestFormatTopTargets_IsStableAndBounded(t *testing.T) {
	got := formatTopTargets(map[string]int{"z": 2, "b": 4, "a": 4, "c": 1}, 3)
	if want := "a=4, b=4, z=2"; got != want {
		t.Fatalf("formatTopTargets = %q, want %q", got, want)
	}
}

// An appended repository's files carry its label, and the edge is added after the
// engine put it there. The edge sits at its source module, so it carries the label
// too; a repository indexed alone has bare files and a bare edge.
func TestBind_EdgeFileFollowsHowTheRepositoryNamesPaths(t *testing.T) {
	repo := func(label, prefix string) []facts.Fact {
		return []facts.Fact{
			{Kind: facts.KindModule, Name: "app/models", File: prefix + "app/models", Repo: label},
			{Kind: facts.KindModule, Name: "app/jobs", File: prefix + "app/jobs", Repo: label},
			{Kind: facts.KindSymbol, Name: label + "Model", File: prefix + "app/models/m.rb", Repo: label,
				Relations: []facts.Relation{{Kind: facts.RelCalls, Target: label + "Job"}}},
			{Kind: facts.KindSymbol, Name: label + "Job", File: prefix + "app/jobs/j.rb", Repo: label},
		}
	}
	files := func(store *facts.Store) map[string]string {
		out := map[string]string{}
		for _, f := range store.ByKind(facts.KindDependency) {
			if f.PropString(DerivedProp) == derivedFromSymbols {
				out[f.Repo] = f.File
			}
		}
		return out
	}

	union := facts.NewStore()
	union.Add(repo("shop", "shop/")...)
	union.Add(repo("blog", "blog/")...)
	if err := New().Bind(context.Background(), union); err != nil {
		t.Fatal(err)
	}
	if got := files(union); got["shop"] != "shop/app/models" || got["blog"] != "blog/app/models" {
		t.Errorf("appended repositories: edge files = %v, want each under its label", got)
	}

	alone := facts.NewStore()
	alone.Add(repo("shop", "")...)
	if err := New().Bind(context.Background(), alone); err != nil {
		t.Fatal(err)
	}
	if got := files(alone); got["shop"] != "app/models" {
		t.Errorf("a repository indexed alone: edge file = %q, want the bare module", got["shop"])
	}
}
