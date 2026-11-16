package orphans

import "github.com/enola-labs/enola/internal/facts"

// Detect is the annotate pass's orphan list for a store, for tests.
func Detect(store *facts.Store) []Orphan {
	syms, refSources := collect(store)
	return detectFrom(syms, refSources)
}
