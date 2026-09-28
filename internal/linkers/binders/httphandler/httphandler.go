// Package httphandler binds each HTTP route to the symbol that actually serves it.
package httphandler

import (
	"context"
	"log"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// HandlerProp is the prop an extractor sets on a symbol whose SIGNATURE is an HTTP
// handler. It is how a language opts into this binder: emit the prop and the routes
// bind, emit nothing and nothing happens. There is no list of supported languages
// here by design — see the Binder doc.
const HandlerProp = "http_handler"

// Binder resolves each HTTP route to the symbol that serves it, emitting a
// handled_by edge — the same contract grpcimpl provides for gRPC.
//
// WHY IT IS NEEDED. An extractor renders a route's `handler` prop from the
// REGISTRATION site, so it is the receiver VARIABLE chain as written there:
//
//	handler prop : h.settingsAnalyticsHandlerV2.GetActiveRoles
//	real symbol  : internal/adapters/http/settings/analytics.HandlerV2.GetActiveRoles
//
// The receiver variable is not the receiver type, and the registration site is not
// the declaring package, so the two key spaces are disjoint: on fairwayhub/golf, 1397
// distinct handler props intersect 13482 symbol names in exactly TWO places (both
// Python), and 0 of 1641 routes carried a handled_by edge. Every consumer that keys
// off the handler — the performance analyzer's route-handler escalation, orphans'
// rescue of handler methods — was therefore matching nothing.
//
// WHY IT RESOLVES BY SIGNATURE. The only thing the two sides share is the METHOD
// NAME, and a method name alone is ambiguous. fairwayhub/golf has four
// GetDailyWeatherRange: the real handler, a domain service, a mock, and a null-object
// stub in the WIRING package where the routes happen to be registered — so both a
// name-only rule and a package-scoped rule bind the route to the stub. A wrong
// handled_by edge feeds impact_analysis and find_path, which is worse than the
// missing edge it replaces.
//
// A Go HTTP handler is exactly func(http.ResponseWriter, *http.Request). goextractor
// tags those with HandlerProp (v111), which is a structural fact from the parser, not
// a name heuristic — and it rejects the stub (signature `(ctx, string)`) by
// construction. Measured on fairwayhub/golf: 1254 of 1639 Go routes resolve, 212 are
// skipped as ambiguous, 16 are unresolved, 154 are middleware.
//
// WHY THERE IS NO LANGUAGE GATE. This used to test `language == "go"`, which made Go
// support a property of the ENGINE rather than of the Go extractor: adding TypeScript
// or Python meant editing a binder none of those extractors know about. The real
// precondition was never the language, it was the structural signature check that
// HandlerProp represents — a language with no handler-shaped signature emits the prop
// nowhere, indexes nothing, and binds nothing, which is the same outcome the language
// gate produced. Today only goextractor emits it, so dropping the gate changes no
// output; what it changes is who can opt in without touching this file.
//
// It is post-link: routes and handlers are matched strictly within one repo, so it
// neither reads nor feeds cross-repo linking.
type Binder struct{}

// New returns the binder.
func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "http-handler" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	// Index handler symbols by method name, per repo. A method name claimed by two
	// handlers is poisoned: from the registration site alone there is nothing left to
	// tell them apart, and grpcimpl's precedent is to skip rather than guess.
	index := map[string]map[string]string{}   // repo -> method name -> symbol name
	ambiguous := map[string]map[string]bool{} // repo -> method name -> seen twice
	// Types by their package-qualified name, for a handler a routes file names that
	// way (Play's `controllers.Users.list`): repo -> fqn -> type symbol name, "" when
	// two types claim one fqn.
	byFQN := map[string]map[string]string{}

	for _, s := range store.ByKind(facts.KindSymbol) {
		if fqn := s.PropString("fqn"); fqn != "" {
			if byFQN[s.Repo] == nil {
				byFQN[s.Repo] = map[string]string{}
			}
			if prev, seen := byFQN[s.Repo][fqn]; seen && prev != s.Name {
				byFQN[s.Repo][fqn] = ""
			} else {
				byFQN[s.Repo][fqn] = s.Name
			}
		}
		if !s.PropBool(HandlerProp) {
			continue
		}
		method := facts.ShortName(s.Name)
		if method == "" {
			continue
		}
		if index[s.Repo] == nil {
			index[s.Repo] = map[string]string{}
			ambiguous[s.Repo] = map[string]bool{}
		}
		if existing, ok := index[s.Repo][method]; ok && existing != s.Name {
			ambiguous[s.Repo][method] = true
		} else {
			index[s.Repo][method] = s.Name
		}
	}

	exact := resolveExactHandlers(store, byFQN)

	bound := 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindRoute || f.Props == nil {
			return
		}
		switch f.PropString(facts.PropRouteType) {
		case facts.RouteTypeMiddleware:
			// Middleware's "handler" is a middleware func, not an HTTP handler. It
			// carries no HandlerProp so it could not bind anyway, but skip it
			// explicitly: binding middleware to a route would pollute the very edges
			// this exists to make trustworthy.
			return
		case facts.RouteTypeGRPC:
			// A gRPC route is grpcimpl's to bind, and by the time both have run it
			// carries a `handler` prop holding a fully qualified method name whose
			// last segment could collide with an HTTP handler's method name. Without
			// this gate the two binders would not be independent — the result would
			// depend on which ran first, which the Binder contract forbids. The old
			// `language == "go"` test excluded these only by accident, gRPC routes
			// carrying language="grpc".
			return
		}
		if key, _, _ := exactCandidate(*f, byFQN); key != "" && exact[key] != "" {
			t := exact[key]
			if !f.HasRelation(facts.RelHandledBy, t) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: t})
				bound++
			}
			return
		}
		handler := f.PropString("handler")
		if handler == "" {
			return
		}
		method := facts.ShortName(handler)
		if method == "" || ambiguous[f.Repo][method] {
			return
		}
		target := index[f.Repo][method]
		if target == "" {
			return
		}
		if f.HasRelation(facts.RelHandledBy, target) {
			return // idempotent across appends
		}
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
		// Deliberately NOT overwriting Props["handler"]: unlike grpcimpl, which
		// reconstructs an exact FQN, the raw prop here is the literal
		// registration-site expression and is the only record of HOW the route was
		// wired. The resolved symbol lives on the edge.
		bound++
	})
	if bound > 0 {
		log.Printf("[binder:http-handler] bound %d HTTP route(s) to their handler", bound)
	}
	return nil
}

