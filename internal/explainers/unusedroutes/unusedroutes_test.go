package unusedroutes

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func flaggedRoute(repo, path, method string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: path, Repo: repo, Props: map[string]any{
		"method":               method,
		"role":                 "server",
		"unmatched_by_clients": true,
	}}
}

func TestExplain_FlagsCandidatesWithCaveat(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindService, Name: "golf", Repo: "golf"}, // multi-repo marker
		facts.Fact{Kind: facts.KindService, Name: "golf-ui", Repo: "golf-ui"},
		flaggedRoute("golf", "/api/secret/cleanup", "POST"),
		flaggedRoute("golf", "/api/legacy/export", "GET"),
		// A called route must not appear in the list, but must still count in the
		// denominator — it carries the positive marker, which is what says the
		// linker looked at it and found a caller. A route with neither marker is
		// one the linker declined to evaluate (a UI route, a GraphQL operation, a
		// generic path) and is excluded from both, because a proportion whose
		// denominator includes unevaluated routes describes a population the
		// numerator was never drawn from.
		facts.Fact{Kind: facts.KindRoute, Name: "/api/items/{id}", Repo: "golf",
			Props: map[string]any{"method": "GET", "role": "server",
				"matched_by_clients": true}},
		// Declined by the linker: present, served, and correctly outside the ratio.
		facts.Fact{Kind: facts.KindRoute, Name: "/health", Repo: "golf",
			Props: map[string]any{"method": "GET", "role": "server"}},
	)

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 per-repo insight, got %d: %+v", len(insights), insights)
	}
	in := insights[0]
	if !strings.Contains(in.Title, "golf") || !strings.Contains(in.Title, "2 of 3 route") {
		t.Errorf("title should name repo, unmatched count and total; got %q", in.Title)
	}
	// The mandatory out-of-snapshot caveat must be present.
	for _, want := range []string{"loaded clients ONLY", "webhooks", "Verify"} {
		if !strings.Contains(in.Description, want) {
			t.Errorf("description missing caveat phrase %q; got %q", want, in.Description)
		}
	}
	if strings.Contains(in.Description, "/api/items/{id}") {
		t.Errorf("a called (unflagged) route leaked into the insight: %q", in.Description)
	}
	if in.Confidence >= 0.9 {
		t.Errorf("confidence %v too high for a candidate-with-caveat signal", in.Confidence)
	}
}

func TestExplain_CapsSamplesAndEvidence(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindService, Name: "golf", Repo: "golf"})
	const n = 30 // > maxSamples (25)
	for i := 0; i < n; i++ {
		store.Add(flaggedRoute("golf", fmt.Sprintf("/api/ep%02d", i), "POST"))
	}
	// Enough called routes that the unmatched share stays below the threshold at
	// which the finding is about the client set rather than the endpoints. This
	// test is about sample capping, and it must keep exercising that branch.
	// They carry the positive marker: that is what says the linker evaluated
	// them and found a caller, and only evaluated routes belong in the
	// denominator.
	for i := 0; i < 20; i++ {
		store.Add(facts.Fact{Kind: facts.KindRoute, Name: fmt.Sprintf("/api/called%02d", i), Repo: "golf",
			Props: map[string]any{"method": "GET", "role": "server",
				"matched_by_clients": true}})
	}

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d", len(insights))
	}
	in := insights[0]
	// The title counts ALL flagged routes, not just the sampled ones.
	if !strings.Contains(in.Title, fmt.Sprintf("%d of 50 route", n)) {
		t.Errorf("title should report full count %d and the total, got %q", n, in.Title)
	}
	// Samples are truncated with a "+N more" marker.
	if !strings.Contains(in.Description, "(+5 more)") {
		t.Errorf("description should note 5 elided samples, got %q", in.Description)
	}
	// Evidence is capped at maxSamples.
	if len(in.Evidence) != maxSamples {
		t.Errorf("evidence should be capped at %d, got %d", maxSamples, len(in.Evidence))
	}
}

