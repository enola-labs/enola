package dashboard

import (
	"github.com/enola-labs/enola/internal/facts"
)

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// buildModuleGraph produces a deliberately bounded architecture map. It ranks
// production modules by connectedness, keeps the most structurally relevant
// ones, and renders only imports whose endpoints are both visible.
func buildModuleGraph(store *facts.Store) *moduleGraphView {
	return buildModuleGraphFocused(store, "")
}
