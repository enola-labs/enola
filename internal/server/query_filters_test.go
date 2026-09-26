package server_test

import (
	"strings"
	"testing"
)

// TestQueryFacts_StatesTheFiltersItReceived: an agent that meant to filter, sent a call
// without the filter, and got the unfiltered result concluded the filter was ignored,
// then repeated the call. The answer now leads with what arrived, and says outright
// when nothing narrowed it.
func TestQueryFacts_StatesTheFiltersItReceived(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	for name, tc := range map[string]struct {
		args        map[string]any
		want, avoid string
	}{
		"nothing narrowing, json": {
			map[string]any{"limit": 5},
			"No kind, name, file or prop filter was sent, so this is the whole store", "",
		},
		"nothing narrowing, compact": {
			map[string]any{"limit": 5, "output_mode": "compact"},
			"Filters applied: limit=5. No kind, name, file or prop filter was sent", "",
		},
		"filtered": {
			map[string]any{"kind": "symbol", "name": "Handle", "output_mode": "compact"},
			`Filters applied: kind=symbol, name contains "Handle" (case-insensitive substring)`, "No kind, name",
		},
	} {
		out := text(s.call(t, "query_facts", tc.args))
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: answer does not state its filters (want %q):\n%.400s", name, tc.want, out)
		}
		if tc.avoid != "" && strings.Contains(out, tc.avoid) {
			t.Errorf("%s: a filtered call was described as unfiltered:\n%.400s", name, out)
		}
	}
}
