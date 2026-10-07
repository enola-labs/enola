package diff

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
)

func componentFact(name string, where map[string]string) facts.Fact {
	return facts.Fact{Kind: facts.KindIntent, Name: "component: " + name, File: "enola/constraints/concepts.yaml",
		Props: map[string]any{
			"intent_kind": "component",
			"component":   name,
			"where":       intent.EncodeWhere(where),
			"source":      "enola/constraints/concepts.yaml",
		}}
}

func ruleFact(id string, props map[string]any) facts.Fact {
	full := map[string]any{
		"intent_kind": "rule", "rule": id, "because": "the concept is the law's subject",
		"source": "enola/constraints/concepts.yaml",
	}
	for k, v := range props {
		full[k] = v
	}
	return facts.Fact{Kind: facts.KindIntent, Name: "rule: " + id, File: "enola/constraints/concepts.yaml", Props: full}
}

func errorClass(name, superclass string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: "app/errors.rb", Repo: "r",
		Props:     map[string]any{"symbol_kind": facts.SymbolClass, "superclass": superclass},
		Relations: []facts.Relation{{Kind: facts.RelImplements, Target: superclass}}}
}

func constraintInsight(title, symbol string) facts.Insight {
	return facts.Insight{Source: "constraints", Title: title, Confidence: 1.0,
		Evidence: []facts.Evidence{{File: "app/errors.rb", Symbol: symbol, Detail: "name outside the declared convention"}}}
}

// The fail-closed findings cite the COMPONENT, and a component is exactly what
// does not change when the code moves out from under its selector. Grading them
// by evidence entity filed the 1.0 "selector cannot be evaluated" finding as
// incidental — never graded, exit 0 — which is a vacuous pass wearing the shape
// of the guarantee against it.
func TestCompute_ConstraintFindingIsNeverIncidental(t *testing.T) {
	base := snap([]facts.Fact{sym("A", "a.go", 1)}, nil)
	cur := snap([]facts.Fact{sym("A", "a.go", 1), sym("B", "b.go", 1)},
		[]facts.Insight{{
			Source:     "constraints",
			Title:      "Constraint component errors selects on unmeasured property superclas",
			Confidence: 1.0,
			Evidence:   []facts.Evidence{{Fact: "component: errors", Detail: "declared in enola/constraints/concepts.yaml"}},
		}})
	d := Compute(base, cur)
	if len(d.FindingsNew) != 1 {
		t.Fatalf("FindingsNew = %+v, want the constraint finding graded", d.FindingsNew)
	}
	if len(d.FindingsNewIncidental) != 0 {
		t.Errorf("FindingsNewIncidental = %+v, want none: a declared rule has no drifting threshold to be incidental about", d.FindingsNewIncidental)
	}
}

// The reproduction the review ran: pin a baseline with a live breach, then
// change the class's superclass with the declaration byte-identical. The breach
// disappears because the component stopped containing it, and printing that
// under "resolved by this change" reports a rule losing its subject as a win.
func TestCompute_BreachSilencedByAMembershipChangeIsNotResolved(t *testing.T) {
	declaration := []facts.Fact{
		componentFact("errors", map[string]string{"superclass": "StandardError"}),
		ruleFact("errors-are-recognisable", map[string]any{"require_name": "errors", "pattern": "*Error"}),
	}
	breach := constraintInsight("Constraint errors-are-recognisable violated: Failed does not match *Error", "Failed")

	base := snap(append(append([]facts.Fact{}, declaration...), errorClass("Failed", "StandardError")),
		[]facts.Insight{breach})
	cur := snap(append(append([]facts.Fact{}, declaration...), errorClass("Failed", "RuntimeError")), nil)

	d := Compute(base, cur)
	if len(d.FindingsResolved) != 0 {
		t.Errorf("FindingsResolved = %+v, want none: the class left the component, the rule was not satisfied", d.FindingsResolved)
	}
	if len(d.FindingsSilenced) != 1 || d.FindingsSilenced[0].Title != breach.Title {
		t.Fatalf("FindingsSilenced = %+v, want the breach that lost its subject", d.FindingsSilenced)
	}
}

// Deleting the rule and keeping the component. The class still breaches what
// the rule said; the rule stopped saying it. Printing that under "Resolved by
// this change" at exit 0 tells CI a deletion was a fix.
func TestCompute_BreachThatLostItsRuleIsNotResolved(t *testing.T) {
	component := componentFact("errors", map[string]string{"superclass": "StandardError"})
	rule := ruleFact("errors-are-recognisable", map[string]any{"require_name": "errors", "pattern": "*Error"})
	breach := constraintInsight("Constraint errors-are-recognisable violated: Failed does not match *Error", "Failed")
	class := errorClass("Failed", "StandardError")

	base := snap([]facts.Fact{component, rule, class}, []facts.Insight{breach})
	cur := snap([]facts.Fact{component, class}, nil)

	d := Compute(base, cur)
	if len(d.FindingsResolved) != 0 {
		t.Errorf("FindingsResolved = %+v, want none: the rule was deleted, the breaching class is untouched", d.FindingsResolved)
	}
	if len(d.FindingsSilenced) != 0 {
		t.Errorf("FindingsSilenced = %+v, want none: the class never left the component", d.FindingsSilenced)
	}
	if len(d.FindingsUndeclared) != 1 || d.FindingsUndeclared[0].Title != breach.Title {
		t.Fatalf("FindingsUndeclared = %+v, want the breach whose rule is gone", d.FindingsUndeclared)
	}
}

