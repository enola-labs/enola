// Package unmatchedroutes flags routes the cross-repo linker could not pair up.
package unmatchedroutes

import (
	"context"
	"log"
	"path"
	"strings"

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
	// PropUnreachedByClients marks a SERVER route that loaded clients call only from
	// code the application cannot run: every matching call site carries
	// facts.PropClientReachable false. It sits beside PropMatchedByClients, which
	// still holds, because the call is in the source; this says it is never sent.
	PropUnreachedByClients = "unreached_by_clients"
	// PropUnmatchedByServer marks a CLIENT call site that resolves to no loaded server.
	PropUnmatchedByServer = facts.PropUnmatchedByServer
	// PropUnmatchedReason carries one of the crossrepo.Reason* values explaining why.
	PropUnmatchedReason = facts.PropUnmatchedReason
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
	verdicts := httpsignal.ServerRouteVerdictsWithTraffic(b.m, all)
	evaluatedKeys, serverKeys, unreachedKeys := verdicts.Evaluated, verdicts.Unmatched, verdicts.Unreached
	// GraphQL is a second seam with its own join, and until now its verdict was
	// computed and discarded — the signal kept per-client coverage counters and
	// recorded nothing about whether a DECLARED operation is consumed. The
	// markers are the same two, because "no client calls this endpoint" is the
	// same claim whether the endpoint is a path or a root field.
	evaluatedOps, unmatchedOps := graphqlsig.ServerOperationVerdicts(all)
	gqlReasons := graphqlUnresolvedReasons(all)
	clientKeys := httpsignal.UnmatchedClientRouteKeys(b.m, all)
	clientMatches := httpsignal.ClientRouteMatches(b.m, all)

	flaggedServer, flaggedClient, linkedClient, unreachedServer := 0, 0, 0, 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindRoute {
			return
		}
		if f.PropString(providers.PropResolutionLevel) == providers.LevelRuntimeObserved {
			f.DelProp(PropMatchedByClients)
			f.DelProp(PropUnmatchedByClients)
			f.DelProp(PropUnreachedByClients)
			f.DelProp(facts.PropMatchedRoutes)
			return
		}
		// A client-role route is a call site, never a served endpoint: it carries the
		// reverse (unmatched_by_server) verdict, never unmatched_by_clients.
		if f.Props != nil && f.PropAny(facts.PropRole) == facts.RoleClient {
			f.DelProp(PropUnmatchedByClients)
			f.DelProp(PropUnreachedByClients)
			if f.PropString(facts.PropRouteType) == facts.RouteTypeGraphQL {
				if reason := gqlReasons[graphqlsig.OperationIdentity(*f)+"\x00"+f.Repo+"\x00"+f.File]; reason != "" {
					f.SetProp(PropGraphQLUnresolvedReason, reason)
				} else {
					f.DelProp(PropGraphQLUnresolvedReason)
				}
			}
			// The positive verdict, and the half a consumer walks: which server
			// routes this call reaches. Cleared when the call no longer resolves,
			// for the same idempotency across appends as the markers.
			if matches := clientMatches[httpsignal.ClientCallKey(*f)]; len(matches) > 0 {
				f.SetProp(facts.PropMatchedRoutes, matchedRoutesProp(matches))
				linkedClient++
			} else {
				f.DelProp(facts.PropMatchedRoutes)
			}
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
				f.DelProp(PropUnreachedByClients)
				setTraffic(f, httpsignal.ClientTraffic{})
			}
			return
		}
		identity := routeindex.RouteIdentity(*f)
		if serverKeys[identity] {
			if f.Props == nil {
				f.Props = map[string]any{}
			}
			f.SetProp(PropUnmatchedByClients, true)
			f.DelProp(PropUnreachedByClients)
			// Clearing the opposite marker is what keeps the binder idempotent
			// across appends, and the positive marker had to join the rule the
			// moment it existed: a route matched while one repo was loaded and
			// unmatched once the next append changed the index carried both
			// verdicts at once, on 3,433 routes of the estate.
			f.DelProp(PropMatchedByClients)
			setTraffic(f, httpsignal.ClientTraffic{})
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
			f.DelProp(PropUnreachedByClients)
			setTraffic(f, httpsignal.ClientTraffic{})
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
		if unreachedKeys[identity] {
			f.SetProp(PropUnreachedByClients, true)
			unreachedServer++
		} else {
			f.DelProp(PropUnreachedByClients)
		}
		setTraffic(f, verdicts.Traffic[identity])
	})
	if flaggedServer > 0 || flaggedClient > 0 || linkedClient > 0 {
		log.Printf("[binder:unmatched-routes] flagged %d server route(s) unused by clients, %d called only from client code that never runs, %d client call(s) unresolved to a server; linked %d client call(s) to their server routes",
			flaggedServer, unreachedServer, flaggedClient, linkedClient)
	}
	return nil
}

