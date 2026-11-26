package orphans

import (
	"strings"
	"testing"
)

// TestReachedDynamicallyStaysReportedAtLowConfidence pins that an export a computed
// namespace lookup may reach (`ns[key]` with a key the code does not pin down) is
// still reported, whatever its class, but at low confidence and with the lookup as
// the reason. Its neighbour, which nothing looks up, keeps its tier.
func TestReachedDynamicallyStaysReportedAtLowConfidence(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"src/util.byTimeDesc","file":"src/util/sortOrders.js","props":{"symbol_kind":"function","language":"typescript","exported":true,"reached_dynamically":true,"reached_dynamically_from":["src/stores/pages.js"]}}
{"kind":"symbol","name":"src/util.byNothing","file":"src/util/sortOrders.js","props":{"symbol_kind":"function","language":"typescript","exported":true,"reached_dynamically":true,"reached_dynamically_from":["src/stores/a.js","src/stores/pages.js"]}}
{"kind":"symbol","name":"src/util.plain","file":"src/util/other.js","props":{"symbol_kind":"function","language":"typescript","exported":true}}
{"kind":"test_ref","name":"src/tests/util/sortOrders.spec.js","file":"src/tests/util/sortOrders.spec.js","relations":[{"kind":"calls","target":"src/util.byTimeDesc"}]}
`
	syms, refs := collectFromJSONL(t, jsonl)
	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	o, ok := got["src/util.byTimeDesc"]
	if !ok || o.Class != classTestOnly || o.Confidence != confLow {
		t.Fatalf("byTimeDesc: %+v, want test-only at low confidence", o)
	}
	if !strings.Contains(o.Note, "may be reached through ns[...] in src/stores/pages.js") {
		t.Errorf("byTimeDesc note %q does not name the lookup", o.Note)
	}
	o = got["src/util.byNothing"]
	if o.Class != classIsolated || o.Confidence != confLow || !strings.Contains(o.Note, "in src/stores/a.js and 1 more file") {
		t.Errorf("byNothing: %+v", o)
	}
	if o := got["src/util.plain"]; o.Confidence != confMedium {
		t.Errorf("plain: confidence %s, want medium", o.Confidence)
	}
	low := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all", Confidence: confLow}))
	if _, ok := low["src/util.byTimeDesc"]; !ok {
		t.Error("confidence=low does not list a dynamically reached export")
	}
}
