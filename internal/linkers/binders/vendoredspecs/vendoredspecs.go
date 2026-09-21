// Package vendoredspecs reclassifies OpenAPI specs that a client-only application
// vendors for code generation: routes it CALLS, not routes it serves.
//
// THE BUG THIS FIXES. The OpenAPI extractor defaults every spec to role=server and
// demotes to client only by directory convention (a "client" segment under an
// "openapi" one). Backend repos follow that convention; mobile apps generally do not,
// keeping each consumed service's spec in a per-feature openapi/ directory the code
// generator reads. Every one of those was indexed as a SERVED endpoint, making the app
// a match target for any repo calling the same paths: a mobile app that serves no HTTP
// at all can collect inbound dependencies from most of the backends around it, plus a
// crop of "endpoint no client calls" findings on routes it never served.
//
// WHY NOT A PATH RULE. The vendored specs sit in a bare openapi/ directory,
// byte-identical in shape to a service's own spec directory; no path convention
// separates them. And the tempting inverse — "a repo whose only server routes come
// from OpenAPI is not a server" — is false: spec-first services exist whose entire
// served surface is declared in api/openapi/*.yml and nowhere in code. Both shapes
// occur in one estate, so the rule has to rest on positive evidence about the repo
// rather than on the absence of evidence.
package vendoredspecs

import (
	"context"
	"log"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// PropVendoredSpec marks a route whose role this binder rewrote: an OpenAPI operation
// that was extracted as server but belongs to a spec the repo vendors in order to CALL
// it. Written rather than silently rewriting, so the change is auditable — the rewritten
// set is query_facts(prop="vendored_spec"), and a prop appearing or disappearing shows up
// in the snapshot diff. It is also what makes the rewrite reversible: see Bind.
const PropVendoredSpec = "vendored_spec"

// Binder demotes vendored OpenAPI server routes to client call sites in repos that
// cannot serve HTTP.
//
// It is pre-link, and that is a correctness constraint rather than a preference. The
// role it rewrites is read by routeindex.IndexServerRoutes, by both unmatched-route
// passes and by the coverage counters; running after linking would leave every one of
// them having already decided against a role this binder was about to change.
type Binder struct{}

func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "vendored-specs" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePreLink }

// Bind rewrites the role of OpenAPI routes in client-only repos, and restores it in
// repos that stop qualifying.
//
// Both directions are required by the Binder idempotency contract. Loading a repo that
// later grows a real server (or re-extracting one under a corrected extractor) must not
// leave the demotion behind, and PropVendoredSpec is what makes that possible: it marks
// exactly the facts this binder rewrote, so restoring touches those and nothing else. A
// genuine client spec — one the extractor itself classified from the openapi/client/
// convention — carries no such mark and is therefore never "restored" into a server
// route it never was.
func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	// FactsRef, not All: classify reads only, and finishes before UpdateWhere mutates
	// anything. Copying the fact set here would duplicate every Props map on every
	// snapshot, for a pass that writes to a few hundred facts at most.
	clientOnly := clientOnlyRepos(store.FactsRef())

	demoted, restored := 0, 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindRoute || f.Repo == "" ||
			f.PropString(facts.PropSource) != facts.RouteSourceOpenAPI {
			return
		}
		wasRewritten, _ := f.PropAny(PropVendoredSpec).(bool)
		// A spec the extractor already classified as a client spec is correct as it
		// stands: leave it alone, and — crucially — do not let the restore branch below
		// promote it to a server route it was never extracted as.
		if !wasRewritten && f.PropString(facts.PropRole) == facts.RoleClient {
			return
		}
		if clientOnly[f.Repo] {
			if f.Props == nil {
				f.Props = map[string]any{}
			}
			if !wasRewritten {
				demoted++
			}
			f.SetProp(facts.PropRole, facts.RoleClient)
			f.SetProp(PropVendoredSpec, true)
			return
		}
		if wasRewritten {
			f.SetProp(facts.PropRole, facts.RoleServer)
			f.DelProp(PropVendoredSpec)
			restored++
		}
	})
	if demoted > 0 || restored > 0 {
		log.Printf("[binder:vendored-specs] reclassified %d vendored OpenAPI route(s) as client call sites, restored %d",
			demoted, restored)
	}
	return nil
}

// clientOnlyRepos returns the repos whose OpenAPI specs must be vendored client
// contracts, by the conjunction of two conditions:
//
//	∃ route{source ∈ facts.NativeAppClientSources}  — the repo ships a native-app
//	                                                  HTTP client, so it is an app
//	∄ route{role ≠ client, source ≠ openapi}        — and nothing outside a spec
//	                                                  declares a served endpoint
//
// The first is positive evidence and does the real work: without it the second alone
// would misclassify every spec-first backend, whose served surface is declared only in
// its own OpenAPI files. The second is the guard that keeps a Kotlin or Swift repo that
// genuinely serves something (a Ktor service, a Vapor app) out of the set.
//
// A route with no role counts as a server route, per the facts.RoleServer contract — an
// extractor that found a declaration without a call site found a served endpoint. That
// deliberately includes UI/page routes: a repo serving browser routes is a web app, and
// leaving it untouched is the safe direction for a linker that everywhere prefers a
// missing edge to a fabricated one.
//
// PropVendoredSpec needs no special handling here even though this runs over a store
// that may already contain the binder's own output: only OpenAPI routes are ever
// rewritten, and the second condition skips OpenAPI routes entirely. The classification
// therefore reads the same before and after a demotion, which is what keeps the pass
// stable across appends instead of self-reinforcing.
func clientOnlyRepos(all []facts.Fact) map[string]bool {
	nativeApp := map[string]bool{}
	servesOutsideSpec := map[string]bool{}
	for _, f := range all {
		if f.Kind != facts.KindRoute || f.Repo == "" {
			continue
		}
		src := f.PropString(facts.PropSource)
		if facts.NativeAppClientSources[src] {
			nativeApp[f.Repo] = true
		}
		if src == facts.RouteSourceOpenAPI {
			continue
		}
		if f.PropString(facts.PropRole) != facts.RoleClient {
			servesOutsideSpec[f.Repo] = true
		}
	}
	out := map[string]bool{}
	for repo := range nativeApp {
		if !servesOutsideSpec[repo] {
			out[repo] = true
		}
	}
	return out
}
