package server

import (
	"github.com/enola-labs/enola/internal/facts"
)

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// Both ask the facts which labels and file shapes the loaded graph has, so they
// need the store even though they resolve no name.
func (s *Server) maybePrefixRepoLabel(input string) string {
	return s.resolver(s.loadedStore()).PrefixRepoLabel(input)
}

func renderQuerySummary(results []facts.Fact, total int) string {
	q := newQuerySummary()
	for i := range results {
		q.add(&results[i])
	}
	return q.render(total)
}
