package dbtextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extract(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	var rels []string
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	e := New()
	var owned []string
	for _, r := range rels {
		if e.OwnsFile(r) {
			owned = append(owned, r)
		}
	}
	ff, err := e.Extract(context.Background(), dir, owned)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

func deps(f facts.Fact) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == facts.RelDependsOn {
			out = append(out, r.Target)
		}
	}
	sort.Strings(out)
	return out
}

// Two projects in one repository: a model refs a model of its own project, one of
// the other project (with and without naming it), and a declared source. The
// installed package copy and compiled output are not read.
func TestDbtModelsRefsAndSources(t *testing.T) {
	ff := extract(t, map[string]string{
		"shared/dbt_project.yml":                  "name: shared\nmodel-paths: [models]\n",
		"shared/models/dim_dates.sql":             "select 1 as d\n",
		"orders/dbt_project.yml":                  "name: orders\n",
		"orders/models/staging/_sources.yml":      "version: 2\nsources:\n  - name: cdc_changelog\n    database: raw\n    tables:\n      - name: shop_orders\n        description: Orders as written by the shop.\n",
		"orders/models/staging/stg_orders.sql":    "{{ config(materialized='view') }}\nselect * from {{ source('cdc_changelog', 'shop_orders') }}\n",
		"orders/models/marts/fct_orders.sql":      "select o.*, d.d from {{ ref(\"stg_orders\") }} o\njoin {{ ref('dim_dates') }} d on true\njoin {{ ref('shared', 'dim_dates') }} d2 on true\njoin {{ ref('country_codes') }} c on true\n",
		"orders/models/marts/num_daily.sql":       "{{ config(alias='num_dau') }}\nselect count(*) from {{ ref('stg_orders') }}\n",
		"orders/seeds/country_codes.csv":          "code,name\nFR,France\n",
		"orders/models/marts/_marts.yml":          "version: 2\nmodels:\n  - name: fct_orders\n    description: One row per order.\n",
		"orders/snapshots/orders_snapshot.sql":    "{% snapshot orders_snapshot %}\nselect * from {{ ref('stg_orders') }}\n{% endsnapshot %}\n",
		"orders/analyses/adhoc.sql":               "select * from {{ ref('fct_orders') }}\n",
		"orders/dbt_packages/shared/models/x.sql": "select * from {{ ref('ghost') }}\n",
		"orders/target/compiled/fct_orders.sql":   "select 1\n",
	})
	got := map[string]facts.Fact{}
	for _, f := range ff {
		got[f.Kind+" "+f.Name] = f
	}
	fct, ok := got["symbol orders.fct_orders"]
	if !ok {
		t.Fatalf("no orders.fct_orders among %v", keys(got))
	}
	if d := deps(fct); len(d) != 3 || d[0] != "orders.country_codes" || d[1] != "orders.stg_orders" || d[2] != "shared.dim_dates" {
		t.Errorf("fct_orders depends on %v, want the seed, orders.stg_orders and shared.dim_dates", d)
	}
	if got["symbol orders.country_codes"].PropString("dbt_resource") != "seed" {
		t.Errorf("the seed is not a seed: %v", got["symbol orders.country_codes"].Props)
	}
	for table, model := range map[string]string{"fct_orders": "orders.fct_orders", "num_dau": "orders.num_daily"} {
		tf, ok := got["storage "+table]
		if !ok || len(deps(tf)) != 1 || deps(tf)[0] != model {
			t.Errorf("table %s: found %v, depends on %v, want %s", table, ok, deps(tf), model)
		}
	}
	if _, ok := fct.Props["exported"]; ok {
		t.Errorf("a model carries an exported prop: %v", fct.Props)
	}
	if fct.PropString(facts.PropFrameworkRoot) != "dbt" {
		t.Errorf("a model is not a root: %v", fct.Props)
	}
	if fct.PropString(facts.PropDoc) == "" {
		t.Errorf("fct_orders lost its description: %v", fct.Props)
	}
	if d := deps(got["symbol orders.stg_orders"]); len(d) != 1 || d[0] != "cdc_changelog.shop_orders" {
		t.Errorf("stg_orders depends on %v, want the source", d)
	}
	if _, ok := got["storage cdc_changelog.shop_orders"]; !ok {
		t.Errorf("the declared source table is missing: %v", keys(got))
	}
	if _, ok := got["symbol orders.orders_snapshot"]; !ok {
		t.Errorf("the snapshot is missing: %v", keys(got))
	}
	for k := range got {
		switch k {
		case "symbol orders.adhoc", "symbol orders.x", "symbol shared.x":
			t.Errorf("%s was read: analyses, installed packages and compiled output are not models", k)
		}
	}
}

func keys(m map[string]facts.Fact) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
