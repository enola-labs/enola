package metrics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestCollectAgainstBundledSnapshot is an integration smoke test that runs the
// store adapter against the repo's bundled .enola/facts.jsonl  It is skipped when the snapshot is absent so the unit
// suite stays hermetic.
func TestCollectAgainstBundledSnapshot(t *testing.T) {
	factsPath := filepath.Join("..", "..", ".enola", "facts.jsonl")
	if _, err := os.Stat(factsPath); err != nil {
		t.Skipf("bundled snapshot not found at %s; skipping", factsPath)
	}

	// Load with defaults (config path is intentionally absent -> defaults).
	store := facts.NewStore()
	if err := store.ReadJSONLFile(factsPath); err != nil {
		t.Fatalf("loading facts: %v", err)
	}

	pkgs, edges, _ := collect(store)
	if len(pkgs) == 0 {
		t.Fatal("expected packages from a real snapshot, got none")
	}
	results := compute(pkgs, edges)

	// Invariant: every directed import edge contributes 1 to some package's Ce
	// and 1 to another's Ca, so the totals must match.
	var totalCa, totalCe int
	for _, m := range results {
		totalCa += m.Ca
		totalCe += m.Ce
		if m.Instability < 0 || m.Instability > 1 {
			t.Errorf("%s instability out of range: %v", m.Package, m.Instability)
		}
		if m.Abstractness < 0 || m.Abstractness > 1 {
			t.Errorf("%s abstractness out of range: %v", m.Package, m.Abstractness)
		}
	}
	if totalCa != totalCe {
		t.Errorf("sum(Ca)=%d != sum(Ce)=%d (edge accounting invariant broken)", totalCa, totalCe)
	}
	t.Logf("snapshot: %d packages, total directed package deps=%d", len(results), totalCe)
}
