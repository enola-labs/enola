package rustextractor

import (
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestAxum_SingleRoute(t *testing.T) {
	ff := extractAST(t, `
async fn root() -> &'static str {
    "hello"
}

fn app() -> Router {
    Router::new().route("/", get(root))
}
`)
	routes := findFactsByKind(ff, facts.KindRoute)
	if len(routes) != 1 {
		t.Fatalf("expected 1 route fact, got %d: %+v", len(routes), routes)
	}
	r := routes[0]
	if r.Name != "/" {
		t.Errorf("route Name = %q, want \"/\"", r.Name)
	}
	if r.Props["method"] != "GET" {
		t.Errorf("method = %v, want GET", r.Props["method"])
	}
	if r.Props["framework"] != "axum" {
		t.Errorf("framework = %v, want axum", r.Props["framework"])
	}
	if r.Props["handler"] != "root" {
		t.Errorf("handler = %v, want root", r.Props["handler"])
	}
}

func TestAxum_ChainedMethodsOnSamePath(t *testing.T) {
	ff := extractAST(t, `
fn app() -> Router {
    Router::new().route("/users", get(list_users).post(create_user))
}
`)
	routes := findFactsByKind(ff, facts.KindRoute)
	if len(routes) != 2 {
		t.Fatalf("expected 2 route facts (GET+POST on the same path), got %d: %+v", len(routes), routes)
	}
	var gotGet, gotPost bool
	for _, r := range routes {
		if r.Name != "/users" {
			t.Errorf("route Name = %q, want /users", r.Name)
		}
		switch r.Props["method"] {
		case "GET":
			gotGet = true
			if r.Props["handler"] != "list_users" {
				t.Errorf("GET handler = %v, want list_users", r.Props["handler"])
			}
		case "POST":
			gotPost = true
			if r.Props["handler"] != "create_user" {
				t.Errorf("POST handler = %v, want create_user", r.Props["handler"])
			}
		}
	}
	if !gotGet || !gotPost {
		t.Errorf("expected both GET and POST routes, got %+v", routes)
	}
}

func TestAxum_MultipleRoutesChained(t *testing.T) {
	ff := extractAST(t, `
fn app() -> Router {
    Router::new()
        .route("/", get(root))
        .route("/users", get(list_users).post(create_user))
        .route("/users/:id", delete(delete_user))
}
`)
	routes := findFactsByKind(ff, facts.KindRoute)
	if len(routes) != 4 {
		t.Fatalf("expected 4 route facts, got %d: %+v", len(routes), routes)
	}
	paths := map[string]int{}
	for _, r := range routes {
		paths[r.Name]++
	}
	for _, want := range []string{"/", "/users", "/users/:id"} {
		if paths[want] == 0 {
			t.Errorf("expected at least one route for path %q, got %+v", want, paths)
		}
	}
}

func TestAxum_QualifiedHandlerPath(t *testing.T) {
	ff := extractAST(t, `
fn app() -> Router {
    Router::new().route("/health", get(handlers::health_check))
}
`)
	routes := findFactsByKind(ff, facts.KindRoute)
	if len(routes) != 1 {
		t.Fatalf("expected 1 route fact, got %d", len(routes))
	}
	if routes[0].Props["handler"] != "handlers::health_check" {
		t.Errorf("handler = %v, want handlers::health_check", routes[0].Props["handler"])
	}
}

func TestAxum_NonRouteCallsIgnored(t *testing.T) {
	// A `.route(` call whose shape doesn't match (non-literal path, or a
	// second arg that isn't a verb-builder chain) must not emit anything.
	ff := extractAST(t, `
fn other(x: SomeBuilder, path: String, svc: SomeService) {
    x.route(path, svc);
}
`)
	if routes := findFactsByKind(ff, facts.KindRoute); len(routes) != 0 {
		t.Errorf("expected 0 route facts for a non-matching .route(...) call, got %+v", routes)
	}
}

// extractComposed runs multiple files through the full per-file extractor and the
// crate-wide composeAxumPrefixes pass, mirroring RustExtractor.Extract, so nest
// mounts that cross files are resolved. Each file is at "src/<name>".
func extractComposed(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	crates := []crateInfo{{name: "app", dir: "."}}
	moduleDirs := map[string]bool{"src": true, "src/routers": true, ".": true}
	var all []facts.Fact
	var builders []axumBuilder
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ff, _, bs, _ := extractFileASTFull([]byte(files[name]), name, crates, moduleDirs)
		all = append(all, ff...)
		builders = append(builders, bs...)
	}
	return composeAxumPrefixes(all, builders, crates)
}