// The same rule id, a different law. `require_name` swapped for a `cap` judges
// something else entirely, and the breach it used to report is not answered by
// the one it reports now — which the gate rendered as PASS, no architectural
// change, one breach resolved.
func TestCompute_BreachWhoseRuleChangedFormIsNotResolved(t *testing.T) {
	component := componentFact("errors", map[string]string{"superclass": "StandardError"})
	before := ruleFact("errors-are-recognisable", map[string]any{"require_name": "errors", "pattern": "*Error"})
	after := ruleFact("errors-are-recognisable", map[string]any{"cap": "errors", "max_members": 100})
	breach := constraintInsight("Constraint errors-are-recognisable violated: Failed does not match *Error", "Failed")
	class := errorClass("Failed", "StandardError")

	d := Compute(
		snap([]facts.Fact{component, before, class}, []facts.Insight{breach}),
		snap([]facts.Fact{component, after, class}, nil))
	if len(d.FindingsResolved) != 0 {
		t.Errorf("FindingsResolved = %+v, want none: the id survived, the law did not", d.FindingsResolved)
	}
	if len(d.FindingsUndeclared) != 1 {
		t.Fatalf("FindingsUndeclared = %+v, want the breach the re-formed rule stopped reporting", d.FindingsUndeclared)
	}
	if d.Empty() {
		t.Error("Empty() = true — a delta that stopped reporting a breach is not no architectural change")
	}
}

// Exempting the witness. The breach is already reported honestly, as an
// exemption; reporting it a second time as resolved says the code was fixed AND
// excused, which cannot both be true.
func TestCompute_BreachSilencedByAnExemptionIsNotResolved(t *testing.T) {
	component := componentFact("errors", map[string]string{"superclass": "StandardError"})
	form := map[string]any{"require_name": "errors", "pattern": "*Error"}
	exempted := map[string]any{"require_name": "errors", "pattern": "*Error",
		"exempt": intent.EncodeExemptions([]intent.ConstraintExemption{{
			Witness: "Failed does not match *Error",
			Owner:   "platform",
			Because: "renaming it is a caller-side change scheduled separately",
			Since:   "2026-08-13",
		}})}
	breach := constraintInsight("Constraint errors-are-recognisable violated: Failed does not match *Error", "Failed")
	class := errorClass("Failed", "StandardError")

	d := Compute(
		snap([]facts.Fact{component, ruleFact("errors-are-recognisable", form), class}, []facts.Insight{breach}),
		snap([]facts.Fact{component, ruleFact("errors-are-recognisable", exempted), class}, nil))
	if len(d.FindingsResolved) != 0 {
		t.Errorf("FindingsResolved = %+v, want none: an excused breach is not a fixed one", d.FindingsResolved)
	}
	if len(d.FindingsUndeclared) != 1 {
		t.Fatalf("FindingsUndeclared = %+v, want the exempted breach", d.FindingsUndeclared)
	}
}

// The other direction, which must keep working: the class stays a member and
// the code is fixed. That is a real resolution and belongs in the good-news
// bucket.
func TestCompute_BreachFixedInsideTheComponentStaysResolved(t *testing.T) {
	declaration := []facts.Fact{
		componentFact("errors", map[string]string{"superclass": "StandardError"}),
		ruleFact("errors-are-recognisable", map[string]any{"require_name": "errors", "pattern": "*Error"}),
	}
	breach := constraintInsight("Constraint errors-are-recognisable violated: Failed does not match *Error", "Failed")

	base := snap(append(append([]facts.Fact{}, declaration...), errorClass("Failed", "StandardError")),
		[]facts.Insight{breach})
	cur := snap(append(append([]facts.Fact{}, declaration...), errorClass("FailedError", "StandardError")), nil)

	d := Compute(base, cur)
	if len(d.FindingsSilenced) != 0 {
		t.Errorf("FindingsSilenced = %+v, want none: the breaching name is gone from the snapshot entirely", d.FindingsSilenced)
	}
	if len(d.FindingsResolved) != 1 {
		t.Fatalf("FindingsResolved = %+v, want the renamed class's breach", d.FindingsResolved)
	}
}

// The same blind spot, one explainer over. An intent finding cites the
// declaration, and a route rename that deletes a claimed cross-repo seam touches
// the routes and the dependency but never the claim. Graded by evidence entity,
// the 1.0 "Claim failed" was incidental and `--fail-on=intent` exited 0 on the
// change that broke the seam.
func TestCompute_IntentFindingIsNeverIncidental(t *testing.T) {
	claim := facts.Fact{Kind: facts.KindIntent, Name: "claim: seam worker -> api via http-client", File: "docs/adr/0001.md",
		Props: map[string]any{"intent_kind": "claim", "metric": "seam"}}
	seam := facts.Fact{Kind: facts.KindDependency, Name: "worker -> api", Repo: "worker",
		Props: map[string]any{"type": "cross_repo"}}
	base := snap([]facts.Fact{claim, seam}, nil)
	cur := snap([]facts.Fact{claim},
		[]facts.Insight{
			{
				Source:     "intent",
				Title:      "Claim failed: claim: seam worker -> api via http-client",
				Confidence: 1.0,
				Evidence:   []facts.Evidence{{Fact: claim.Name, Detail: "claimed in docs/adr/0001.md"}},
			},
			{
				Source:     "intent",
				Title:      "Missing intended seam: worker -> api via http-client",
				Confidence: 0.8,
				Evidence:   []facts.Evidence{{Fact: "consumes api via http-client", Detail: "declared in enola-intent.yaml"}},
			},
		})
	d := Compute(base, cur)
	if len(d.FindingsNew) != 2 {
		t.Fatalf("FindingsNew = %+v, want both intent findings graded", d.FindingsNew)
	}
	if len(d.FindingsNewIncidental) != 0 {
		t.Errorf("FindingsNewIncidental = %+v, want none: a declaration has no drifting threshold to be incidental about", d.FindingsNewIncidental)
	}
}
