package constraints

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func rubyModule(name string) facts.Fact {
	return facts.Fact{Kind: facts.KindModule, Name: name, File: name, Repo: "app", Props: map[string]any{"language": "ruby"}}
}

func moduleEdge(from, to string, weight int) facts.Fact {
	return facts.Fact{Kind: facts.KindDependency, Name: "module-edge: " + from + " -> " + to, File: from + "/x.rb", Repo: "app",
		Props:     map[string]any{facts.PropCouplingKind: facts.CouplingSymbolRollup, "symbol_edges": weight, "derived": "symbol-rollup"},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: to}}}
}

func cyclesRuleIntent(id, subject string, among []string) facts.Fact {
	return facts.Fact{Kind: facts.KindIntent, Name: "rule: " + id, File: "wiki/p.md",
		Props: map[string]any{"intent_kind": "rule", "rule": id, "forbid_cycles": subject, "among": strings.Join(among, " "),
			"because": "parts that reach each other in a circle cannot be taken apart", "source": "wiki/p.md"}}
}

// Three parts, a circle through two of them, and the third only downstream:
// the rule names the circle and leaves the third alone.
func TestForbidCycles_ReportsACircleAmongParts(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("jobs", "app/jobs/**"),
		componentIntent("models", "app/models/**"),
		componentIntent("mailers", "app/mailers/**"),
		cyclesRuleIntent("parts-never-cycle", "jobs", []string{"models", "mailers"}),
		rubyModule("app/jobs"), rubyModule("app/models"), rubyModule("app/mailers"),
		moduleEdge("app/jobs", "app/models", 35),
		moduleEdge("app/models", "app/jobs", 20),
		moduleEdge("app/models", "app/mailers", 4),
	)
	got, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, i := range got {
		if strings.Contains(i.Title, "parts-never-cycle") {
			titles = append(titles, i.Title)
		}
	}
	if len(titles) != 1 || !strings.Contains(titles[0], "jobs, models depend on each other in a circle") {
		t.Fatalf("want the one circle named, got %v", titles)
	}
	if strings.Contains(titles[0], "mailers") {
		t.Fatalf("mailers sits downstream and is not in the circle: %s", titles[0])
	}
}

// A part spanning two directories whose modules depend on each other is not
// a circle among parts; the graph is contracted to one node per part first.
func TestForbidCycles_ContractsToOneNodePerPart(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("models", "app/models/**"),
		componentIntent("jobs", "app/jobs/**"),
		cyclesRuleIntent("parts-never-cycle", "models", []string{"jobs"}),
		rubyModule("app/models"), rubyModule("app/models/billing"), rubyModule("app/jobs"),
		moduleEdge("app/models", "app/models/billing", 9),
		moduleEdge("app/models/billing", "app/models", 3),
		moduleEdge("app/models", "app/jobs", 2),
	)
	got, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range got {
		if strings.Contains(i.Title, "parts-never-cycle") {
			t.Fatalf("a circle inside one part is not a circle among parts: %s", i.Title)
		}
	}
}

func independentRuleIntent(id, subject string) facts.Fact {
	return facts.Fact{Kind: facts.KindIntent, Name: "rule: " + id, File: "wiki/p.md",
		Props: map[string]any{"intent_kind": "rule", "rule": id, "independent": subject,
			"because": "a mixin that knows its includer is half a class in hiding", "source": "wiki/p.md"}}
}

func rubyMixin(name, file string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Repo: "app", Props: map[string]any{"symbol_kind": "interface", "language": "ruby"}}
}

// The concern reaches the class that includes it, which resolved ancestry
// says; the other concern reaches a class that does not include it, which
// is allowed.
func TestIndependent_NamesAModuleReachingItsIncluder(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		predicateComponentIntent("concerns", map[string]string{"symbol_kind": "interface"}, map[string]any{"match": "app/models/concerns/**"}),
		independentRuleIntent("mixins-stay-independent", "concerns"),
		rubyMixin("Taggable", "app/models/concerns/taggable.rb"),
		rubyMixin("Auditable", "app/models/concerns/auditable.rb"),
		rubyClass("Post", "app/models/post.rb", "ApplicationRecord"),
		rubyClass("Ledger", "app/models/ledger.rb", "ApplicationRecord"),
		resolvedAncestor("Post", "app/models/post.rb", "Taggable", 1),
		resolvedAncestor("Post", "app/models/post.rb", "Auditable", 2),
		facts.Fact{Kind: facts.KindSymbol, Name: "Taggable#tags", File: "app/models/concerns/taggable.rb", Repo: "app",
			Props:     map[string]any{"symbol_kind": "method", "language": "ruby"},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Post.find"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Auditable#audit", File: "app/models/concerns/auditable.rb", Repo: "app",
			Props:     map[string]any{"symbol_kind": "method", "language": "ruby"},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Ledger.record"}}},
	)
	got, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, i := range got {
		if strings.Contains(i.Title, "mixins-stay-independent") {
			titles = append(titles, i.Title)
		}
	}
	if len(titles) != 1 || !strings.Contains(titles[0], "Taggable reaches its includer Post") {
		t.Fatalf("want Taggable named and Auditable left alone, got %v", titles)
	}
}

