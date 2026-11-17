package lookmlextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extract(t *testing.T, files map[string]string) map[string]facts.Fact {
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
	ff, err := New().Extract(context.Background(), dir, rels)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]facts.Fact{}
	for _, f := range ff {
		out[f.Name] = f
	}
	return out
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

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Views read their table by its last segment, a derived table the tables its SQL
// names, and an extending view its base. An explore depends on its base view and
// its joins, under the model file's name. SQL holding braces does not unbalance
// the parse, and comments are skipped.
func TestLookMLViewsAndExplores(t *testing.T) {
	got := extract(t, map[string]string{
		"views/orders.view.lkml": `# the orders mart
view: orders {
  sql_table_name: analytics-prod.marts.fct_orders ;;

  dimension: id {
    primary_key: yes
    sql: ${TABLE}.id ;;
  }
  measure: total {
    type: sum
    sql: CASE WHEN ${TABLE}.x > 0 THEN {% parameter p %} END ;;
  }
}

view: orders_extended {
  extends: [orders]
}
`,
		"views/daily.view.lkml": `view: daily_summary {
  derived_table: {
    sql: SELECT d.day, count(*) FROM ` + "`analytics-prod.marts.fct_orders`" + ` o
         JOIN analytics-prod.marts.dim_dates d ON true GROUP BY 1 ;;
  }
}
`,
		"models/shop.model.lkml": `connection: "warehouse"
include: "/views/*.view.lkml"

explore: orders {
  label: "Orders"
  join: users {
    sql_on: ${orders.user_id} = ${users.id} ;;
    relationship: many_to_one
  }
  join: buyers {
    from: users
  }
}

explore: daily {
  from: daily_summary
}
`,
	})
	for name, want := range map[string][]string{
		"lookml.orders":          {"fct_orders"},
		"lookml.orders_extended": {"lookml.orders"},
		"lookml.daily_summary":   {"dim_dates", "fct_orders"},
		"lookml.shop.orders":     {"lookml.orders", "lookml.users"},
		"lookml.shop.daily":      {"lookml.daily_summary"},
	} {
		f, ok := got[name]
		if !ok {
			t.Errorf("missing %s", name)
			continue
		}
		if d := deps(f); !eq(d, want) {
			t.Errorf("%s depends on %v, want %v", name, d, want)
		}
		if f.PropString(facts.PropFrameworkRoot) != "looker" {
			t.Errorf("%s is not a root", name)
		}
	}
	if len(got) != 5 {
		t.Errorf("facts = %d, want 5 (dimensions and measures are not symbols)", len(got))
	}
}
