// Package finalinherit reports types that derive from a type marked `final` —
// C++'s `class X final`, Java's `final class X`, Swift's `final class X` —
// which is a construct the compiler rejects outright.
//
// Most of what this engine reports is advisory; this is the rare class of
// finding that CAN fail a build: every C++/Java/Swift compiler refuses to
// compile the edge it names, so `enola check --fail-on final-inheritance`
// is safe to gate on the way `cycles` is. It is a parse-level verdict, not a
// type-checker: it reads the `final` prop off the base's symbol fact and the
// implements edge off the derived type's, both emitted deterministically by
// the extractors (docs/extraction/cpp.md, docs/extraction/java.md).
//
// Scope is deliberately type-level only. The method-level rule — overriding
// a `final` member — needs signature matching the fact model does not carry:
// a same-named member in a derived class can be a legal hiding overload, and
// flagging it on the bare name would turn legal code into violations.
package finalinherit

import (
	"context"
	"fmt"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
)

// maxFindings bounds the report: proof-class explainers are uncapped in
// spirit, but a repository with more than this many build breaks has a
// migration, not a finding list — the rollup keeps the report readable and
// the count honest.
const maxFindings = 200

// typeSymbolKinds are the symbol kinds that can *carry* an inheritance list,
// i.e. the kinds the extractors can mark final and can attach implements
// relations to.
func isTypeLike(f facts.Fact) bool {
	switch f.PropString("symbol_kind") {
	case facts.SymbolClass, facts.SymbolStruct, facts.SymbolType:
		return true
	}
	return false
}

type violation struct {
	derived facts.Fact
	base    facts.Fact
}

// Explainer verdicts implements edges whose target symbol fact carries final=true.
type Explainer struct{}

func New() *Explainer { return &Explainer{} }

func (e *Explainer) Name() string { return "final-inheritance" }

func (e *Explainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	symbols := store.ByKind(facts.KindSymbol)

	// The final set is keyed by (repo, name): names are only unique within a
	// repository, and an identically-named type in another repo is not this
	// one's base.
	finalTypes := make(map[facts.FactKey]facts.Fact, len(symbols)/32)
	for _, f := range symbols {
		if f.PropBool("final") && isTypeLike(f) {
			finalTypes[facts.FactKey{Name: f.Name, Repo: f.Repo}] = f
		}
	}
	if len(finalTypes) == 0 {
		return nil, nil
	}

	var seen = make(map[facts.FactKey]bool)
	var violations []violation
	for _, f := range symbols {
		if !isTypeLike(f) {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind != facts.RelImplements {
				continue
			}
			key := facts.FactKey{Name: rel.Target, Repo: f.Repo}
			base, ok := finalTypes[key]
			if !ok || base.Name == f.Name {
				continue // not final, or a self-edge from a broken parse
			}
			pair := facts.FactKey{Name: f.Name + "\x00" + base.Name, Repo: f.Repo}
			if seen[pair] {
				continue
			}
			seen[pair] = true
			violations = append(violations, violation{derived: f, base: base})
		}
	}
	if len(violations) == 0 {
		return nil, nil
	}
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].base.Name != violations[j].base.Name {
			return violations[i].base.Name < violations[j].base.Name
		}
		return violations[i].derived.Name < violations[j].derived.Name
	})

	insights := make([]facts.Insight, 0, min(len(violations), maxFindings))
	for _, v := range violations {
		if len(insights) >= maxFindings {
			break
		}
		insights = append(insights, facts.Insight{
			Title: fmt.Sprintf("Type derives from final type: %s from %s", v.derived.Name, v.base.Name),
			Description: fmt.Sprintf(
				"%q declares an inheritance edge to %q, which carries `final` — the language forbids "+
					"deriving from it, and the compiler rejects this construct outright. Either drop the "+
					"base class or drop `final` from it.",
				v.derived.Name, v.base.Name,
			),
			Confidence: 1.0,
			Evidence: []facts.Evidence{{
				File:   v.derived.File,
				Symbol: v.derived.Name,
				Line:   v.derived.Line,
				Detail: fmt.Sprintf("derives from final %s · %s:%d", v.base.Name, v.base.File, v.base.Line),
			}},
			Actions: []string{
				fmt.Sprintf("Remove `final` from %q, or remove it from %q's inheritance list.", v.base.Name, v.derived.Name),
			},
		})
	}
	return insights, nil
}