func TestExplain_MultipleReposSortedAndRoutesSorted(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindService, Name: "zulu", Repo: "zulu"},
		facts.Fact{Kind: facts.KindService, Name: "alpha", Repo: "alpha"},
		flaggedRoute("zulu", "/api/z2", "GET"),
		flaggedRoute("zulu", "/api/z1", "GET"),
		flaggedRoute("alpha", "/api/a1", "POST"),
	)
	// Called routes keep each repo's unmatched share below the point at which the
	// finding becomes one about the client set. This test is about ordering and
	// evidence keying, and it has to keep exercising that branch.
	for i := 0; i < 4; i++ {
		store.Add(
			facts.Fact{Kind: facts.KindRoute, Name: fmt.Sprintf("/api/zc%d", i), Repo: "zulu",
				Props: map[string]any{"method": "GET", "role": "server",
					"matched_by_clients": true}},
			facts.Fact{Kind: facts.KindRoute, Name: fmt.Sprintf("/api/ac%d", i), Repo: "alpha",
				Props: map[string]any{"method": "GET", "role": "server",
					"matched_by_clients": true}},
		)
	}

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 2 {
		t.Fatalf("expected one insight per repo, got %d", len(insights))
	}
	// Repos are reported in sorted order: alpha before zulu.
	if !strings.Contains(insights[0].Title, "alpha") || !strings.Contains(insights[1].Title, "zulu") {
		t.Errorf("repos not sorted: %q then %q", insights[0].Title, insights[1].Title)
	}
	// Routes within a repo are sorted: z1 before z2. Evidence is keyed on the route
	// FACT NAME, not the "METHOD /path" label — that is what lets the snapshot diff
	// attribute a newly-unused route to the change that caused it. The method rides
	// in the detail so two verbs on one path stay distinguishable.
	zulu := insights[1]
	if len(zulu.Evidence) != 2 || zulu.Evidence[0].Fact != "/api/z1" || zulu.Evidence[1].Fact != "/api/z2" {
		t.Errorf("routes within repo not sorted, or not keyed on the fact name: %+v", zulu.Evidence)
	}
	if !strings.HasPrefix(zulu.Evidence[0].Detail, "GET /api/z1 — ") {
		t.Errorf("method should be preserved in the detail, got %q", zulu.Evidence[0].Detail)
	}
}

func TestExplain_NoMethodFallbackAndUnlabeledRepo(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindService, Name: "svc", Repo: "svc"},
		// No Repo label -> "(unlabeled)" bucket; no method prop -> label is the bare name.
		facts.Fact{Kind: facts.KindRoute, Name: "/api/loose", Props: map[string]any{"unmatched_by_clients": true}},
		// Called routes in the same bucket, so the unmatched share stays low
		// enough that this stays a candidate list. The test is about the
		// unlabeled-repo fallback and the missing-method label.
		facts.Fact{Kind: facts.KindRoute, Name: "/api/called1",
			Props: map[string]any{"method": "GET", "matched_by_clients": true}},
		facts.Fact{Kind: facts.KindRoute, Name: "/api/called2",
			Props: map[string]any{"method": "GET", "matched_by_clients": true}},
		facts.Fact{Kind: facts.KindRoute, Name: "/api/called3",
			Props: map[string]any{"method": "GET", "matched_by_clients": true}},
	)

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d", len(insights))
	}
	if !strings.Contains(insights[0].Title, "(unlabeled)") {
		t.Errorf("blank repo should bucket under (unlabeled), got %q", insights[0].Title)
	}
	if insights[0].Evidence[0].Fact != "/api/loose" {
		t.Errorf("route with no method should fall back to its name, got %q", insights[0].Evidence[0].Fact)
	}
}

func TestExplain_SingleRepoNoServiceNodesYieldsNothing(t *testing.T) {
	store := facts.NewStore()
	// Route flagged but no service nodes (single-repo snapshot) -> no insights.
	store.Add(flaggedRoute("golf", "/api/secret/cleanup", "POST"))

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("single-repo snapshot should yield no insights, got %d", len(insights))
	}
}

// TestExplain_ThinClientCoverageDescribesTheSnapshot pins the case the estate
// actually hit: a large Rails monolith reported 3,392 of 3,732 routes unmatched,
// because its callers are browsers and third-party integrators rather than
// repositories.
// Presenting that as a list of dead-endpoint candidates is how a finding earns
// being ignored wholesale.
func TestExplain_ThinClientCoverageDescribesTheSnapshot(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindService, Name: "public-api", Repo: "public-api"})
	for i := 0; i < 90; i++ {
		store.Add(flaggedRoute("public-api", fmt.Sprintf("/v1/ep%02d", i), "GET"))
	}
	for i := 0; i < 10; i++ {
		store.Add(facts.Fact{Kind: facts.KindRoute, Name: fmt.Sprintf("/v1/called%02d", i), Repo: "public-api",
			Props: map[string]any{"method": "GET", "role": "server"}})
	}

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d", len(insights))
	}
	in := insights[0]
	if !strings.Contains(in.Title, "too thin") {
		t.Errorf("the finding must be about the client set, not the endpoints; got %q", in.Title)
	}
	if len(in.Evidence) != 0 {
		t.Errorf("listing 90 endpoints as candidates is the thing this branch exists to avoid; got %d", len(in.Evidence))
	}
	if in.Confidence < 0.8 {
		t.Errorf("the claim about coverage is a confident one: %v", in.Confidence)
	}
}