// matchedRoutesProp renders a call's matches as the prop value: a []any of
// map[string]any, the shape the value has after a round trip through facts.jsonl, so
// a fact built here and one read back from disk are the same to every reader.
// setTraffic writes, on a server route, the strongest trigger among the client calls
// that match it, and clears what an earlier pass wrote when there is none, so the binder
// stays idempotent across appends.
func setTraffic(f *facts.Fact, t httpsignal.ClientTraffic) {
	for _, p := range []string{facts.PropClientTrigger, facts.PropClientEveryPage, facts.PropClientPollMs, facts.PropClientTriggerFrom} {
		f.DelProp(p)
	}
	if t.Trigger == "" {
		return
	}
	f.SetProp(facts.PropClientTrigger, t.Trigger)
	if t.EveryPage {
		f.SetProp(facts.PropClientEveryPage, true)
	}
	if t.PollMs > 0 {
		f.SetProp(facts.PropClientPollMs, t.PollMs)
	}
	if t.Caller != "" {
		f.SetProp(facts.PropClientTriggerFrom, t.Caller)
	}
}

func matchedRoutesProp(matches []httpsignal.RouteMatch) []any {
	out := make([]any, 0, len(matches))
	for _, m := range matches {
		out = append(out, map[string]any{
			"repo":       m.Repo,
			"method":     m.Method,
			"name":       m.Name,
			"file":       m.File,
			"confidence": m.Confidence,
		})
	}
	return out
}

// PropGraphQLUnresolvedReason says why no loaded schema answers a GraphQL client
// operation. It is not unmatched_by_server: that verdict, and the coverage counts
// read from it, belong to the HTTP linker.
const PropGraphQLUnresolvedReason = "unresolved_reason"

// Reasons for PropGraphQLUnresolvedReason.
const (
	// GraphQLNoSchemaInRepo: nothing in the operation's package talks to a schema of
	// this repository; its server is elsewhere (a hosted API, a repository not loaded).
	GraphQLNoSchemaInRepo = "no-server-schema-in-repo"
	// GraphQLFieldNotFound: the package does use a schema of this repository, and
	// no root field enola read from it answers the operation. It says what was read,
	// not that the field is absent: a schema declared in a form no extractor reads
	// (graphql-ruby's mount_mutation, a stitched remote) lands here too.
	GraphQLFieldNotFound = "field-not-found"
)

// graphqlUnresolvedReasons decides, for every client operation no loaded schema
// declares, why. Its server is the one its package talks to: if the package
// declares fields itself, or another of its operations is answered by this
// repository, no field read from that schema answers it; otherwise that schema is
// not in this repository. A package is the nearest directory with a package.json. Keyed
// by operation identity, repo and file.
func graphqlUnresolvedReasons(all []facts.Fact) map[string]string {
	served := map[string]bool{}
	pkgDirs := map[string][]string{}
	for _, f := range all {
		if path.Base(f.File) == "package.json" {
			pkgDirs[f.Repo] = append(pkgDirs[f.Repo], path.Dir(f.File))
		}
		if isGraphQLRole(f, facts.RoleServer) {
			served[f.Name] = true
		}
	}
	packageOf := func(repo, file string) string {
		best := ""
		for _, d := range pkgDirs[repo] {
			if d != "." && strings.HasPrefix(file, d+"/") && len(d) > len(best) {
				best = d
			}
		}
		return repo + "\x00" + best
	}
	serving := map[string]bool{}
	for _, f := range all {
		if isGraphQLRole(f, facts.RoleServer) || (isGraphQLRole(f, facts.RoleClient) && served[f.Name]) {
			serving[packageOf(f.Repo, f.File)] = true
		}
	}
	out := map[string]string{}
	for _, f := range all {
		if !isGraphQLRole(f, facts.RoleClient) || served[f.Name] {
			continue
		}
		reason := GraphQLNoSchemaInRepo
		if serving[packageOf(f.Repo, f.File)] {
			reason = GraphQLFieldNotFound
		}
		out[graphqlsig.OperationIdentity(f)+"\x00"+f.Repo+"\x00"+f.File] = reason
	}
	return out
}

func isGraphQLRole(f facts.Fact, role string) bool {
	return f.Kind == facts.KindRoute && f.PropString(facts.PropRouteType) == facts.RouteTypeGraphQL &&
		f.PropString(facts.PropRole) == role
}
