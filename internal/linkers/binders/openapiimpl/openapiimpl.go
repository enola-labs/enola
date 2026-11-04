// Package openapiimpl binds each OpenAPI server operation to the method that implements it.
package openapiimpl

import (
	"context"
	"log"
	"sort"
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/binders/vendoredspecs"
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
// A spec read as served that the repository does not implement is one it calls. Where
// a repository implements its specs by operation, as it does when any of them binds
// above, a spec of which not one operation is implemented — by a method or by a route
// the code registers — is not served by it, and is made a client spec. A repository
// whose specs are implemented some other way, documenting routes its framework
// registers, is not judged by a rule it does not follow.
//
// It is pre-link, after vendored-specs: the roles it settles are what the cross-repo
// linker matches on. Routes and implementations are matched strictly within one repo.
type Binder struct{}

// Values of the handler_binding prop: how an operation's handler was found.
const (
	PropHandlerBinding = "handler_binding"
	// BindingOperation: a hand-written method named for the operation.
	BindingOperation = "operation"
	// BindingCodeRoute: the handler of the route the code registers at the path.
	BindingCodeRoute = "code-route"
)

// New returns the binder.
func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "openapi-impl" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePreLink }

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
		if op, ok := serverOperation(f); ok && op.id != "" && methods[op.repo] != nil {
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

	// A spec documenting routes the code registers (a Rails app writing its API
	// down per package) is served by the code route's handler.
	codeHandlers := map[string]string{} // repo\x00METHOD path -> handler, "" when two disagree
	// The handler an extractor named but left to a later binder to confirm
	// (handler_target) counts once the symbol it names exists: this binder runs
	// before that one, and a Go route registered through a helper has nothing else.
	declared := map[string]bool{} // repo\x00symbol
	for _, s := range store.ByKind(facts.KindSymbol) {
		declared[s.Repo+"\x00"+s.Name] = true
	}
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.PropString(facts.PropSource) == facts.RouteSourceOpenAPI || f.PropString(facts.PropRole) == facts.RoleClient {
			continue
		}
		var targets []string
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				targets = append(targets, r.Target)
			}
		}
		if t := f.PropString(facts.PropHandlerTarget); len(targets) == 0 && t != "" && declared[f.Repo+"\x00"+t] {
			targets = append(targets, t)
		}
		for _, target := range targets {
			k := f.Repo + "\x00" + endpointKey(f.PropString("method"), f.Name)
			if prev, seen := codeHandlers[k]; seen && prev != target {
				codeHandlers[k] = ""
			} else {
				codeHandlers[k] = target
			}
		}
	}

	bound := 0
	store.UpdateWhere(func(f *facts.Fact) {
		op, ok := serverOperation(*f)
		if !ok {
			return
		}
		target, how := targets[op], BindingOperation
		if target == "" {
			target, how = codeRouteHandler(*f, codeHandlers), BindingCodeRoute
		}
		if target == "" {
			return
		}
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
		f.SetProp("handler", target)
		f.SetProp(PropHandlerBinding, how)
		bound++
	})
	demoted := demoteUnimplemented(store)
	if bound > 0 || demoted > 0 {
		log.Printf("[binder:openapi-impl] bound %d OpenAPI operation(s) to their implementation; %d operation(s) of unimplemented specs made client", bound, demoted)
	}
	return nil
}

// demoteUnimplemented makes a client spec of every served spec of which no operation
// is implemented, in the repositories that implement their specs by operation.
func demoteUnimplemented(store *facts.Store) int {
	implementsByOperation := map[string]bool{}
	specBound := map[[2]string]bool{}
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.PropString(facts.PropSource) != facts.RouteSourceOpenAPI || f.PropString(facts.PropRole) != facts.RoleServer {
			continue
		}
		if f.PropString(PropHandlerBinding) == BindingOperation {
			implementsByOperation[f.Repo] = true
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				specBound[[2]string{f.Repo, f.PropString("spec_file")}] = true
			}
		}
	}
	demoted := 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindRoute || f.PropString(facts.PropSource) != facts.RouteSourceOpenAPI ||
			f.PropString(facts.PropRole) != facts.RoleServer || !implementsByOperation[f.Repo] {
			return
		}
		if specBound[[2]string{f.Repo, f.PropString("spec_file")}] || f.PropString("spec_file") == "" {
			return
		}
		f.SetProp(facts.PropRole, facts.RoleClient)
		f.SetProp(vendoredspecs.PropVendoredSpec, true)
		demoted++
	})
	return demoted
}

// serverOperation reports the operation a fact declares when it is a served OpenAPI
// route with no handler yet; its id is empty when the spec gives none. A route that already has one, from
// another binder or from an earlier run of this one, is left as it is.
func serverOperation(f facts.Fact) (operation, bool) {
	if f.Kind != facts.KindRoute || f.Props == nil {
		return operation{}, false
	}
	if f.PropString(facts.PropSource) != facts.RouteSourceOpenAPI || f.PropString(facts.PropRole) != facts.RoleServer {
		return operation{}, false
	}
	// An operation without an operationId has no method to be named after, but a
	// spec that documents routes the code registers is bound by method and path,
	// which needs no id.
	id := f.PropString("operationId")
	for _, r := range f.Relations {
		if r.Kind == facts.RelHandledBy {
			return operation{}, false
		}
	}
	return operation{f.Repo, f.PropString("spec_file"), id}, true
}

// endpointKey spells a verb and path the same whichever router wrote them: every
// path parameter, `{id}` or `:id`, is `{}`, and a trailing slash is dropped.
func endpointKey(method, p string) string {
	segs := strings.Split(strings.TrimSuffix(p, "/"), "/")
	for i, seg := range segs {
		if strings.HasPrefix(seg, ":") || strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			segs[i] = "{}"
		}
	}
	return strings.ToUpper(method) + " " + strings.Join(segs, "/")
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

// codeRouteHandler is the handler of the route the code registers where a spec
// operation is served. The path the spec writes is tried as written, then under the
// spec's server base (`servers: [{url: …/stores/v1}]`), then as the tail of exactly one
// code route of the repo with that method: a spec written relative to a version or
// mount prefix the code adds (/tickets for /api/v1/tickets). Two code routes ending
// in that tail bind nothing.
func codeRouteHandler(f facts.Fact, codeHandlers map[string]string) string {
	method := f.PropString("method")
	if h := codeHandlers[f.Repo+"\x00"+endpointKey(method, f.Name)]; h != "" {
		return h
	}
	if base := f.PropString("server_base"); base != "" {
		if h := codeHandlers[f.Repo+"\x00"+endpointKey(method, base+f.Name)]; h != "" {
			return h
		}
	}
	key := endpointKey(method, f.Name)
	verb, tail, _ := strings.Cut(key, " ")
	if strings.Count(tail, "/") < 1 || tail == "/" {
		return ""
	}
	found := ""
	prefix := f.Repo + "\x00" + verb + " "
	for k, h := range codeHandlers {
		if h == "" || !strings.HasPrefix(k, prefix) {
			continue
		}
		path := strings.TrimPrefix(k, prefix)
		if path == tail || !strings.HasSuffix(path, tail) {
			continue
		}
		if found != "" && found != h {
			return ""
		}
		found = h
	}
	return found
}
