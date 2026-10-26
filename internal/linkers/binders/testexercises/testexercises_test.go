package testexercises

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/vocab"
)

func route(name, method, role, typ string) facts.Fact {
	f := facts.Fact{Kind: facts.KindRoute, Name: name, Repo: "app", File: "src/routes.ts",
		Props: map[string]any{facts.PropRole: role}}
	if method != "" {
		f.Props["method"] = method
	}
	if typ != "" {
		f.Props[facts.PropRouteType] = typ
	}
	return f
}

func exercised(store *facts.Store) ([]string, []string) {
	for _, f := range store.All() {
		if f.Kind != facts.KindTestRef {
			continue
		}
		var out []string
		for _, r := range f.Relations {
			if r.Kind == facts.RelExercises {
				out = append(out, r.Target)
			}
		}
		sort.Strings(out)
		un, _ := f.PropAny(PropUnmatched).([]string)
		return out, un
	}
	return nil, nil
}

func TestBindExercises(t *testing.T) {
	store := facts.NewStore()
	op := route("Query.listWidgets", "", facts.RoleClient, facts.RouteTypeGraphQL)
	op.Props["operation"] = "ListWidgets"
	store.Add(
		route("/widgets", "GET", facts.RoleServer, ""),
		route("/widgets/:id", "DELETE", facts.RoleServer, ""),
		route("/", "GET", facts.RoleServer, ""),
		route("/widget/:id", "", "", "page"),
		route("Query.listWidgets", "", facts.RoleServer, facts.RouteTypeGraphQL),
		route("Mutation.saveWidget", "", facts.RoleServer, facts.RouteTypeGraphQL),
		op,
		facts.Fact{Kind: facts.KindTestRef, Name: "cypress/widgets.cy.ts", File: "cypress/widgets.cy.ts", Repo: "app",
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "src.helper"}},
			Props: map[string]any{
				PropHTTPCalls:  []string{"GET /", "GET /widgets", "DELETE /widgets/:param", "GET /likes/:param"},
				PropPageVisits: []string{"/widget/:param"},
				PropGraphQL:    []string{"Mutation.saveWidget"},
				PropGraphQLOps: []string{"ListWidgets", "Unknown"},
			}},
	)
	b := New(vocab.Default())
	for round := 0; round < 2; round++ { // a re-bind after an append changes nothing
		if err := b.Bind(context.Background(), store); err != nil {
			t.Fatal(err)
		}
		got, unmatched := exercised(store)
		want := []string{"/widget/:id", "/widgets", "/widgets/:id", "Mutation.saveWidget", "Query.listWidgets"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round %d: exercises %v, want %v", round, got, want)
		}
		if !reflect.DeepEqual(unmatched, []string{"GET /", "GET /likes/:param", "operation Unknown"}) {
			t.Errorf("round %d: unmatched %v", round, unmatched)
		}
	}
	for _, f := range store.All() {
		if f.Kind == facts.KindTestRef && !f.HasRelation(facts.RelCalls, "src.helper") {
			t.Error("the test_ref lost its own calls")
		}
	}
}