// routePaths returns the set of route-fact Names (multiplicity ignored).
func routePaths(ff []facts.Fact) map[string]bool {
	out := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute {
			out[f.Name] = true
		}
	}
	return out
}

// TestAxum_NestComposesPrefix_CrossFile covers the core fix: a router mounted via
// `.nest("/api/v1/datasets", routers::datasets::router())` in one file has the
// mount prefix composed onto the routes defined in another file, while the root
// builder's own routes keep their bare path.
func TestAxum_NestComposesPrefix_CrossFile(t *testing.T) {
	got := routePaths(extractComposed(t, map[string]string{
		"src/router_builder.rs": `
fn build() -> Router {
    Router::new()
        .route("/", get(root))
        .nest("/api/v1/datasets", routers::datasets::router())
        .nest("/api/v1/search", routers::search::router())
}`,
		"src/routers/datasets.rs": `
pub fn router() -> Router {
    Router::new()
        .route("/", get(list).post(create))
        .route("/status", get(status))
        .route("/{dataset_id}/data", get(data))
}`,
		"src/routers/search.rs": `
pub fn router() -> Router {
    Router::new().route("/", post(search))
}`,
	}))

	for _, want := range []string{
		"/",                       // root builder's own route, bare
		"/api/v1/datasets",        // datasets router "/" under the mount
		"/api/v1/datasets/status", // composed
		"/api/v1/datasets/{dataset_id}/data",
		"/api/v1/search", // search router "/" under the mount
	} {
		if !got[want] {
			t.Errorf("missing composed route %q; got %v", want, got)
		}
	}
	if got["/status"] {
		t.Errorf("bare sub-path /status must have been composed away; got %v", got)
	}
}

// TestAxum_NestMultiLevel covers a router nested under another nested router: the
// prefix accumulates across both mounts.
func TestAxum_NestMultiLevel(t *testing.T) {
	got := routePaths(extractComposed(t, map[string]string{
		"src/router_builder.rs": `
fn build() -> Router {
    Router::new().nest("/api/v1/activity", routers::activity::router())
}`,
		"src/routers/activity.rs": `
pub fn router() -> Router {
    Router::new()
        .route("/agents", get(agents))
        .nest("/spans", routers::spans::router())
}`,
		"src/routers/spans.rs": `
pub fn router() -> Router {
    Router::new().route("/{id}", get(one))
}`,
	}))

	for _, want := range []string{"/api/v1/activity/agents", "/api/v1/activity/spans/{id}"} {
		if !got[want] {
			t.Errorf("missing multi-level composed route %q; got %v", want, got)
		}
	}
}

// TestAxum_UnresolvedNestKeepsBarePath covers graceful degradation: a dynamic
// mount (`.nest(mount, r)` with non-literal args) leaves the callee's routes at
// their bare path rather than dropping them.
func TestAxum_UnresolvedNestKeepsBarePath(t *testing.T) {
	got := routePaths(extractComposed(t, map[string]string{
		"src/router_builder.rs": `
fn build(mount: &str, r: Router) -> Router {
    Router::new().nest(mount, r)
}`,
		"src/routers/orphan.rs": `
pub fn router() -> Router {
    Router::new().route("/thing", get(thing))
}`,
	}))
	if !got["/thing"] {
		t.Errorf("unresolved-mount router must keep its bare path /thing; got %v", got)
	}
}

func TestAxum_NoHandlerStillEmitsRoute(t *testing.T) {
	// A closure handler (or anything else axumFirstArgName can't name) still
	// produces a route fact, just without a "handler" prop.
	ff := extractAST(t, `
fn app() -> Router {
    Router::new().route("/", get(|| async { "hi" }))
}
`)
	routes := findFactsByKind(ff, facts.KindRoute)
	if len(routes) != 1 {
		t.Fatalf("expected 1 route fact, got %d", len(routes))
	}
	if _, present := routes[0].Props["handler"]; present {
		t.Errorf("expected no handler prop for a closure handler, got %v", routes[0].Props["handler"])
	}
}

