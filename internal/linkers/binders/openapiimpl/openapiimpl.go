// Package openapiimpl binds each OpenAPI server operation to the method that implements it.
package openapiimpl

import (
	"context"
	"log"
	"sort"
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Binder connects each server operation read from an OpenAPI spec to the method that
// implements it, the contract grpcimpl provides for a .proto.
//
// A spec-first service registers its routes in generated code: oapi-codegen writes a
// ServerInterface (or StrictServerInterface) with one method per operation, named after
// the operationId, plus wrappers that call it. The spec route and the hand-written
// implementation are extracted by different extractors and share nothing but that
// name, so without this binder every operation of such a service stops at the route.
//
// The implementation is found by method set, not by an implements edge: Go satisfies
// an interface implicitly, and the type implementing it is usually handed to the
// generated constructor in another package. For each spec, the receiver type whose
// methods name the most of the spec's operations wins, if it implements more than half
// of them; generated methods (the interface, its wrappers, mocks) carry generated=true
// and are never candidates. A tie binds nothing, as in grpcimpl: a wrong handled_by
// edge is worse than a missing one.
//
// It is post-link: routes and implementations are matched strictly within one repo.
type Binder struct{}

// New returns the binder.
func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "openapi-impl" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

type operation struct {
	repo, spec, id string
}

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	// Hand-written methods per repo, by short name, each with its owner type.
	type method struct{ name, owner string }
	methods := map[string]map[string][]method{} // repo -> short name -> methods
	for _, s := range store.ByKind(facts.KindSymbol) {
		if s.PropString("symbol_kind") != facts.SymbolMethod || s.PropBool("generated") {
			continue
		}
		short := facts.ShortName(s.Name)
		owner := strings.TrimRight(strings.TrimSuffix(s.Name, short), ".#:")
		if short == "" || owner == "" || owner == s.Name {
			continue
		}
		if methods[s.Repo] == nil {
			methods[s.Repo] = map[string][]method{}
		}
		methods[s.Repo][short] = append(methods[s.Repo][short], method{s.Name, owner})
	}

	// Server operations per repo and spec.
	specs := map[[2]string][]string{} // {repo, spec} -> operationIds
	for _, f := range store.ByKind(facts.KindRoute) {
		if op, ok := serverOperation(f); ok && methods[op.repo] != nil {
			key := [2]string{op.repo, op.spec}
			specs[key] = append(specs[key], op.id)
		}
	}

	// lookup returns the methods an operationId names: oapi-codegen's PascalCase
	// spelling, else the operationId as written.
	lookup := func(repo, id string) []method {
		if ms := methods[repo][pascal(id)]; len(ms) > 0 {
			return ms
		}
		return methods[repo][id]
	}

	targets := map[operation]string{}
	for key, ids := range specs {
		// Score each owner type by how many of the spec's operations it implements.
		score := map[string]int{}
		for _, id := range ids {
			seen := map[string]bool{}
			for _, m := range lookup(key[0], id) {
				if !seen[m.owner] {
					seen[m.owner] = true
					score[m.owner]++
				}
			}
		}
		best, tie := "", false
		for _, owner := range sortedKeys(score) {
			switch {
			case best == "" || score[owner] > score[best]:
				best, tie = owner, false
			case score[owner] == score[best]:
				tie = true
			}
		}
		// A type implementing only a minority of the spec does not serve it: a spec
		// the repo calls shares its health checks with the repo's own server.
		if best == "" || tie || 2*score[best] <= len(ids) {
			continue
		}
		for _, id := range ids {
			for _, m := range lookup(key[0], id) {
				if m.owner == best {
					targets[operation{key[0], key[1], id}] = m.name
				}
			}
		}
	}

	bound := 0
	store.UpdateWhere(func(f *facts.Fact) {
		op, ok := serverOperation(*f)
		if !ok {
			return
		}
		target := targets[op]
		if target == "" {
			return
		}
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
		f.SetProp("handler", target)
		bound++
	})
	if bound > 0 {
		log.Printf("[binder:openapi-impl] bound %d OpenAPI operation(s) to their implementation", bound)
	}
	return nil
}

// serverOperation reports the operation a fact declares when it is a served OpenAPI
// route with an operationId and no handler yet. A route that already has one, from
// another binder or from an earlier run of this one, is left as it is.
func serverOperation(f facts.Fact) (operation, bool) {
	if f.Kind != facts.KindRoute || f.Props == nil {
		return operation{}, false
	}
	if f.PropString(facts.PropSource) != facts.RouteSourceOpenAPI || f.PropString(facts.PropRole) != facts.RoleServer {
		return operation{}, false
	}
	id := f.PropString("operationId")
	if id == "" {
		return operation{}, false
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelHandledBy {
			return operation{}, false
		}
	}
	return operation{f.Repo, f.PropString("spec_file"), id}, true
}

// pascal spells an operationId the way oapi-codegen names its method: separators
// dropped and the letter after each one, and the first, upper-cased.
func pascal(id string) string {
	var b strings.Builder
	up := true
	for _, r := range id {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			up = true
			continue
		}
		if up {
			r = unicode.ToUpper(r)
			up = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
