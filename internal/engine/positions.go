package engine

import "github.com/enola-labs/enola/internal/facts"

// positionInsights gives every piece of evidence the span of the fact it
// names, so a reader can show the offending line without each explainer
// carrying four more fields to every construction site.
//
// The span is copied, never derived: evidence is positioned only when the
// store holds a fact with that exact name in that exact file and the fact's
// extractor measured a position. Evidence naming no symbol, a symbol the
// store does not hold, or a fact from an extractor with no spans stays
// unpositioned, and the renderer says so by printing no frame.
func positionInsights(insights []facts.Insight, store *facts.Store) {
	if len(insights) == 0 {
		return
	}
	type key struct{ file, name string }
	type span struct{ line, endLine, column, endColumn int }

	// FactsRef, not All: the four fields below are all this needs, and All hands
	// back a deep copy of every fact, Props map and Relations slice included. On a
	// kernel-sized graph that copy was 252 MiB of the run's peak live heap, built
	// to read four ints and dropped. This loop neither retains the slice nor
	// mutates the store, which is the contract FactsRef asks for.
	//
	// The map holds the span for the same reason: a facts.Fact value would keep
	// every positioned fact's props and relations alive for as long as it exists.
	positions := map[key]span{}
	for _, f := range store.FactsRef() {
		if f.Line == 0 || f.Name == "" {
			continue
		}
		k := key{f.File, f.Name}
		if existing, ok := positions[k]; ok && existing.line <= f.Line {
			continue
		}
		positions[k] = span{f.Line, f.EndLine, f.Column, f.EndColumn}
	}
	for i := range insights {
		for j := range insights[i].Evidence {
			ev := &insights[i].Evidence[j]
			if ev.Line != 0 || ev.Symbol == "" {
				continue
			}
			f, ok := positions[key{ev.File, ev.Symbol}]
			if !ok {
				continue
			}
			ev.Line, ev.EndLine, ev.Column, ev.EndColumn = f.line, f.endLine, f.column, f.endColumn
		}
	}
}