// The count of routes nothing assessed is part of the finding, not a detail of
// the implementation. "27 of 39 unused" and "27 of 39, with 827 more the pass
// could not look at" describe very different repositories, and only the second
// one is true of `insights`.
func TestExplain_ReportsWhatWasNotAssessed(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindService, Name: "svc", Repo: "svc"},
		flaggedRoute("svc", "/api/orders", "GET"),
		facts.Fact{Kind: facts.KindRoute, Name: "/api/items", Repo: "svc",
			Props: map[string]any{"method": "GET", "role": "server", "matched_by_clients": true}},
		// Neither marker: the linker declined to reason about these.
		facts.Fact{Kind: facts.KindRoute, Name: "/health", Repo: "svc",
			Props: map[string]any{"method": "GET", "role": "server"}},
		facts.Fact{Kind: facts.KindRoute, Name: "/dashboard", Repo: "svc",
			Props: map[string]any{"method": "GET", "role": "server", "type": "page"}},
	)

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d", len(insights))
	}
	if !strings.Contains(insights[0].Title, "1 of 2 route(s)") {
		t.Errorf("the denominator must be the routes actually assessed, got %q", insights[0].Title)
	}
	if !strings.Contains(insights[0].Description, "A further 2 route(s)") {
		t.Errorf("the unassessed count must appear in the finding, got %q", insights[0].Description)
	}
}

func TestExplain_RuntimeObservationsStayOutOfEveryCensus(t *testing.T) {
	store := facts.NewStore()
	runtimeProps := func(method, path string) map[string]any {
		return map[string]any{
			"resolution_level": "runtime-observed",
			"observed_via":     "rails-boot",
			"method":           method,
			"path":             path,
		}
	}
	store.Add(
		facts.Fact{Kind: facts.KindService, Name: "svc", Repo: "svc"},
		flaggedRoute("svc", "/api/orders", "GET"),
		facts.Fact{Kind: facts.KindRoute, Name: "/api/items", Repo: "svc",
			Props: map[string]any{"method": "GET", "role": "server", "matched_by_clients": true}},
		facts.Fact{Kind: facts.KindRoute, Name: "runtime-route: GET /api/orders", Repo: "svc",
			File: ".enola-runtime/boot.json", Props: runtimeProps("GET", "/api/orders")},
		facts.Fact{Kind: facts.KindRoute, Name: "runtime-route: GET /admin/avo", Repo: "svc",
			File: ".enola-runtime/boot.json", Props: runtimeProps("GET", "/admin/avo")},
	)

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d: %+v", len(insights), insights)
	}
	in := insights[0]
	if !strings.Contains(in.Title, "1 of 2 route(s)") {
		t.Errorf("observations must not enter the denominator, got %q", in.Title)
	}
	if strings.Contains(in.Description, "A further") {
		t.Errorf("observations must not count as linker-declined routes, got %q", in.Description)
	}
	if strings.Contains(in.Description, "runtime-route") {
		t.Errorf("an observation leaked into the candidate list: %q", in.Description)
	}
}

// Withheld as a list, the unmatched routes are still located: per route file, how
// many of the routes assessed there match no client. No route is named.
func TestExplain_ThinCoverageSaysWhereTheUnmatchedAre(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindService, Name: "api", Repo: "api"})
	add := func(file string, n, called int) {
		for i := 0; i < n; i++ {
			r := flaggedRoute("api", fmt.Sprintf("/%s/ep%02d", file, i), "GET")
			r.File = file + ".go"
			if i < called {
				delete(r.Props, "unmatched_by_clients")
				r.Props["matched_by_clients"] = true
			}
			store.Add(r)
		}
	}
	add("shop", 60, 5)
	add("social", 40, 0)
	insights, err := New().Explain(context.Background(), store)
	if err != nil || len(insights) != 1 || !strings.Contains(insights[0].Title, "too thin") {
		t.Fatalf("want the thin-coverage finding, got %+v (%v)", insights, err)
	}
	ev := insights[0].Evidence
	if len(ev) != 2 || ev[0].File != "shop.go" || ev[0].Detail != "55 of 60 assessed route(s) in this file match no client call site" ||
		ev[1].File != "social.go" {
		t.Errorf("evidence = %+v, want shop.go (55 of 60) then social.go", ev)
	}
}

