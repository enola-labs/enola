package avroextractor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

func rels(f facts.Fact, kind string) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == kind {
			out = append(out, r.Target)
		}
	}
	sort.Strings(out)
	return out
}

// An IDL record uses a type of its own protocol and one imported by its dotted
// name; annotations with JSON arguments and comments do not disturb the parse.
// A JSON schema's record nests another. Each record names its full name.
func TestAvroNamedTypesAndReferences(t *testing.T) {
	got := extract(t, map[string]string{
		"avro/common/metadata.avdl": `@namespace("events.common")
protocol metadata_protocol {
  record EventMetadata {
    string id;
    timestamp_ms time;
  }
}
`,
		"avro/orders/placed.avdl": `@namespace("events.orders")
protocol placed_protocol {
  import idl "../common/metadata.avdl";

  /** An order was placed. */
  @owner({"team": "shop", "notes": {"a": 1}})
  record order_placed {
    // the shared envelope
    events.common.EventMetadata metadata;
    union { null, Channel } channel = null;
    array<Line> lines;
    long @primaryKey(true) id;
  }
  enum Channel { web, app }
  record Line { string sku; int qty; }
}
`,
		"schemas/payment.avsc": `{"type": "record", "name": "payment", "namespace": "cdc.payments",
 "doc": "A payment row.",
 "fields": [
   {"name": "id", "type": "long"},
   {"name": "method", "type": {"type": "record", "name": "Method", "fields": [{"name": "kind", "type": "string"}]}},
   {"name": "refunds", "type": {"type": "array", "items": "Method"}}
 ]}`,
	})
	placed, ok := got["avro.events.orders.order_placed"]
	if !ok {
		t.Fatalf("missing order_placed among %v", keys(got))
	}
	if d := rels(placed, facts.RelDependsOn); !slices.Equal(d, []string{"avro.events.common.EventMetadata", "avro.events.orders.Channel", "avro.events.orders.Line"}) {
		t.Errorf("order_placed depends on %v", d)
	}
	if n := rels(placed, facts.RelNames); !slices.Equal(n, []string{"events.orders.order_placed"}) {
		t.Errorf("order_placed names %v, want its full name", n)
	}
	if placed.PropString(facts.PropDoc) != "An order was placed." {
		t.Errorf("doc = %q", placed.PropString(facts.PropDoc))
	}
	pay, ok := got["avro.cdc.payments.payment"]
	if !ok {
		t.Fatalf("missing the JSON schema's record among %v", keys(got))
	}
	if d := rels(pay, facts.RelDependsOn); !slices.Equal(d, []string{"avro.cdc.payments.Method"}) {
		t.Errorf("payment depends on %v", d)
	}
	for _, name := range []string{"avro.events.common.EventMetadata", "avro.events.orders.Channel", "avro.events.orders.Line", "avro.cdc.payments.Method"} {
		if _, ok := got[name]; !ok {
			t.Errorf("missing %s among %v", name, keys(got))
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
