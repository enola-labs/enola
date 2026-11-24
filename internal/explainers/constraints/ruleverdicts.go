package constraints

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// RuleVerdict is one declared rule and what it reports against a store: the
// breaches the explainer would raise for it now, before any ratchet decides
// which of them are new.
type RuleVerdict struct {
	ID, Mode, Because, Source string
	// Components names every component the rule reads, so a caller can say when
	// a rule holds only because one of them selects nothing.
	Components []string
	// Breaches are the rule's violation findings. Exempted breaches are left out
	// and counted in Exempted: an exemption is a decision already made.
	Breaches []facts.Insight
	Exempted int
	// NotComputed carries the cause when the rule's evaluation failed; such a
	// rule has no verdict, which is not the same as holding.
	NotComputed string
}

// RuleVerdicts evaluates every declared rule in store and returns one entry per
// rule, in the order the explainer reads them. It is the verdict pass the explainer runs, read per
// rule, so a declaration can be previewed against a snapshot without being
// compiled into one: `constraints lint` uses it to say what a draft rule would
// report today.
func RuleVerdicts(store *facts.Store) []RuleVerdict {
	ev := New().evaluate(store, evaluation{guard: true})
	notComputed := map[string]string{}
	for _, n := range ev.notComputed {
		notComputed[n.Rule] = n.Cause
	}
	out := make([]RuleVerdict, 0, len(ev.rules))
	for _, r := range ev.rules {
		v := RuleVerdict{ID: r.id, Mode: r.mode, Because: r.because, Source: r.source, Components: r.componentNames(), NotComputed: notComputed[r.id]}
		for _, in := range ev.byRule[r.id] {
			if RuleIDFromTitle(in.Title) == r.id {
				v.Breaches = append(v.Breaches, in)
			} else if strings.HasPrefix(in.Title, "Exempted from constraint "+r.id+": ") {
				v.Exempted++
			}
		}
		out = append(out, v)
	}
	return out
}
