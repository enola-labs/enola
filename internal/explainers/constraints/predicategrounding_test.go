package constraints

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A component declaring BOTH narrowings answered for its whole glob as an edge
// target: the file join read c.match alone and dropped the predicate, so an
// imports edge landing on any file under app/components/** drew a 1.0 verdict —
// including the 41 files in the monolith that host no ViewComponent::Base class
// at all. That is a gating verdict on code the declaration excluded, which is
// the one thing a declaration must never do.
//
// The predicate ANDs in the only way a FILE can carry it: the component
// measured a member there. A file under the glob hosting no member is not in
// the component, exactly as resolveMembership says it is not.
func TestGrounding_APredicateComponentGroundsOnlyOntoFilesItMeasuredAMemberIn(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		predicateComponentIntent("view-components", map[string]string{"superclass": "ViewComponent::Base"},
			map[string]any{"match": "app/components/**"}),
		componentIntent("jobs", "app/jobs/**"),
		formRuleIntent("jobs-avoid-components", map[string]any{
			"forbid": "jobs", "to": "view-components", "via": "imports"}),
		facts.Fact{Kind: facts.KindSymbol, Name: "Hires::Cover", File: "app/components/hires/cover.rb",
			Props: map[string]any{"superclass": "ViewComponent::Base", "symbol_kind": "class"}},
		// Under the glob, and not a member: a plain helper class nobody
		// declared law about.
		facts.Fact{Kind: facts.KindSymbol, Name: "Hires::PlainHelper", File: "app/components/hires/plain_helper.rb",
			Props: map[string]any{"symbol_kind": "class"}},
		facts.Fact{Kind: facts.KindFileRef, Name: "app/components/hires/cover.rb", File: "app/components/hires/cover.rb"},
		facts.Fact{Kind: facts.KindFileRef, Name: "app/components/hires/plain_helper.rb", File: "app/components/hires/plain_helper.rb"},
		// Both imports land under the glob. Only the first lands on a file the
		// component measured a member in, and only the first is the component.
		// The second is what the glob-only join turned into a 1.0 verdict.
		facts.Fact{Kind: facts.KindDependency, Name: "app/jobs -> app/components/hires/cover.rb", File: "app/jobs/render_job.rb",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/components/hires/cover.rb"}}},
		facts.Fact{Kind: facts.KindDependency, Name: "app/jobs -> app/components/hires/plain_helper.rb", File: "app/jobs/sync_job.rb",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/components/hires/plain_helper.rb"}}},
	)
	insights := explain(t, store)
	for _, insight := range insights {
		if strings.Contains(insight.Title, "cannot verdict") {
			t.Fatalf("an imports edge grounds onto a member's file, so the component is reachable and the rule must run: %+v", insight)
		}
	}
	got := violationTitles(insights)
	want := "Constraint jobs-avoid-components violated: app/jobs -> app/components/hires/cover.rb via imports"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("violations = %v, want exactly %q — the helper's file hosts no member, and the declaration excluded it", got, want)
	}
}

// A PATH component's match globs are its whole claim about files, so the file
// join is exactly what the declaration said and must be left alone. Narrowing
// the predicate case must not narrow this one with it.
func TestGrounding_APathComponentStillGroundsOnItsWholeGlob(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("components", "app/components/**"),
		componentIntent("jobs", "app/jobs/**"),
		formRuleIntent("jobs-avoid-components", map[string]any{
			"forbid": "jobs", "to": "components", "via": "imports"}),
		facts.Fact{Kind: facts.KindFileRef, Name: "app/components/hires/plain_helper.rb", File: "app/components/hires/plain_helper.rb"},
		facts.Fact{Kind: facts.KindDependency, Name: "app/jobs -> app/components/hires/plain_helper.rb", File: "app/jobs/sync_job.rb",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/components/hires/plain_helper.rb"}}},
	)
	for _, insight := range explain(t, store) {
		if strings.HasPrefix(insight.Title, "Constraint jobs-avoid-components violated") {
			return
		}
	}
	t.Error("a path component claims every file under its globs, and the import lands on one")
}

// internalFiles marked a whole file internal from one non-exported MEMBER, and
// a membership can be a strict subset of what a file holds — every narrowing on
// a component makes it one. So a file hosting a non-exported member and an
// exported class gated every import of the file, including the imports that
// reached the exported class. The file test reads the snapshot's facts now, not
// the component's members.
func TestPrivate_AFileWithExportedContentIsNotInternal(t *testing.T) {
	build := func(exported bool) *facts.Store {
		s := facts.NewStore()
		s.Add(
			predicateComponentIntent("internals", map[string]string{"symbol_kind": "method"},
				map[string]any{"match": "app/services/**"}),
			componentIntent("callers", "app/controllers/**"),
			formRuleIntent("internals-are-private", map[string]any{"private": "internals"}),
			facts.Fact{Kind: facts.KindSymbol, Name: "Billing::Charge#settle", File: "app/services/billing/charge.rb",
				Props: map[string]any{"symbol_kind": "method", "exported": false}},
			facts.Fact{Kind: facts.KindSymbol, Name: "Billing::Charge", File: "app/services/billing/charge.rb",
				Props: map[string]any{"symbol_kind": "class", "exported": exported}},
			facts.Fact{Kind: facts.KindFileRef, Name: "app/services/billing/charge.rb", File: "app/services/billing/charge.rb"},
			facts.Fact{Kind: facts.KindDependency, Name: "app/controllers -> app/services/billing/charge.rb", File: "app/controllers/billing_controller.rb",
				Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/services/billing/charge.rb"}}},
		)
		return s
	}
	verdicted := func(store *facts.Store) bool {
		for _, insight := range explain(t, store) {
			if strings.HasPrefix(insight.Title, "Constraint internals-are-private violated") {
				return true
			}
		}
		return false
	}
	if verdicted(build(true)) {
		t.Error("the file holds an exported class, so importing it is not a reach into non-exported code")
	}
	if !verdicted(build(false)) {
		t.Error("every fact the snapshot measured in the file is non-exported, so the rule must still verdict")
	}
}
