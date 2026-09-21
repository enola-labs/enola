// Package unmatchedroutes flags routes the cross-repo linker could not pair up.
package unmatchedroutes

import (
	"context"
	"log"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/signals/graphqlsig"
	httpsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/http"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/internal/providers"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Props written by this binder. They are the queryable counterpart to the aggregate
// edge_coverage numbers on a service node: the coverage figure says HOW MANY call
// sites went unresolved, these say WHICH ones and why.
const (
	// PropUnmatchedByClients marks a SERVER route no loaded client calls.
	PropUnmatchedByClients = "unmatched_by_clients"
	// PropMatchedByClients is the positive half. It exists because its absence
	// was mistaken for a negative result: nothing distinguished a route a client
	// calls from one no linker looked at. A route carrying NEITHER marker is that
	// third case, and it is written by leaving both off rather than by a value:
	// the linker declined to reason about it, so it has no verdict to publish.
	PropMatchedByClients = "matched_by_clients"
	// PropUnmatchedByServer marks a CLIENT call site that resolves to no loaded server.
	PropUnmatchedByServer = "unmatched_by_server"
	// PropUnmatchedReason carries one of the crossrepo.Reason* values explaining why.
	PropUnmatchedReason = "unmatched_reason"
)

// Binder records the cross-repo linker's non-matches on the route facts themselves,
// in both directions: a server route no loaded client calls, and a client call site
// that resolves to no loaded server (with the reason it did not).
//
// It is post-link and that is a correctness constraint: it reports the linker's
// verdicts, so running it before linking would report verdicts that had not been
// reached yet.
//
// Writing the verdict as a prop rather than an insight is deliberate. A finding per
// unused endpoint would be noise on a large backend, and the props make the residual
// self-triaging — query_facts(kind=route, prop=unmatched_by_clients) is the whole
// report. It also stays comparable across snapshots for free, since a prop that
// appears or disappears shows up in the snapshot diff.
//
// Both directions CLEAR their props when a route no longer qualifies, which is what
// makes the binder idempotent across appends: loading a second repo can resolve a call
// site that was unmatched when only one repo was in the store, and the stale prop must
// not survive that.
type Binder struct {
	m *routeindex.Matcher
}

// New returns the binder, matching under the given vocabulary.
//
// It takes the vocabulary at construction rather than reading it from the store,
// because its verdicts must be computed with the SAME matching rules the HTTP signal
// used — a binder matching under different vocabulary would report as "unmatched"
// routes the linker had happily linked.
func New(v *vocab.Set) *Binder { return &Binder{m: routeindex.New(v)} }

func (b *Binder) Name() string { return "unmatched-routes" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	// FactsRef, not All: both key builders only read, and both finish before the
	// UpdateWhere below mutates anything. A copy here duplicated the entire fact set —
	// including, since Freeze made sharing load-bearing, every Props map and Relations
	// slice — during linking, on every snapshot.
	all := store.FactsRef()
	evaluatedKeys, serverKeys := httpsignal.ServerRouteVerdicts(b.m, all)
	// GraphQL is a second seam with its own join, and until now its verdict was
	// computed and discarded — the signal kept per-client coverage counters and
	// recorded nothing about whether a DECLARED operation is consumed. The
	// markers are the same two, because "no client calls this endpoint" is the
	// same claim whether the endpoint is a path or a root field.
	evaluatedOps, unmatchedOps := graphqlsig.ServerOperationVerdicts(all)
	clientKeys := httpsignal.UnmatchedClientRouteKeys(b.m, all)

	flaggedServer, flaggedClient := 0, 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindRoute {
			return
		}
		if f.PropString(providers.PropResolutionLevel) == providers.LevelRuntimeObserved {
			f.DelProp(PropMatchedByClients)
			f.DelProp(PropUnmatchedByClients)
			return
		}
		// A client-role route is a call site, never a served endpoint: it carries the
		// reverse (unmatched_by_server) verdict, never unmatched_by_clients.
		if f.Props != nil && f.PropAny(facts.PropRole) == facts.RoleClient {
			f.DelProp(PropUnmatchedByClients)
			if reason, ok := clientKeys[routeindex.RouteIdentity(*f)]; ok {
				f.SetProp(PropUnmatchedByServer, true)
				f.SetProp(PropUnmatchedReason, reason)
				flaggedClient++
			} else {
				f.DelProp(PropUnmatchedByServer)
				f.DelProp(PropUnmatchedReason)
			}
			return
		}
		if f.PropString(facts.PropRouteType) == facts.RouteTypeGraphQL {
			key := graphqlsig.OperationIdentity(*f)
			switch {
			case unmatchedOps[key]:
				f.SetProp(PropUnmatchedByClients, true)
				f.DelProp(PropMatchedByClients)
				flaggedServer++
			case evaluatedOps[key]:
				f.SetProp(PropMatchedByClients, true)
				f.DelProp(PropUnmatchedByClients)
			default:
				f.DelProp(PropMatchedByClients)
				f.DelProp(PropUnmatchedByClients)
			}
			return
		}
		// Per fact, before any identity lookup. Two facts on one path — an Ember
		// page route and the Rails route beneath it — share an identity, so a set
		// membership test alone would hand this one the other's verdict.
		if !b.m.IsLinkable(*f) {
			if f.Props != nil {
				f.DelProp(PropMatchedByClients)
				f.DelProp(PropUnmatchedByClients)
			}
			return
		}
		identity := routeindex.RouteIdentity(*f)
		if serverKeys[identity] {
			if f.Props == nil {
				f.Props = map[string]any{}
			}
			f.SetProp(PropUnmatchedByClients, true)
			// Clearing the opposite marker is what keeps the binder idempotent
			// across appends, and the positive marker had to join the rule the
			// moment it existed: a route matched while one repo was loaded and
			// unmatched once the next append changed the index carried both
			// verdicts at once, on 3,433 routes of the estate.
			f.DelProp(PropMatchedByClients)
			flaggedServer++
			return
		}
		if f.Props == nil {
			return
		}
		if !evaluatedKeys[identity] {
			// The pass declined this one — a UI route, a GraphQL operation, a
			// route with no verb, a generic path, or a route in a repo no loaded
			// client calls at all. It gets no verdict in either direction. The
			// else-branch used to claim all of these as matched, which put 2,997
			// routes nothing had examined into the population every "client
			// coverage is too thin" proportion was measured against.
			f.DelProp(PropMatchedByClients)
			f.DelProp(PropUnmatchedByClients)
			return
		}
		// A matched route says so, rather than merely stopping saying the
		// opposite. Only the unmatched marker existed, so a matched fact and a
		// fact no linker ever evaluated were indistinguishable — and a check for
		// `unmatched_by_clients: false` counted zero matches across 161 real
		// ones, which is absence of a marker read as measurement of absence. The
		// same shape as a coverage zero from an extractor that examined nothing.
		f.SetProp(PropMatchedByClients, true)
		f.DelProp(PropUnmatchedByClients)
	})
	if flaggedServer > 0 || flaggedClient > 0 {
		log.Printf("[binder:unmatched-routes] flagged %d server route(s) unused by clients, %d client call(s) unresolved to a server",
			flaggedServer, flaggedClient)
	}
	return nil
}
