package perf

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestAnalyzeAgainstBundledSnapshot is an integration smoke test that runs the
// collector + analyzer against the repo's bundled .enola/facts.jsonl  It is skipped when the snapshot is absent so the unit
// suite stays hermetic.
//
// Note: until enola is rebuilt and the snapshot regenerated, bundled facts may
// lack the new complexity props, so functions read as O(1) and there may be no
// findings at all — the test asserts only structural invariants, never that
// findings exist.
func TestAnalyzeAgainstBundledSnapshot(t *testing.T) {
	factsPath := filepath.Join("..", "..", ".enola", "facts.jsonl")
	if _, err := os.Stat(factsPath); err != nil {
		t.Skipf("bundled snapshot not found at %s; skipping", factsPath)
	}

	store := facts.NewStore()
	if err := store.ReadJSONLFile(factsPath); err != nil {
		t.Fatalf("loading facts: %v", err)
	}

	funcs, storage, routeHandlers, _ := collect(store)
	if len(funcs) == 0 {
		t.Fatal("expected functions/methods from a real snapshot, got none")
	}

	findings := analyze(funcs, storage, routeHandlers, nil)

	validSeverity := map[string]bool{"high": true, "medium": true, "low": true}
	seen := make(map[string]bool, len(funcs))
	for _, f := range funcs {
		seen[f.Name] = true
	}
	for _, f := range findings {
		if f.BigO == "" {
			t.Errorf("finding for %q has empty big_o", f.Symbol)
		}
		if !validSeverity[f.Severity] {
			t.Errorf("finding for %q has invalid severity %q", f.Symbol, f.Severity)
		}
		if f.Symbol == "" || !seen[f.Symbol] {
			t.Errorf("finding references unknown symbol %q", f.Symbol)
		}
	}

	// Findings must be ranked by severity (high first) — the same invariant the
	// MCP response relies on.
	for i := 1; i < len(findings); i++ {
		if severityRank(findings[i-1].Severity) < severityRank(findings[i].Severity) {
			t.Fatalf("findings not severity-ordered at %d: %q before %q",
				i, findings[i-1].Severity, findings[i].Severity)
		}
	}
}
