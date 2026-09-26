package server_test

import (
	"fmt"
	"os"
	"path/filepath"
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

// TestQueryFacts_RetriesANameCarryingTheRepoLabel: in a multi-repo store every file
// path starts with its repo label and no name does, so a name copied from a path
// matched nothing. An empty answer is retried without the label, scoped to that repo,
// and says so.
func TestQueryFacts_RetriesANameCarryingTheRepoLabel(t *testing.T) {
	root := t.TempDir()
	alpha, beta := filepath.Join(root, "alpha"), filepath.Join(root, "beta")
	writeGoRepo(t, alpha, "AlphaUpper")
	writeGoRepo(t, beta, "BetaUpper")
	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_paths": []string{alpha, beta}}); res.IsError {
		t.Fatalf("repo_paths: %s", text(res))
	}

	out := text(s.call(t, "query_facts", map[string]any{
		"kind": "symbol", "names": []string{"beta/internal/svc.BetaUpper"}, "output_mode": "compact",
	}))
	if !strings.Contains(out, "internal/svc.BetaUpper") || !strings.Contains(out, `Retried without "beta/"`) {
		t.Errorf("a repo-labelled name was not retried without the label:\n%s", out)
	}
}

// TestQueryFacts_WarnsWhenANameMatchesMostOfTheStore: name is a case-insensitive
// substring, and a short one matched most of the store, which an agent read as the
// filter being ignored. The answer now says how much it matched and why.
func TestQueryFacts_WarnsWhenANameMatchesMostOfTheStore(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "many")
	var b strings.Builder
	b.WriteString("package svc\n\n")
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&b, "// Detail%d is a function.\nfunc Detail%d() {}\n\n", i, i)
	}
	for rel, body := range map[string]string{
		"go.mod":              "module example.com/many\n\ngo 1.22\n",
		"internal/svc/svc.go": b.String(),
	} {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": repo, "fresh": true}); res.IsError {
		t.Fatalf("snapshot: %s", text(res))
	}

	out := text(s.call(t, "query_facts", map[string]any{"kind": "symbol", "name": "ai", "output_mode": "summary"}))
	if !strings.Contains(out, `name contains "ai" matched`) || !strings.Contains(out, "case-insensitive substring") {
		t.Errorf("a name matching most of the store carried no warning:\n%.600s", out)
	}
	narrow := text(s.call(t, "query_facts", map[string]any{"kind": "symbol", "name": "Detail17", "output_mode": "summary"}))
	if strings.Contains(narrow, "matched") && strings.Contains(narrow, "case-insensitive substring, so a short") {
		t.Errorf("a narrow name was warned about:\n%.600s", narrow)
	}
}
