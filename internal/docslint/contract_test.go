package docslint

import (
	"strings"
	"testing"
)

// surface is one place a vocabulary must be enumerated in full.
type surface struct {
	// Doc is the repo-relative page.
	Doc string
	// Section scopes the check to one heading's body. Empty means the whole page.
	// A named section that does not exist fails: a renamed heading must not
	// silently turn the contract into a check over nothing.
	Section string
	// Why records what the reader is there for, quoted back on failure so the
	// person fixing it knows what kind of entry to write.
	Why string
}

// contract binds a live vocabulary to the surfaces that must enumerate it.
type contract struct {
	Inventory string
	Surfaces  []surface
	// Exceptions waives one item on one surface, with a reason. Keyed "doc:item".
	Exceptions map[string]string
}

// contracts is the answer to "represented where it needs to be represented".
//
// Tier B asks whether a NUMBER in the prose matches the code. That catches a stale
// count and nothing else: ARCHITECTURE.md said "sixteen explainers" while describing
// twelve of them, and both halves were consistent with each other. This is the check
// that the enumeration is actually complete.
//
// Only pages that PROMISE completeness are listed. docs/EXPLAINERS.md argues about
// what a finding is worth and names every explainer doing it; the glossary defines the
// names you type. A page that merely mentions a tool in passing is not a catalogue and
// is deliberately absent — adding it here would force unrelated prose to grow an
// enumeration nobody asked it for.
var contracts = []contract{
	{
		Inventory: "explainers",
		Surfaces: []surface{
			{Doc: "ARCHITECTURE.md", Section: "Insights (explainers)",
				Why: "what each one computes, its thresholds, and what it ignores"},
			{Doc: "docs/EXPLAINERS.md",
				Why: "why a derived finding is still not a verdict"},
			{Doc: "docs/GLOSSARY.md",
				Why: "the name as vocabulary — it is what you type in --fail-on"},
			{Doc: "docs/GATING.md", Section: "What fails the build",
				Why: "which names a policy can gate on"},
		},
	},
	{
		Inventory: "fact kinds",
		Surfaces: []surface{
			{Doc: "docs/GRAPH.md", Section: "What's in the graph",
				Why: "what one fact of that kind stands for"},
			{Doc: "docs/schema/facts.md", Section: "Fact kinds",
				Why: "what its name holds and which contract props it carries"},
		},
	},
	{
		Inventory: "relation kinds",
		Surfaces: []surface{
			{Doc: "docs/GRAPH.md", Section: "What's in the graph",
				Why: "what the edge means"},
			{Doc: "docs/schema/facts.md", Section: "Relation kinds",
				Why: "what the edge means, for a consumer loading facts.jsonl"},
		},
	},
	{
		Inventory: "MCP tools",
		Surfaces: []surface{
			{Doc: "ARCHITECTURE.md", Section: "The tools",
				Why: "the question it answers and its full parameter reference"},
			{Doc: "docs/CLI.md",
				Why: "the catalogue a reader without the binary installed can still read"},
		},
	},
}

// TestEveryInventoryIsCompleteOnEverySurface is Tier A.
//
// Each gap it found was invisible to every other check in this repository. The tools
// section documented fifteen of nineteen; the insights section twelve of sixteen; the
// glossary four of sixteen while asserting there were fifteen. Nothing failed, because
// nothing tied a page that enumerates to the thing it enumerates.
func TestEveryInventoryIsCompleteOnEverySurface(t *testing.T) {
	docs := map[string]Doc{}
	for _, d := range corpus(t) {
		docs[d.Path] = d
	}
	byKey := map[string]Inventory{}
	for _, inv := range Inventories() {
		byKey[inv.Key] = inv
	}

	for _, c := range contracts {
		inv, ok := byKey[c.Inventory]
		if !ok {
			t.Errorf("contract names inventory %q, which Inventories() does not provide", c.Inventory)
			continue
		}
		for _, s := range c.Surfaces {
			d, ok := docs[s.Doc]
			if !ok {
				t.Errorf("contract names %s, which is not in the corpus", s.Doc)
				continue
			}
			text := d.Prose
			if s.Section != "" {
				body, found := d.Section(s.Section)
				if !found {
					t.Errorf("%s has no section matching %q — it was renamed, and the "+
						"contract for %s now checks nothing", s.Doc, s.Section, inv.Key)
					continue
				}
				text = body
			}
			lower := strings.ToLower(text)

			for _, item := range inv.Items {
				want := item
				if inv.Quoted {
					want = "`" + item + "`"
				}
				if strings.Contains(lower, strings.ToLower(want)) {
					continue
				}
				if _, waived := c.Exceptions[s.Doc+":"+item]; waived {
					continue
				}
				where := s.Doc
				if s.Section != "" {
					where += " §" + s.Section
				}
				t.Errorf("%s never names %q, which %s ships.\n"+
					"    That surface exists to give the reader: %s\n"+
					"    Add an entry, or waive it in the contract's Exceptions with a reason.",
					where, item, inv.Source, s.Why)
			}
		}
	}
}

// TestNoStaleContractExceptions refuses a waiver for an item now present, and one for
// an item the inventory no longer ships.
func TestNoStaleContractExceptions(t *testing.T) {
	docs := map[string]Doc{}
	for _, d := range corpus(t) {
		docs[d.Path] = d
	}
	byKey := map[string]Inventory{}
	for _, inv := range Inventories() {
		byKey[inv.Key] = inv
	}

	for _, c := range contracts {
		inv := byKey[c.Inventory]
		live := map[string]bool{}
		for _, item := range inv.Items {
			live[item] = true
		}
		for key, why := range c.Exceptions {
			doc, item, ok := strings.Cut(key, ":")
			if !ok {
				t.Errorf("exception key %q is not \"doc:item\"", key)
				continue
			}
			if !live[item] {
				t.Errorf("%s waives %q, which %s no longer ships (%s)", doc, item, inv.Source, why)
				continue
			}
			if d, ok := docs[doc]; ok && strings.Contains(strings.ToLower(d.Prose), strings.ToLower(item)) {
				t.Errorf("%s waives %q (%s), but the page now names it — drop the exception", doc, item, why)
			}
		}
	}
}