// resolveExactHandlers resolves the routes whose handler names its symbol outright,
// so no signature is needed and the name is not a guess: a package function the
// extractor resolved (PropHandlerTarget), or `<type fqn>.<method>` as a routes file
// writes it. Each resolves only to a symbol carrying exactly the resolved name in the
// route's repo; the result maps exactCandidate's key to that name, "" when none. It
// runs before the update, which holds the store's write lock.
func resolveExactHandlers(store *facts.Store, byFQN map[string]map[string]string) map[string]string {
	exact := map[string]string{}
	for _, f := range store.FactsRef() {
		key, name, wantKind := exactCandidate(f, byFQN)
		if key == "" {
			continue
		}
		if _, done := exact[key]; done {
			continue
		}
		exact[key] = ""
		for _, s := range store.ByName(name) {
			if s.Kind == facts.KindSymbol && s.Repo == f.Repo && s.PropString("symbol_kind") == wantKind {
				exact[key] = name
				break
			}
		}
	}
	return exact
}

// exactCandidate returns the symbol name a route's handler names outright and the
// symbol kind it must have, keyed per repo: the extractor's PropHandlerTarget (a
// function), else a `<type fqn>.<method>` handler whose type a symbol's fqn prop
// names (a method). "" when the route names neither.
func exactCandidate(f facts.Fact, byFQN map[string]map[string]string) (key, name, kind string) {
	if f.Kind != facts.KindRoute {
		return "", "", ""
	}
	if t := f.PropString(facts.PropHandlerTarget); t != "" {
		return f.Repo + "\x00f\x00" + t, t, facts.SymbolFunc
	}
	h := f.PropString("handler")
	dot := strings.LastIndexByte(h, '.')
	if dot <= 0 || dot == len(h)-1 {
		return "", "", ""
	}
	typ := byFQN[f.Repo][h[:dot]]
	if typ == "" {
		return "", "", ""
	}
	name = typ + "." + h[dot+1:]
	return f.Repo + "\x00m\x00" + name, name, facts.SymbolMethod
}