// TestAxum_PerRouteLayerDoesNotSwallowTheVerb covers `.route(p, get(h).layer(mw))`.
//
// A non-verb method in the MethodRouter chain used to terminate the walk, so
// per-route middleware — idiomatic Axum — discarded the verbs beneath it and the
// route vanished entirely, with `get` sitting in plain sight. A route silently absent
// from the graph is the worst way to be wrong: nothing reports it, and a client
// calling that path resolves to nothing. Found on a production Axum service added to
// the benchmark corpus.
func TestAxum_PerRouteLayerDoesNotSwallowTheVerb(t *testing.T) {
	ff := extractComposed(t, map[string]string{
		"src/router.rs": `
fn build() -> Router {
    Router::new()
        .route("/plain", get(root))
        .route("/layered", get(openapi::handler).layer(Extension(Arc::new(openapi))))
        .route("/stacked", post(create).layer(a).route_layer(b))
        .route("/both_verbs_then_layer", get(list).post(create).layer(mw))
}`,
	})

	got := routePaths(ff)
	for _, want := range []string{"/plain", "/layered", "/stacked", "/both_verbs_then_layer"} {
		if !got[want] {
			t.Errorf("missing route %q — a decorator in the chain dropped it; got %v", want, got)
		}
	}

	// The verbs under the decorator must survive, not just the path.
	methods := map[string]map[string]bool{}
	for _, f := range ff {
		if f.Kind != facts.KindRoute {
			continue
		}
		m, _ := f.Props["method"].(string)
		if methods[f.Name] == nil {
			methods[f.Name] = map[string]bool{}
		}
		methods[f.Name][m] = true
	}
	if !methods["/layered"]["GET"] {
		t.Errorf("/layered lost its GET verb: %v", methods["/layered"])
	}
	if !methods["/both_verbs_then_layer"]["GET"] || !methods["/both_verbs_then_layer"]["POST"] {
		t.Errorf("/both_verbs_then_layer should keep both verbs, got %v", methods["/both_verbs_then_layer"])
	}
}

// TestAxum_HandlerValueWithoutVerbStillEmitsNothing pins the deliberate half of the
// same walk: `.route(path, handler_var)` has no verb to infer, and inventing one
// would produce a route that could false-match another repository's endpoint. The
// permissive recursion above must not turn this into a guess.
func TestAxum_HandlerValueWithoutVerbStillEmitsNothing(t *testing.T) {
	ff := extractComposed(t, map[string]string{
		"src/router.rs": `
fn build() -> Router {
    Router::new()
        .route("/from_var", okay.clone())
        .route("/real", get(handler))
}`,
	})
	got := routePaths(ff)
	if got["/from_var"] {
		t.Error("/from_var has no HTTP verb and must not be emitted")
	}
	if !got["/real"] {
		t.Error("/real should still be extracted")
	}
}

// TestAxum_NestedAtTwoMounts_PropsAreNotShared pins that each emitted copy of a router
// mounted at several paths gets its OWN props map.
//
// The rewrite builds copies with `nf := f`, which copies the struct but not the map
// behind Props. Sharing it is invisible at extraction — every copy holds identical props
// — and only bites afterwards, when the binders write PER-ROUTE verdicts into them
// (unmatched_by_clients, unmatched_reason, handler). One router nested at two mount
// points then has one of its paths' verdicts written over both.
//
// The failure was also cache-dependent, which is worse than the bug itself: cached facts
// round-trip through json.Unmarshal and come back with independent maps, so this could
// only ever appear on a cache MISS — the same tree giving a different answer depending
// on whether .enola happened to be warm.
//
// Asserting on props EQUALITY would pass either way, so this mutates one copy and
// requires the other not to move.
func TestAxum_NestedAtTwoMounts_PropsAreNotShared(t *testing.T) {
	ff := extractComposed(t, map[string]string{
		"src/main.rs": `
fn build() -> Router {
    Router::new()
        .nest("/api/v1/items", routers::items::router())
        .nest("/api/v2/items", routers::items::router())
}`,
		"src/routers/items.rs": `
pub fn router() -> Router {
    Router::new().route("/list", get(list))
}`,
	})

	var copies []*facts.Fact
	for i := range ff {
		if ff[i].Kind == facts.KindRoute && strings.HasSuffix(ff[i].Name, "/items/list") {
			copies = append(copies, &ff[i])
		}
	}
	if len(copies) != 2 {
		t.Fatalf("expected the router to compose at both mounts, got %d copies", len(copies))
	}
	if copies[0].Props == nil || copies[1].Props == nil {
		t.Fatal("route copies carry no props")
	}

	// What a binder does: write a verdict onto one route.
	copies[0].Props["unmatched_by_clients"] = true

	if _, leaked := copies[1].Props["unmatched_by_clients"]; leaked {
		t.Errorf("props are shared between mount copies: a verdict written to %q appeared on %q",
			copies[0].Name, copies[1].Name)
	}
}
