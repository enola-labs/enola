// Package entrypoints marks the symbols a framework invokes directly, so
// reachability has roots to start from.
//
// Dead code was declined twice on the same measurement: 1,573 of 1,573 routed
// controller actions carry no inbound call edge, because routing is not a call.
// Neither is a queue draining a job, a mailer delivering, or a rake task
// running. Without roots, every one of them looks unreachable.
//
// The roots and the walk from them are the framework-roots binder's; this
// explainer reports them per mechanism and carries the reached share as the
// ceiling on what the graph can see. Reachability was measured before roots
// existed and reported 86% of the monolith's symbols unreachable, which is not
// a finding about the monolith: 53% of the Ruby extractor's call targets are
// bare method names, and a cross-object call cannot be followed until the
// receiver has a type. No dead-code verdict is derived here.
package entrypoints

import (
	"context"
	"fmt"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/binders/frameworkroots"
)

type Explainer struct{}

func New() *Explainer { return &Explainer{} }

func (e *Explainer) Name() string { return "entry-points" }

func (e *Explainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	symbols := map[string]bool{}
	perMechanism := map[string]int{}
	methods, reached, unreached := 0, 0, 0
	for _, fact := range store.ByKind(facts.KindSymbol) {
		symbols[fact.Name] = true
		if mechanism := fact.PropString(frameworkroots.RootProp); mechanism != "" {
			perMechanism[mechanism]++
			continue
		}
		if fact.PropString("language") != "ruby" || fact.PropString("symbol_kind") != facts.SymbolMethod {
			continue
		}
		methods++
		if fact.PropString(frameworkroots.ReachedFromProp) != "" {
			reached++
		} else {
			unreached++
		}
	}
	if len(symbols) == 0 {
		return nil, nil
	}

	_, unmatched := routeRoots(store, symbols)
	total := 0
	for _, n := range perMechanism {
		total += n
	}
	if total == 0 {
		return nil, nil
	}

	evidence := []facts.Evidence{{
		Detail: fmt.Sprintf("%d route handlers name a controller action that is not a known symbol", unmatched),
	}}
	if methods > 0 {
		evidence = append(evidence, facts.Evidence{
			Detail: fmt.Sprintf("%d of %d non-root methods (%.1f%%) are reached from a root; the rest sit behind calls the graph cannot follow (untyped receivers, dynamic dispatch), not behind an absent caller",
				reached, methods, 100*float64(reached)/float64(methods)),
		})
	}
	return []facts.Insight{{
		Title: fmt.Sprintf("%d entry points: %d routed actions, %d jobs, %d mailer actions, %d hooks, %d tasks, %d callbacks",
			total, perMechanism[frameworkroots.MechanismRoute], perMechanism[frameworkroots.MechanismJob], perMechanism[frameworkroots.MechanismMailer],
			perMechanism[frameworkroots.MechanismHook], perMechanism[frameworkroots.MechanismTask], perMechanism[frameworkroots.MechanismCallback]),
		Description: "These symbols are invoked by a framework rather than by a call, so " +
			"they are the roots any reachability question has to start from; each carries " +
			"the mechanism as its root prop, and every symbol a walk from them reaches carries " +
			"reached_from. A dead-code verdict is deliberately not derived from them: a " +
			"cross-object call cannot be followed until its receiver has a type, so the " +
			"unreached share is a ceiling on what the graph can see, not a count of dead code.",
		Confidence: 1.0,
		Evidence:   evidence,
	}}, nil
}

// routeRoots counts route handlers that resolve to a symbol, and those that do
// not. A handler naming a controller the graph never extracted is a gap in
// extraction rather than an entry point, and conflating the two is how a root
// set silently shrinks.
func routeRoots(store *facts.Store, symbols map[string]bool) (int, int) {
	resolved, missing := 0, 0
	for _, fact := range store.ByKind(facts.KindRoute) {
		handler, _ := fact.PropAny("handler").(string)
		controller, action, ok := strings.Cut(handler, "#")
		if !ok || controller == "" {
			continue
		}
		if symbols[controllerClass(controller)+"#"+action] {
			resolved++
			continue
		}
		missing++
	}
	return resolved, missing
}

// controllerClass turns a Rails handler path into the class the graph names:
// `admin/accounts` is `Admin::AccountsController`, not `AdminAccountsController`.
// Getting that wrong resolved 63 of 2,528 handlers instead of 1,538.
func controllerClass(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		parts := strings.Split(segment, "_")
		for j, part := range parts {
			if part == "" {
				continue
			}
			parts[j] = strings.ToUpper(part[:1]) + part[1:]
		}
		segments[i] = strings.Join(parts, "")
	}
	return strings.Join(segments, "::") + "Controller"
}