// One method on one path registered in two files: one of the two handlers is not
// served. Path parameters compare equal however they are spelled.
func TestExplain_RouteRegisteredInTwoFiles(t *testing.T) {
	store := facts.NewStore()
	route := func(name, file string, line int) facts.Fact {
		return facts.Fact{Kind: facts.KindRoute, Name: name, File: file, Line: line, Repo: "api",
			Props: map[string]any{"method": "GET", "role": "server"}}
	}
	store.Add(
		route("/api/settings/maintenance-activities/{id:[0-9]+}", "settings_routes.go", 166),
		route("/api/settings/maintenance-activities/{id}", "inventory_routes.go", 95),
		route("/api/settings/brands/{id}", "inventory_routes.go", 40),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil || len(insights) != 1 {
		t.Fatalf("want one duplicate-route finding in a single-repo snapshot, got %+v (%v)", insights, err)
	}
	in := insights[0]
	if !strings.Contains(in.Title, "more than one file") || len(in.Evidence) != 1 ||
		in.Evidence[0].Detail != "GET /api/settings/maintenance-activities/{} is registered at inventory_routes.go:95, settings_routes.go:166" {
		t.Errorf("finding = %+v", in)
	}
}

// A multi-repository snapshot reports a duplicate registration too: it used to be
// found only when no client was loaded, which is exactly when nobody asks this
// explainer about routes. Middleware guarding one prefix from two files is not a
// duplicate, and neither is a Next.js layout beside its page: neither serves an
// endpoint of its own.
func TestExplain_RouteRegisteredInTwoFilesWithClientsLoaded(t *testing.T) {
	store := facts.NewStore()
	route := func(method, name, file string, line int, props map[string]any) facts.Fact {
		p := map[string]any{"method": method, "role": "server"}
		for k, v := range props {
			p[k] = v
		}
		return facts.Fact{Kind: facts.KindRoute, Name: name, File: file, Line: line, Repo: "api", Props: p}
	}
	store.Add(
		facts.Fact{Kind: facts.KindService, Name: "api", Repo: "api"},
		facts.Fact{Kind: facts.KindService, Name: "web", Repo: "web"},
		route("GET", "/api/settings/maintenance-activities", "settings_routes.go", 163, nil),
		route("GET", "/api/settings/maintenance-activities", "inventory_routes.go", 98, nil),
		route("USE", "/api/settings/maintenance-activities", "settings_routes.go", 161, map[string]any{"type": "middleware"}),
		route("USE", "/api/settings/maintenance-activities", "inventory_routes.go", 93, map[string]any{"type": "middleware"}),
		route("GET", "/login", "src/app/login/layout.tsx", 1, map[string]any{"type": "layout"}),
		route("GET", "/login", "src/app/login/page.tsx", 1, map[string]any{"type": "page"}),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var dup []facts.Insight
	for _, in := range insights {
		if strings.Contains(in.Title, "more than one file") {
			dup = append(dup, in)
		}
	}
	if len(dup) != 1 || len(dup[0].Evidence) != 1 ||
		dup[0].Evidence[0].Detail != "GET /api/settings/maintenance-activities is registered at inventory_routes.go:98, settings_routes.go:163" {
		t.Errorf("want one duplicate, the GET and not the middleware or the layout; got %+v", dup)
	}
}

// Routes a loaded client calls only from code the application cannot run are their
// own finding, naming the client call that never runs, and appear even when no route
// is unmatched at all.
func TestExplain_RoutesCalledOnlyFromCodeThatNeverRuns(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindService, Name: "api", Repo: "api"},
		facts.Fact{Kind: facts.KindService, Name: "web", Repo: "web"},
		facts.Fact{Kind: facts.KindRoute, Name: "/api/items/{id}", File: "items.go", Repo: "api", Props: map[string]any{
			"method": "GET", "role": "server", "matched_by_clients": true, "unreached_by_clients": true}},
		facts.Fact{Kind: facts.KindRoute, Name: "/api/orders", File: "orders.go", Repo: "api", Props: map[string]any{
			"method": "GET", "role": "server", "matched_by_clients": true}},
		facts.Fact{Kind: facts.KindRoute, Name: "/api/items/{}", File: "src/lib/api/items.ts", Line: 12, Repo: "web", Props: map[string]any{
			"method": "GET", "role": "client", facts.PropClientReachable: false, facts.PropCaller: "src/lib/api.ItemsApi.getItem",
			facts.PropMatchedRoutes: []any{map[string]any{"repo": "api", "method": "GET", "name": "/api/items/{id}"}}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var found *facts.Insight
	for i := range insights {
		if strings.Contains(insights[i].Title, "never runs") {
			found = &insights[i]
		}
	}
	if found == nil || len(found.Evidence) != 1 {
		t.Fatalf("want one never-runs finding with one route, got %+v", insights)
	}
	if d := found.Evidence[0].Detail; !strings.Contains(d, "src/lib/api/items.ts:12 (src/lib/api.ItemsApi.getItem)") {
		t.Errorf("evidence should name the call that never runs, got %q", d)
	}
}