// Without resolved ancestry the rule cannot know who includes whom, so it
// refuses in one advisory and verdicts nothing.
func TestIndependent_RefusedWithoutResolvedAncestry(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		predicateComponentIntent("concerns", map[string]string{"symbol_kind": "interface"}, map[string]any{"match": "app/models/concerns/**"}),
		independentRuleIntent("mixins-stay-independent", "concerns"),
		rubyMixin("Taggable", "app/models/concerns/taggable.rb"),
		rubyClass("Post", "app/models/post.rb", "ApplicationRecord"),
		facts.Fact{Kind: facts.KindSymbol, Name: "Taggable#tags", File: "app/models/concerns/taggable.rb", Repo: "app",
			Props:     map[string]any{"symbol_kind": "method", "language": "ruby"},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Post.find"}}},
	)
	got, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var refused bool
	for _, i := range got {
		if strings.Contains(i.Title, "mixins-stay-independent violated") {
			t.Fatalf("no verdict without resolved ancestry, got %s", i.Title)
		}
		if strings.Contains(i.Title, "cannot be evaluated: no resolved ancestry") {
			refused = true
		}
	}
	if !refused {
		t.Fatal("the refusal must surface as a finding")
	}
}

// any_of: a class satisfies the protocol by defining either name; the one
// defining neither is the breach and the finding names the whole list.
func TestRequireDefines_AnyOfAcceptsEither(t *testing.T) {
	store := facts.NewStore()
	cls := func(name, method string) []facts.Fact {
		out := []facts.Fact{{Kind: facts.KindSymbol, Name: name, File: "app/services/" + strings.ToLower(name) + ".rb", Repo: "app", Props: map[string]any{"symbol_kind": "class", "language": "ruby"}}}
		if method != "" {
			out = append(out, facts.Fact{Kind: facts.KindSymbol, Name: name + "#" + method, File: "app/services/" + strings.ToLower(name) + ".rb", Repo: "app", Props: map[string]any{"symbol_kind": "method", "language": "ruby"}})
		}
		return out
	}
	store.Add(componentIntent("services", "app/services/**"))
	store.Add(facts.Fact{Kind: facts.KindIntent, Name: "rule: entry-point", File: "wiki/p.md",
		Props: map[string]any{"intent_kind": "rule", "rule": "entry-point", "require_defines": "services", "any_of": "call run", "because": "a service answers to one of two doors", "source": "wiki/p.md"}})
	store.Add(cls("Charge", "call")...)
	store.Add(cls("Refund", "run")...)
	store.Add(cls("Reconcile", "")...)
	got, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, i := range got {
		if strings.Contains(i.Title, "entry-point") {
			titles = append(titles, i.Title)
		}
	}
	if len(titles) != 1 || !strings.Contains(titles[0], "Reconcile does not define any of call, run") {
		t.Fatalf("want only Reconcile named with the list, got %v", titles)
	}
}

// A bare-method literal matches the method at the end of a chain; a
// receiver-qualified literal stays exact.
func TestForbidToName_BareMethodMatchesTheChainedCall(t *testing.T) {
	if !matchesAnyBoundedName("where.update_all", []string{"update_all"}) {
		t.Fatal("update_all must match where.update_all")
	}
	if !matchesAnyBoundedName("Order.update_all", []string{"update_all"}) {
		t.Fatal("update_all must match Order.update_all")
	}
	if matchesAnyBoundedName("where.update_all", []string{"Order.update_all"}) {
		t.Fatal("a receiver-qualified literal stays exact")
	}
	if matchesAnyBoundedName("where.update_all_later", []string{"update_all"}) {
		t.Fatal("the method must match whole, not as a prefix")
	}
}

// The parts of a circle are listed in name order, and the edges that close it need
// not join neighbours in that order: here the circle runs alpha -> gamma -> beta ->
// delta -> alpha, and its lightest edge, alpha -> gamma, joins the first and third.
func TestForbidCycles_CutIsAnEdgeOfTheCircle(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("alpha", "app/alpha/**"),
		componentIntent("beta", "app/beta/**"),
		componentIntent("gamma", "app/gamma/**"),
		componentIntent("delta", "app/delta/**"),
		cyclesRuleIntent("parts-never-cycle", "alpha", []string{"beta", "gamma", "delta"}),
		rubyModule("app/alpha"), rubyModule("app/beta"), rubyModule("app/gamma"), rubyModule("app/gamma/x"),
		rubyModule("app/delta"), rubyModule("app/delta/x"),
		moduleEdge("app/alpha", "app/gamma", 1),
		moduleEdge("app/gamma", "app/beta", 1),
		moduleEdge("app/gamma/x", "app/beta", 1),
		moduleEdge("app/beta", "app/delta", 1),
		moduleEdge("app/beta", "app/delta/x", 1),
		moduleEdge("app/delta", "app/alpha", 1),
		moduleEdge("app/delta/x", "app/alpha", 1),
	)
	got, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range got {
		if !strings.Contains(i.Title, "parts-never-cycle") {
			continue
		}
		if len(i.Actions) == 0 || !strings.Contains(i.Actions[0], "alpha -> gamma") {
			t.Errorf("cut = %v, want the lightest edge of the circle, alpha -> gamma", i.Actions)
		}
		reaches := false
		for _, e := range i.Evidence {
			reaches = reaches || e.Detail == "alpha reaches gamma"
		}
		if !reaches {
			t.Errorf("evidence = %v, want the alpha -> gamma edge among it", i.Evidence)
		}
		return
	}
	t.Fatal("the circle was not reported")
}
