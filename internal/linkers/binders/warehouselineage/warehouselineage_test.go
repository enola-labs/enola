package warehouselineage

import (
	"context"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func source(repo, project, name string, props map[string]any) facts.Fact {
	p := map[string]any{"storage_kind": "table", "engine": engineSource, "language": "dbt", "dbt_project": project}
	for k, v := range props {
		p[k] = v
	}
	return facts.Fact{Kind: facts.KindStorage, Name: name, File: project + "/models/sources.yml", Repo: repo, Props: p}
}

func built(repo, project, file, name string) facts.Fact {
	return facts.Fact{Kind: facts.KindStorage, Name: name, File: file, Repo: repo, Props: map[string]any{
		"storage_kind": "table", "engine": engineModel, "language": "dbt", "dbt_project": project,
	}}
}

func record(repo, full string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: "avro." + full, File: "schemas/x.avdl", Repo: repo, Props: map[string]any{
		"symbol_kind": facts.SymbolType, "language": "avro", "avro_kind": "record",
	}}
}

// A warehouse in its own repositories, fed by another dbt project, an event
// contract, a database's change feed and a database's landed copy.
func lineageStore() *facts.Store {
	s := facts.NewStore()
	s.Add(
		// The shop service: a Rails model and a plain table.
		facts.Fact{Kind: facts.KindStorage, Name: "Order", File: "app/models/order.rb", Repo: "shop",
			Props: map[string]any{"storage_kind": "model", "table": "orders", "language": "ruby"}},
		facts.Fact{Kind: facts.KindStorage, Name: "line_items", File: "db/structure.sql", Repo: "shop",
			Props: map[string]any{"storage_kind": "table", "language": "sql"}},
		// A Go service whose queries name its table.
		facts.Fact{Kind: facts.KindStorage, Name: "holds", File: "internal/repo/holds.go", Repo: "ledger-api",
			Props: map[string]any{"storage_kind": "table_reference", "language": "go", "operation": "INSERT"}},
		record("registry", "cdc.ledger_api.holds"),
		// The schema registry: an event contract and the orders change feed.
		record("registry", "contracts.shop.orders.order_placed"),
		record("registry", "cdc.shop.orders"),
		// A stream job consuming the sandbox copy's change feed.
		facts.Fact{Kind: facts.KindStorage, Name: "cdc.shop_sandbox.orders", File: "jobs/app.conf", Repo: "streams",
			Props: map[string]any{"storage_kind": facts.StorageKindTopic}},
		facts.Fact{Kind: facts.KindStorage, Name: "events", File: "jobs/app.conf", Repo: "streams",
			Props: map[string]any{"storage_kind": facts.StorageKindTopic}},
		// The core dbt repo: two projects building a users dimension each.
		built("lake", "people", "people/models/dimensions/dim_users.sql", "dim_users"),
		built("lake", "legacy", "legacy/models/staging/dim_users.sql", "dim_users"),
		built("lake", "sales", "sales/models/marts/fct_sales.sql", "fct_sales"),
		// The reporting dbt repo's sources.
		source("reports", "finance", "people__dimensions.dim_users", map[string]any{"schema": "dimensions"}),
		source("reports", "finance", "sales__marts.fct_sales", nil),
		source("reports", "finance", "contracts.shop_orders_order_placed", nil),
		source("reports", "finance", "cdc_changelog.shop_orders", nil),
		source("reports", "finance", "raw.shop__line_items", nil),
		source("reports", "finance", "raw.events", nil),
		source("reports", "finance", "raw.unknown_table", nil),
	)
	return s
}

func depends(s *facts.Store, repo, name string) []string {
	var out []string
	for _, f := range s.FactsRef() {
		if f.Repo != repo || f.Name != name {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelDependsOn {
				out = append(out, r.Target)
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestSourcesLinkToWhatLoadsThem(t *testing.T) {
	s := lineageStore()
	for range 2 { // the second run must add nothing
		if err := New().Bind(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		repo, name string
		want       []string
	}{
		// Another project's model; the schema picks one of two dimensions.
		{"reports", "people__dimensions.dim_users", []string{"dim_users"}},
		{"reports", "sales__marts.fct_sales", []string{"fct_sales"}},
		// An event contract, separators aside.
		{"reports", "contracts.shop_orders_order_placed", []string{"avro.contracts.shop.orders.order_placed"}},
		// A change feed landed without its namespace's first segment.
		{"reports", "cdc_changelog.shop_orders", []string{"avro.cdc.shop.orders"}},
		// A landed database copy with no record: the database names the repo.
		{"reports", "raw.shop__line_items", []string{"line_items"}},
		// A one-segment topic and an unknown table name nothing.
		{"reports", "raw.events", nil},
		{"reports", "raw.unknown_table", nil},
		// The change feed and the sandbox copy's topic reach the model.
		{"registry", "avro.cdc.shop.orders", []string{"Order"}},
		// A database named like its repository, underscores for hyphens.
		{"registry", "avro.cdc.ledger_api.holds", []string{"holds"}},
		{"streams", "cdc.shop_sandbox.orders", []string{"Order"}},
	}
	for _, c := range cases {
		if got := depends(s, c.repo, c.name); !slices.Equal(got, c.want) {
			t.Errorf("%s depends on %v, want %v", c.name, got, c.want)
		}
	}
	for _, f := range s.FactsRef() {
		if f.Name == "people__dimensions.dim_users" && f.PropString(PropLineage) != "model" {
			t.Errorf("lineage = %q, want model", f.PropString(PropLineage))
		}
	}
}

// Two equally good candidates link nothing.
func TestAmbiguousBuiltTableStaysUnlinked(t *testing.T) {
	s := lineageStore()
	s.Add(source("reports", "finance", "people__other.dim_users", map[string]any{"schema": "other"}))
	if err := New().Bind(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got := depends(s, "reports", "people__other.dim_users"); got != nil {
		t.Errorf("an ambiguous source depends on %v", got)
	}
}
