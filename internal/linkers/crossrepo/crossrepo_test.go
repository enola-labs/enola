package crossrepo

import (
	"reflect"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	httpsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/http"
	importsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/imports"
	kafkasignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/kafka"
	sharedcodesignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/sharedcode"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// --- helpers ---

func serverRoute(repo, path, method string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindRoute,
		Name: path,
		Repo: repo,
		Props: map[string]any{
			"method": method,
			"role":   "server",
		},
	}
}

func clientRoute(repo, path, method string, extra map[string]any) facts.Fact {
	props := map[string]any{"method": method, "role": "client"}
	for k, v := range extra {
		props[k] = v
	}
	return facts.Fact{Kind: facts.KindRoute, Name: path, Repo: repo, Props: props}
}

func importDep(repo, target string) facts.Fact {
	return facts.Fact{
		Kind:      facts.KindDependency,
		Name:      repo + " -> " + target,
		Repo:      repo,
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: target}},
	}
}

// findEdge returns the cross-repo dependency (evidence) fact for
// consumer->provider, or nil.
func findEdge(out []facts.Fact, consumer, provider string) *facts.Fact {
	want := consumer + " -> " + provider
	for i := range out {
		f := out[i]
		if f.Kind == facts.KindDependency && f.Repo == consumer && f.Name == want {
			return &out[i]
		}
	}
	return nil
}

// hasServiceEdge reports whether the consumer's service node carries a
// depends_on relation to provider (the traversable graph edge).
func hasServiceEdge(out []facts.Fact, consumer, provider string) bool {
	for _, f := range out {
		if f.Kind != facts.KindService || f.Name != consumer {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelDependsOn && rel.Target == provider {
				return true
			}
		}
	}
	return false
}

func serviceNodes(out []facts.Fact) []string {
	var names []string
	for _, f := range out {
		if f.Kind == facts.KindService {
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)
	return names
}

// crossRepoEdges returns the consumer->provider dependency (evidence) facts —
// the actual cross-repo edges. Service nodes (which now exist for every loaded
// repo) are excluded, so this measures "did a link form?".
func crossRepoEdges(out []facts.Fact) []facts.Fact {
	return depFactsOfType(out, facts.TypeCrossRepo)
}

// sharedCodeFacts returns the symmetric shared-code coupling facts — pairs that share
// type names with no import or call between them. These are deliberately NOT edges, so
// they must be counted separately from crossRepoEdges: lumping them together would let
// a "must not link" test pass while the linker was in fact emitting a coupling.
func sharedCodeFacts(out []facts.Fact) []facts.Fact {
	return depFactsOfType(out, facts.TypeCrossRepoSharedCode)
}

func depFactsOfType(out []facts.Fact, typ string) []facts.Fact {
	var ff []facts.Fact
	for _, f := range out {
		if f.Kind == facts.KindDependency && f.Props["type"] == typ {
			ff = append(ff, f)
		}
	}
	return ff
}

// findSharedCode returns the coupling fact for the unordered pair {a, b}, or nil.
func findSharedCode(out []facts.Fact, a, b string) *facts.Fact {
	if a > b {
		a, b = b, a
	}
	want := a + " <-> " + b
	for i := range out {
		if out[i].Kind == facts.KindDependency && out[i].Name == want &&
			out[i].Props["type"] == facts.TypeCrossRepoSharedCode {
			return &out[i]
		}
	}
	return nil
}

// anyServiceEdge reports whether ANY service node carries a depends_on relation — the
// check that a coupling stayed out of the traversable graph.
func anyServiceEdge(out []facts.Fact) bool {
	for _, f := range out {
		if f.Kind != facts.KindService {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelDependsOn {
				return true
			}
		}
	}
	return false
}

// httpCoverageOf returns the http_client coverage counts attached to a service
// node, and whether an edge_coverage entry was present.
func httpCoverageOf(out []facts.Fact, repo string) (detected, resolved, unresolved int, ok bool) {
	for _, f := range out {
		if f.Kind != facts.KindService || f.Name != repo {
			continue
		}
		list, _ := f.Props["edge_coverage"].([]map[string]any)
		for _, ec := range list {
			if ec["edge_type"] != "http_client" {
				continue
			}
			return ec["detected"].(int), ec["resolved"].(int), ec["unresolved"].(int), true
		}
	}
	return 0, 0, 0, false
}

// --- normalization ---

func TestNormalizeLabel(t *testing.T) {
	for _, in := range []string{"app-web", "app_web", "AppWeb", "APP-WEB"} {
		if got := facts.NormalizeRepoLabel(in); got != "appweb" {
			t.Errorf("facts.NormalizeRepoLabel(%q) = %q, want appweb", in, got)
		}
	}
}

// A named single-segment endpoint links. This is the wrapper ->
// enola-licensing-api shape: a client POSTing /activate to the service that
// serves POST /activate. It drew no edge while isGenericPath was a segment
// count, and even once the path cleared that filter the server-side index still
// dropped it, because pathSuffixes yields nothing below minSharedSegments —
// so the call reported path_unknown against an index it was never admitted to.
func TestComputeLinks_HTTP_SingleSegmentPath(t *testing.T) {
	in := []facts.Fact{
		serverRoute("licensing-api", "/activate", "POST"),
		clientRoute("enterprise", "/activate", "POST", nil),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	if findEdge(out, "enterprise", "licensing-api") == nil {
		t.Fatalf("enterprise should depend on licensing-api via POST /activate; edges=%+v", crossRepoEdges(out))
	}
	if !hasServiceEdge(out, "enterprise", "licensing-api") {
		t.Errorf("enterprise service node missing depends_on licensing-api")
	}
	// The call site resolved, so it must no longer read as a coverage blind spot
	// — the symptom that exposed this bug in the first place.
	for _, f := range out {
		if f.Kind != facts.KindService || f.Repo != "enterprise" {
			continue
		}
		cov, ok := f.Props["edge_coverage"].([]map[string]any)
		if !ok || len(cov) != 1 {
			t.Fatalf("enterprise edge_coverage = %+v, want one http_client entry", f.Props["edge_coverage"])
		}
		if cov[0]["resolved"] != 1 || cov[0]["unresolved"] != 0 {
			t.Errorf("enterprise coverage = %+v, want resolved 1 / unresolved 0", cov[0])
		}
	}
}

// The generic vocabulary still refuses to link: every service serves /health,
// so a path+method match between two repos is no evidence they talk.
func TestComputeLinks_HTTP_GenericPathDoesNotLink(t *testing.T) {
	in := []facts.Fact{
		serverRoute("svc-a", "/health", "GET"),
		clientRoute("svc-b", "/health", "GET", nil),
	}
	if edges := crossRepoEdges(ComputeLinks(in, nil, allSignals(), vocab.Default())); len(edges) != 0 {
		t.Errorf("/health must not link, got %d edge(s): %+v", len(edges), edges)
	}
}

// A single-segment path is thinner evidence, so an ambiguous match (two loaded
// repos serving POST /activate) draws nothing rather than guessing a provider.
func TestComputeLinks_HTTP_SingleSegmentAmbiguousDoesNotLink(t *testing.T) {
	in := []facts.Fact{
		serverRoute("licensing-a", "/activate", "POST"),
		serverRoute("licensing-b", "/activate", "POST"),
		clientRoute("enterprise", "/activate", "POST", nil),
	}
	if edges := crossRepoEdges(ComputeLinks(in, nil, allSignals(), vocab.Default())); len(edges) != 0 {
		t.Errorf("ambiguous single-segment match must not link, got %d edge(s): %+v", len(edges), edges)
	}
}

// --- (A) HTTP linking ---

func TestComputeLinks_HTTPMatch(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/{id}", "GET", nil),
		serverRoute("svc-beta", "/api/items/{itemId}", "GET"),
		serverRoute("svc-beta", "/api/items/{itemId}", "POST"), // method mismatch — ignored
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	if got, want := serviceNodes(out), []string{"svc-alpha", "svc-beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("service nodes = %v, want %v", got, want)
	}
	if !hasServiceEdge(out, "svc-alpha", "svc-beta") {
		t.Fatalf("svc-alpha service node missing depends_on svc-beta; got %+v", out)
	}
	e := findEdge(out, "svc-alpha", "svc-beta")
	if e == nil {
		t.Fatalf("missing svc-alpha -> svc-beta edge; got %+v", out)
	}
	if e.Props["type"] != "cross_repo" || e.Props["synthetic"] != SyntheticMarker {
		t.Errorf("edge props = %v", e.Props)
	}
	if via, _ := e.Props["via"].([]string); !reflect.DeepEqual(via, []string{"http"}) {
		t.Errorf("via = %v, want [http]", e.Props["via"])
	}
	if c, _ := e.Props["endpoint_count"].(int); c != 1 {
		t.Errorf("endpoint_count = %v, want 1", e.Props["endpoint_count"])
	}
}

// TestComputeLinks_ExternalClientBucketed verifies that a client route tagged
// external is counted in the external bucket, not unresolved, and produces no
// cross-repo edge — while internal calls still resolve/unresolve as before.
func TestComputeLinks_ExternalClientBucketed(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/{id}", "GET", nil),                                 // resolves to svc-beta
		clientRoute("svc-alpha", "/rest/api/2/issue", "POST", map[string]any{"external": true}), // external third party
		clientRoute("svc-alpha", "/api/unknown/{id}", "GET", nil),                               // internal, no server -> unresolved
		serverRoute("svc-beta", "/api/items/{itemId}", "GET"),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	ec := edgeCoverageOf(out, "svc-alpha")
	if ec == nil {
		t.Fatalf("no edge_coverage on svc-alpha; got %+v", out)
	}
	if ec["detected"] != 3 || ec["resolved"] != 1 || ec["external"] != 1 || ec["unresolved"] != 1 {
		t.Errorf("coverage = detected:%v resolved:%v external:%v unresolved:%v; want 3/1/1/1",
			ec["detected"], ec["resolved"], ec["external"], ec["unresolved"])
	}
	// The external call must not create a cross-repo edge.
	if hasServiceEdge(out, "svc-alpha", "svc-beta") == false {
		t.Errorf("expected the internal /api/items edge to svc-beta")
	}
}

// TestUnmatchedClientRouteKeys verifies the per-call unresolved verdict mirrors the
// linker: resolved calls are absent, and each unresolved call carries its reason,
// while external calls are omitted.
func TestUnmatchedClientRouteKeys(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/{id}", "GET", nil),                                 // resolves
		clientRoute("svc-alpha", "/api/items/{id}", "DELETE", nil),                              // path exists, wrong verb -> method_mismatch
		clientRoute("svc-alpha", "/api/unknown/{id}", "GET", nil),                               // no server serves path -> path_unknown
		clientRoute("svc-alpha", "/health", "GET", nil),                                         // 1 segment -> generic_path
		clientRoute("svc-alpha", "/api/things/{id}", "", nil),                                   // no verb -> no_method
		clientRoute("svc-alpha", "/rest/api/2/issue", "POST", map[string]any{"external": true}), // external -> omitted
		serverRoute("svc-beta", "/api/items/{itemId}", "GET"),
	}
	got := httpsignal.UnmatchedClientRouteKeys(routeindex.New(vocab.Default()), in)

	want := map[string]string{
		routeindex.RouteIdentity(clientRoute("svc-alpha", "/api/items/{id}", "DELETE", nil)): "method_mismatch",
		routeindex.RouteIdentity(clientRoute("svc-alpha", "/api/unknown/{id}", "GET", nil)):  "path_unknown",
		routeindex.RouteIdentity(clientRoute("svc-alpha", "/health", "GET", nil)):            "generic_path",
		routeindex.RouteIdentity(clientRoute("svc-alpha", "/api/things/{id}", "", nil)):      "no_method",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d unmatched, want %d: %+v", len(got), len(want), got)
	}
	for id, reason := range want {
		if got[id] != reason {
			t.Errorf("identity %q: got reason %q, want %q", id, got[id], reason)
		}
	}
	// The resolved and the external call must not appear.
	if _, bad := got[routeindex.RouteIdentity(clientRoute("svc-alpha", "/api/items/{id}", "GET", nil))]; bad {
		t.Errorf("resolved call should not be flagged unmatched")
	}
}

// edgeCoverageOf returns the http_client edge_coverage map for a service node.
func edgeCoverageOf(out []facts.Fact, repo string) map[string]any {
	for _, f := range out {
		if f.Kind != facts.KindService || f.Name != repo {
			continue
		}
		list, _ := f.Props["edge_coverage"].([]map[string]any)
		for _, ec := range list {
			if ec["edge_type"] == "http_client" {
				return ec
			}
		}
	}
	return nil
}

func TestComputeLinks_HTTPGatewayPath(t *testing.T) {
	server := serverRoute("svc-beta", "/items/{id}", "GET")
	server.Props["gateway_path"] = "/api/catalogue/items/{id}"
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/catalogue/items/{id}", "GET", nil),
		server,
	}
	if findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "svc-beta") == nil {
		t.Errorf("expected gateway-path match to produce an edge")
	}
}

// TestComputeLinks_HTTPClientPrefixMatch covers the BFF/gateway pattern: the client
// calls a path carrying an extra "/api/settings" prefix that the server (a different
// repo) serves un-prefixed. Client-suffix matching must still resolve the edge.
func TestComputeLinks_HTTPClientPrefixMatch(t *testing.T) {
	in := []facts.Fact{
		clientRoute("golf-ui", "/api/settings/tickets/{id}/resolve", "POST", nil),
		serverRoute("golf", "/tickets/{id:[0-9]+}/resolve", "POST"),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if !hasServiceEdge(out, "golf-ui", "golf") {
		t.Fatalf("client prefix path did not resolve to server; got %+v", out)
	}
	// Suffix (not full-path) match → probable, and the call counts as resolved.
	if _, r, u, ok := httpCoverageOf(out, "golf-ui"); !ok || r != 1 || u != 0 {
		t.Errorf("golf-ui coverage = resolved %d unresolved %d (ok=%v); want 1/0", r, u, ok)
	}
}

// TestComputeLinks_HTTPNextjsDynamicSegment checks that a Next.js-style server path
// with a [id] dynamic segment normalizes and matches a client {} placeholder.
func TestComputeLinks_HTTPNextjsDynamicSegment(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/{id}", "GET", nil),
		serverRoute("svc-beta", "/api/items/[id]", "GET"),
	}
	if !hasServiceEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "svc-beta") {
		t.Errorf("Next.js [id] server segment did not match client {} placeholder")
	}
}

// TestComputeLinks_HTTPFormatSuffixMatch checks that a client calling a
// ".json"-suffixed path (Retrofit/redux-tools/URLSession idiom) resolves to the
// backend route served without the format suffix.
func TestComputeLinks_HTTPFormatSuffixMatch(t *testing.T) {
	in := []facts.Fact{
		clientRoute("android", "v2/devices/{id}.json", "DELETE", nil),
		serverRoute("golf", "/devices/:id", "DELETE"),
	}
	if !hasServiceEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "android", "golf") {
		t.Errorf("client .json path did not match server route without the suffix")
	}
}

func TestComputeLinks_HTTPGenericPathSkipped(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/health", "GET", nil),
		serverRoute("svc-beta", "/health", "GET"),
	}
	if edges := crossRepoEdges(ComputeLinks(in, nil, allSignals(), vocab.Default())); len(edges) != 0 {
		t.Errorf("generic path produced edges: %+v", edges)
	}
}

func TestComputeLinks_HTTPSelfLinkSkipped(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/{id}", "GET", nil),
		serverRoute("svc-alpha", "/api/items/{id}", "GET"),
	}
	if out := ComputeLinks(in, nil, allSignals(), vocab.Default()); len(out) != 0 {
		t.Errorf("self-link produced links: %+v", out)
	}
}

// A repo that serves a path AND calls it is calling its own backend. Another
// loaded repo serving an API-compatible surface (a rewrite, a second
// implementation) must not capture the call: the nearest explanation wins.
func TestComputeLinks_HTTPSelfServedPreferredOverOtherRepo(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/{id}", "GET", nil),
		serverRoute("svc-alpha", "/api/items/{id}", "GET"),
		serverRoute("svc-rewrite", "/api/items/{id}", "GET"),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if e := findEdge(out, "svc-alpha", "svc-rewrite"); e != nil {
		t.Errorf("client bound to an API-compatible other repo: %+v", e)
	}
}

// The preference is for the client's OWN repo only — an unrelated third repo
// serving the same path still leaves the two genuine candidates ambiguous.
func TestComputeLinks_HTTPSelfPreferenceDoesNotSuppressOthers(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/{id}", "GET", nil),
		serverRoute("svc-beta", "/api/items/{id}", "GET"),
	}
	if findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "svc-beta") == nil {
		t.Errorf("a genuine cross-repo call was suppressed")
	}
}

func TestComputeLinks_HTTPAmbiguousResolvedByHint(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/{id}", "GET", map[string]any{"api": "svc-beta"}),
		serverRoute("svc-beta", "/api/items/{id}", "GET"),
		serverRoute("svc-other", "/api/items/{id}", "GET"),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if findEdge(out, "svc-alpha", "svc-beta") == nil {
		t.Errorf("hint did not resolve to svc-beta: %+v", out)
	}
	if findEdge(out, "svc-alpha", "svc-other") != nil {
		t.Errorf("unexpected edge to svc-other")
	}
}

func TestComputeLinks_HTTPAmbiguousNoHintSkipped(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/{id}", "GET", nil),
		serverRoute("svc-beta", "/api/items/{id}", "GET"),
		serverRoute("svc-other", "/api/items/{id}", "GET"),
	}
	for _, f := range ComputeLinks(in, nil, allSignals(), vocab.Default()) {
		if f.Kind == facts.KindDependency {
			t.Errorf("ambiguous match without hint produced edge: %+v", f)
		}
	}
}

// TestComputeLinks_CoverageBlindSpot checks that a service whose only outbound
// HTTP-client call site cannot be resolved to any loaded server is recorded as a
// coverage gap (detected>0, resolved=0) rather than a true isolate — while a
// service whose call site does resolve is fully covered.
func TestComputeLinks_CoverageBlindSpot(t *testing.T) {
	in := []facts.Fact{
		// svc-alpha calls a path no loaded repo serves -> unresolved blind spot.
		clientRoute("svc-alpha", "/api/orders/{id}", "GET", nil),
		// svc-gamma calls a path svc-beta serves -> resolved.
		clientRoute("svc-gamma", "/api/items/{id}", "GET", nil),
		serverRoute("svc-beta", "/api/items/{id}", "GET"),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	// svc-alpha: detected but unresolved, and no outbound edge formed.
	if hasServiceEdge(out, "svc-alpha", "svc-beta") {
		t.Errorf("svc-alpha unexpectedly linked to svc-beta")
	}
	d, r, u, ok := httpCoverageOf(out, "svc-alpha")
	if !ok {
		t.Fatalf("svc-alpha has no edge_coverage; want a coverage gap")
	}
	if d != 1 || r != 0 || u != 1 {
		t.Errorf("svc-alpha coverage = detected %d resolved %d unresolved %d; want 1/0/1", d, r, u)
	}

	// svc-gamma: detected and fully resolved.
	d, r, u, ok = httpCoverageOf(out, "svc-gamma")
	if !ok {
		t.Fatalf("svc-gamma has no edge_coverage")
	}
	if d != 1 || r != 1 || u != 0 {
		t.Errorf("svc-gamma coverage = detected %d resolved %d unresolved %d; want 1/1/0", d, r, u)
	}

	// svc-beta serves but makes no client calls -> no coverage entry (true leaf).
	if _, _, _, ok := httpCoverageOf(out, "svc-beta"); ok {
		t.Errorf("svc-beta unexpectedly has edge_coverage; it makes no client calls")
	}
}

// --- (B) import linking ---

func TestComputeLinks_ImportMatch(t *testing.T) {
	in := []facts.Fact{
		importDep("app-web-app", "@app-web/lib-api/api-client-util"),
		facts.Fact{Kind: facts.KindModule, Name: "lib-api", Repo: "app-web"},
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	e := findEdge(out, "app-web-app", "app-web")
	if e == nil {
		t.Fatalf("missing import edge; got %+v", out)
	}
	if via, _ := e.Props["via"].([]string); !reflect.DeepEqual(via, []string{"import"}) {
		t.Errorf("via = %v, want [import]", e.Props["via"])
	}
	if c, _ := e.Props["import_count"].(int); c != 1 {
		t.Errorf("import_count = %v, want 1", e.Props["import_count"])
	}
}

func TestComputeLinks_ImportRubyStyle(t *testing.T) {
	in := []facts.Fact{
		importDep("svc-alpha", "lib-core/money/converter"),
		facts.Fact{Kind: facts.KindModule, Name: "money", Repo: "lib-core"},
	}
	if findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "lib-core") == nil {
		t.Errorf("expected svc-alpha -> lib-core import edge")
	}
}

func TestComputeLinks_ImportRelativeAndSelfIgnored(t *testing.T) {
	in := []facts.Fact{
		importDep("svc-alpha", "./local/thing"),   // relative — skip
		importDep("svc-alpha", "svc-alpha/inner"), // self — skip
		facts.Fact{Kind: facts.KindModule, Name: "x", Repo: "lib-core"},
	}
	if edges := crossRepoEdges(ComputeLinks(in, nil, allSignals(), vocab.Default())); len(edges) != 0 {
		t.Errorf("relative/self imports produced edges: %+v", edges)
	}
}

// TestComputeLinks_ImportIntraRepoNamespaceSkipped guards the reverse-DNS false
// positive: a consumer's own file whose path embeds another repo's short label as
// an interior namespace segment (e.g. "de/backend/app/..." in an app whose top-level
// source dir is "app", vs a backend repo "backend") must NOT fabricate an import
// edge, while a genuine deep cross-repo path still links.
func TestComputeLinks_ImportIntraRepoNamespaceSkipped(t *testing.T) {
	in := []facts.Fact{
		// consumer "mobile" declares a top-level "app" source dir and imports its own
		// file whose path contains the interior segment "backend".
		module("mobile", "app/src/main/java/de/backend/app/ui"),
		importDep("mobile", "app/src/main/java/de/backend/app/ui/Screen"),
		// a real backend repo happens to be labeled "backend".
		serverRoute("backend", "/api/items/{id}", "GET"),
		// a genuine deep cross-repo import still resolves (leading seg not a mobile dir).
		importDep("mobile", "github.com/x/go-auth/adapters"),
		module("go-auth", "adapters"),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if findEdge(out, "mobile", "backend") != nil {
		t.Errorf("intra-repo namespace path must not link to a same-named repo; got edge to backend")
	}
	if findEdge(out, "mobile", "go-auth") == nil {
		t.Errorf("a genuine deep cross-repo import should still resolve to go-auth")
	}
}

// TestComputeLinks_ImportSelfNamedTargetSkipped covers an app whose own module is
// named the same short token as a backend repo (e.g. a mobile app target "acme"
// alongside a backend repo also labeled "acme"): importing it is intra-repo, not a
// call to the backend.
func TestComputeLinks_ImportSelfNamedTargetSkipped(t *testing.T) {
	in := []facts.Fact{
		module("app-ios", "acme"), // the app's own target module
		importDep("app-ios", "acme"),
		serverRoute("acme", "/api/items/{id}", "GET"),
	}
	if findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "app-ios", "acme") != nil {
		t.Errorf("importing the app's own same-named module must not link to the backend repo")
	}
}

// moduleWithPackage is a module fact carrying the npm package its directory
// belongs to, as the TypeScript extractor emits it.
func moduleWithPackage(repo, name, pkg string) facts.Fact {
	return facts.Fact{
		Kind:  facts.KindModule,
		Name:  name,
		Repo:  repo,
		Props: map[string]any{"package_name": pkg},
	}
}

// A repo that publishes under @cognee importing @cognee/neon-darwin-arm64 is
// pulling in a sibling package of its own — not depending on a repo that happens
// to be labeled "cognee". The scope is a namespace, so it never appears among the
// repo's source directories and the ownDirs guard cannot see it.
func TestComputeLinks_ImportOwnScopeSkipped(t *testing.T) {
	in := []facts.Fact{
		moduleWithPackage("cognee-rs", "ts/src", "@cognee/cognee-ts"),
		importDep("cognee-rs", "@cognee/neon-darwin-arm64"),
		module("cognee", "api"),
		serverRoute("cognee", "/api/items/{id}", "GET"),
	}
	if e := findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "cognee-rs", "cognee"); e != nil {
		t.Errorf("importing a package under the repo's own scope linked to another repo: %+v", e)
	}
}

// The guard is per consumer: a repo that does NOT publish under @app-web still
// gets an edge from importing that scope, which is the monorepo-org case the
// import signal exists for.
func TestComputeLinks_ImportForeignScopeStillLinks(t *testing.T) {
	in := []facts.Fact{
		moduleWithPackage("app-ios", "src", "@app-ios/mobile"),
		importDep("app-ios", "@app-web/lib-api"),
		module("app-web", "lib-api"),
	}
	if findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "app-ios", "app-web") == nil {
		t.Errorf("import of another repo's scope must still link")
	}
}

// --- (A+B) merge + determinism ---

func TestComputeLinks_MergedViaAndDeterministic(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/{id}", "GET", nil),
		serverRoute("svc-beta", "/api/items/{id}", "GET"),
		importDep("svc-alpha", "svc-beta/client"),
		facts.Fact{Kind: facts.KindModule, Name: "client", Repo: "svc-beta"},
	}
	out1 := ComputeLinks(in, nil, allSignals(), vocab.Default())
	e := findEdge(out1, "svc-alpha", "svc-beta")
	if e == nil {
		t.Fatalf("missing merged edge: %+v", out1)
	}
	if via, _ := e.Props["via"].([]string); !reflect.DeepEqual(via, []string{"http", "import"}) {
		t.Errorf("via = %v, want [http import]", e.Props["via"])
	}

	// Deterministic: identical input yields identical output.
	out2 := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if !reflect.DeepEqual(out1, out2) {
		t.Errorf("ComputeLinks not deterministic:\n%+v\n%+v", out1, out2)
	}
}

func TestComputeLinks_SingleRepoNoLinks(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/{id}", "GET", nil),
		serverRoute("svc-alpha", "/api/items/{id}", "GET"),
	}
	if out := ComputeLinks(in, nil, allSignals(), vocab.Default()); out != nil {
		t.Errorf("single repo produced links: %+v", out)
	}
}

// --- (A) suffix-aware HTTP matching ---

// TestComputeLinks_HTTPWildcardServerMethod covers a programmatic servlet (or verb-less
// mapping) server route, emitted with method=facts.MethodAny because it serves every
// verb: a concrete-method client call must resolve to it. This is the linker half of
// the JVM programmatic-route fix — the Kotlin extractor emits such routes for the
// .addServlet("/path", handler) DSL, and without this a GET caller would not bind.
func TestComputeLinks_HTTPWildcardServerMethod(t *testing.T) {
	in := []facts.Fact{
		serverRoute("svc-beta", "/v1/widgets/details", facts.MethodAny),
		clientRoute("svc-alpha", "v1/widgets/details", "GET", nil),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	e := findEdge(out, "svc-alpha", "svc-beta")
	if e == nil {
		t.Fatalf("GET client should resolve to a wildcard-method server route; edges=%+v", crossRepoEdges(out))
	}
	if via, _ := e.Props["via"].([]string); len(via) == 0 || via[0] != "http" {
		t.Errorf("via = %v, want [http]", e.Props["via"])
	}
	if !hasServiceEdge(out, "svc-alpha", "svc-beta") {
		t.Errorf("svc-alpha service node missing depends_on svc-beta")
	}
}

// topicFact builds a Kafka topic-reference fact as the extractors emit it.
func topicFact(repo, topic string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindStorage,
		Name: topic,
		Repo: repo,
		Props: map[string]any{
			"storage_kind": facts.StorageKindTopic,
			"messaging":    "kafka",
		},
	}
}

// TestComputeLinks_Kafka covers async binding: a repo referencing another loaded
// repo's topic (identified by the topic name's owning-service prefix) depends on it;
// a repo's own topic and a topic owned by no loaded repo draw no edge.
func TestComputeLinks_Kafka(t *testing.T) {
	in := []facts.Fact{
		module("svc-alpha", "app"),                             // consumer
		module("svc-beta", "app"),                              // producer / owner
		topicFact("svc-alpha", "svc-beta.things_updated"),      // alpha consumes beta's topic
		topicFact("svc-alpha", "svc-alpha.cache.v1.evictions"), // alpha's own topic — no edge
		topicFact("svc-alpha", "sink.third_party.user_state"),  // owner not loaded — no edge
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	e := findEdge(out, "svc-alpha", "svc-beta")
	if e == nil {
		t.Fatalf("svc-alpha should depend on svc-beta via kafka; edges=%+v", crossRepoEdges(out))
	}
	if via, _ := e.Props["via"].([]string); len(via) == 0 || via[0] != "kafka" {
		t.Errorf("via = %v, want [kafka]", e.Props["via"])
	}
	if !hasServiceEdge(out, "svc-alpha", "svc-beta") {
		t.Errorf("svc-alpha service node missing depends_on svc-beta")
	}
	// Only svc-alpha -> svc-beta: the self-topic and unloaded-owner topic draw nothing.
	if edges := crossRepoEdges(out); len(edges) != 1 {
		t.Errorf("expected exactly 1 kafka edge, got %d: %+v", len(edges), edges)
	}
}

func TestComputeLinks_HTTPSuffixMatch(t *testing.T) {
	// golf serves the full /api/settings path; consumers call it with varying
	// prefixes (Swift base-relative, Kotlin/TS with /api). All must link to golf.
	in := []facts.Fact{
		serverRoute("golf", "/api/settings/entitlements/definitions", "GET"),
		clientRoute("ios", "settings/entitlements/definitions", "GET", nil), // base-relative, no slash
		clientRoute("android", "/api/settings/entitlements/definitions", "GET", nil),
		clientRoute("golf-ui", "/settings/entitlements/definitions", "GET", nil), // leading slash, no /api
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	for _, consumer := range []string{"ios", "android", "golf-ui"} {
		if findEdge(out, consumer, "golf") == nil {
			t.Errorf("%s did not link to golf via suffix match; out=%+v", consumer, out)
		}
		if !hasServiceEdge(out, consumer, "golf") {
			t.Errorf("%s service node missing depends_on golf", consumer)
		}
	}
}

func TestComputeLinks_HTTPSuffixMinSegments(t *testing.T) {
	// A single trailing segment is below minSharedSegments → no link.
	in := []facts.Fact{
		serverRoute("golf", "/api/settings/definitions", "GET"),
		clientRoute("ios", "definitions", "GET", nil),
	}
	if edges := crossRepoEdges(ComputeLinks(in, nil, allSignals(), vocab.Default())); len(edges) != 0 {
		t.Errorf("single-segment suffix should not link: %+v", edges)
	}
}

func TestComputeLinks_HTTPSuffixMethodMismatch(t *testing.T) {
	in := []facts.Fact{
		serverRoute("golf", "/api/settings/feedback", "GET"),
		clientRoute("golf-ui", "settings/feedback", "POST", nil),
	}
	if edges := crossRepoEdges(ComputeLinks(in, nil, allSignals(), vocab.Default())); len(edges) != 0 {
		t.Errorf("method mismatch should not link: %+v", edges)
	}
}

// --- (A2) per-repo service nodes ---

func TestComputeLinks_PerRepoServiceNodes(t *testing.T) {
	// Five loaded repos but only one real edge (golf -> go-auth import). Every
	// repo must still get an addressable service node; edgeless ones carry no
	// depends_on relations.
	in := []facts.Fact{
		importDep("golf", "github.com/x/go-auth/adapters"),
		facts.Fact{Kind: facts.KindModule, Name: "adapters", Repo: "go-auth"},
		facts.Fact{Kind: facts.KindModule, Name: "src/ui", Repo: "golf-ui"},
		facts.Fact{Kind: facts.KindModule, Name: "app", Repo: "android"},
		facts.Fact{Kind: facts.KindModule, Name: "App", Repo: "ios"},
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	got := serviceNodes(out)
	want := []string{"android", "go-auth", "golf", "golf-ui", "ios"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("service nodes = %v, want %v", got, want)
	}
	if findEdge(out, "golf", "go-auth") == nil {
		t.Errorf("expected the golf -> go-auth import edge to remain")
	}
	// An edgeless repo's service node has no depends_on relations.
	for _, f := range out {
		if f.Kind == facts.KindService && f.Name == "golf-ui" && len(f.Relations) != 0 {
			t.Errorf("isolated repo golf-ui should have no relations, got %+v", f.Relations)
		}
	}
}

// --- (C) shared symbol surface ---

func module(repo, name string) facts.Fact {
	return facts.Fact{Kind: facts.KindModule, Name: name, Repo: repo}
}

func typeSym(repo, name, kind string) facts.Fact {
	return facts.Fact{
		Kind:  facts.KindSymbol,
		Name:  name,
		Repo:  repo,
		Props: map[string]any{"symbol_kind": kind},
	}
}

func TestComputeLinks_SharedSymbolsMatch(t *testing.T) {
	// getdp and gmsh both declare the vendored onelab/GmshSocket types, under
	// different directory prefixes (src/common vs Common). Enough distinctive shared
	// types must record a SYMMETRIC coupling — but no dependency edge: neither repo
	// imports or calls the other, and a depends_on relation would let traversal
	// compose a path through the pair that does not exist.
	in := []facts.Fact{
		module("getdp", "src/common"),
		typeSym("getdp", "src/common.GmshClient", facts.SymbolClass),
		typeSym("getdp", "src/common.GmshServer", facts.SymbolClass),
		typeSym("getdp", "src/common.onelab::remoteNetworkClient", facts.SymbolClass),
		module("gmsh", "Common"),
		typeSym("gmsh", "Common.GmshClient", facts.SymbolClass),
		typeSym("gmsh", "Common.GmshServer", facts.SymbolClass),
		typeSym("gmsh", "Common.onelab::remoteNetworkClient", facts.SymbolClass),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	sc := findSharedCode(out, "getdp", "gmsh")
	if sc == nil {
		t.Fatalf("missing shared-code coupling for getdp/gmsh; out=%+v", out)
	}
	if via, _ := sc.Props["via"].([]string); !reflect.DeepEqual(via, []string{"shared_symbols"}) {
		t.Errorf("via = %v, want [shared_symbols]", sc.Props["via"])
	}
	if c, _ := sc.Props["symbol_count"].(int); c != 3 {
		t.Errorf("symbol_count = %v, want 3", sc.Props["symbol_count"])
	}
	if repos, _ := sc.Props["repos"].([]string); !reflect.DeepEqual(repos, []string{"getdp", "gmsh"}) {
		t.Errorf("repos = %v, want [getdp gmsh] (canonical, lexicographic)", sc.Props["repos"])
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("shared code alone must not create a dependency edge: %+v", edges)
	}
	if anyServiceEdge(out) {
		t.Error("shared code alone must not attach a depends_on relation to any service node")
	}
}

func TestComputeLinks_SharedSymbolsBelowThreshold(t *testing.T) {
	// Only one distinctive shared type (below minSharedSymbols) → no link.
	in := []facts.Fact{
		module("alpha", "core"),
		typeSym("alpha", "core.WidgetRegistry", facts.SymbolClass),
		module("beta", "lib"),
		typeSym("beta", "lib.WidgetRegistry", facts.SymbolClass),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if sc := sharedCodeFacts(out); len(sc) != 0 {
		t.Errorf("must not couple the repos either: %+v", sc)
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("single shared type should not link: %+v", edges)
	}
}

func TestComputeLinks_SharedSymbolsGenericNamesIgnored(t *testing.T) {
	// Common generic/short unqualified type names are not distinctive enough to
	// link two otherwise-unrelated repos, even at count >= threshold.
	in := []facts.Fact{
		module("alpha", "core"),
		typeSym("alpha", "core.Config", facts.SymbolClass),
		typeSym("alpha", "core.Error", facts.SymbolStruct),
		typeSym("alpha", "core.Node", facts.SymbolClass),
		typeSym("alpha", "core.Item", facts.SymbolClass),
		module("beta", "lib"),
		typeSym("beta", "lib.Config", facts.SymbolClass),
		typeSym("beta", "lib.Error", facts.SymbolStruct),
		typeSym("beta", "lib.Node", facts.SymbolClass),
		typeSym("beta", "lib.Item", facts.SymbolClass),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if sc := sharedCodeFacts(out); len(sc) != 0 {
		t.Errorf("must not couple the repos either: %+v", sc)
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("generic shared names should not link: %+v", edges)
	}
}

func TestComputeLinks_SharedSymbolsFrameworkConventionIgnored(t *testing.T) {
	// Two Rails apps sharing only framework-convention base classes (which every
	// Rails app declares identically) must not link — that is boilerplate, not
	// shared code. Ability (CanCanCan) and the namespaced ApplicationCable::* are
	// excluded too.
	in := []facts.Fact{
		module("svc-a", "app"),
		typeSym("svc-a", "app.ApplicationController", facts.SymbolClass),
		typeSym("svc-a", "app.ApplicationRecord", facts.SymbolClass),
		typeSym("svc-a", "app.ApplicationJob", facts.SymbolClass),
		typeSym("svc-a", "app.Ability", facts.SymbolClass),
		typeSym("svc-a", "app.ApplicationCable::Connection", facts.SymbolClass),
		module("svc-b", "app"),
		typeSym("svc-b", "app.ApplicationController", facts.SymbolClass),
		typeSym("svc-b", "app.ApplicationRecord", facts.SymbolClass),
		typeSym("svc-b", "app.ApplicationJob", facts.SymbolClass),
		typeSym("svc-b", "app.Ability", facts.SymbolClass),
		typeSym("svc-b", "app.ApplicationCable::Connection", facts.SymbolClass),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if sc := sharedCodeFacts(out); len(sc) != 0 {
		t.Errorf("must not couple the repos either: %+v", sc)
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("framework-convention boilerplate should not link: %+v", edges)
	}
}

// migrationSym is a class symbol declared in a Rails migration file.
func migrationSym(repo, name string) facts.Fact {
	f := typeSym(repo, name, facts.SymbolClass)
	f.File = repo + "/db/migrate/20230101000000_" + name + ".rb"
	return f
}

func TestComputeLinks_SharedSymbolsMigrationsIgnored(t *testing.T) {
	// Auto-generated migration classes coincide when two apps ran the same
	// migration (parallel schema history), not shared code — no link.
	in := []facts.Fact{
		module("svc-a", "db/migrate"),
		migrationSym("svc-a", "CreateWidgets"),
		migrationSym("svc-a", "AddIndexToWidgets"),
		migrationSym("svc-a", "InitSchema"),
		module("svc-b", "db/migrate"),
		migrationSym("svc-b", "CreateWidgets"),
		migrationSym("svc-b", "AddIndexToWidgets"),
		migrationSym("svc-b", "InitSchema"),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if sc := sharedCodeFacts(out); len(sc) != 0 {
		t.Errorf("must not couple the repos either: %+v", sc)
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("shared migration class names should not link: %+v", edges)
	}
}

func TestComputeLinks_SharedSymbolsGenuineSurvivesBoilerplate(t *testing.T) {
	// Boilerplate + migrations mixed with >=3 genuine distinctive shared types: the
	// edge is still drawn, and symbol_count reflects ONLY the genuine types.
	in := []facts.Fact{
		module("svc-a", "app"),
		typeSym("svc-a", "app.ApplicationController", facts.SymbolClass), // excluded
		typeSym("svc-a", "app.Ability", facts.SymbolClass),               // excluded
		migrationSym("svc-a", "InitSchema"),                              // excluded
		typeSym("svc-a", "app.WidgetRegistry", facts.SymbolClass),        // genuine
		typeSym("svc-a", "app.PaymentLedger", facts.SymbolClass),         // genuine
		typeSym("svc-a", "app.RetryPolicy", facts.SymbolClass),           // genuine
		module("svc-b", "app"),
		typeSym("svc-b", "app.ApplicationController", facts.SymbolClass),
		typeSym("svc-b", "app.Ability", facts.SymbolClass),
		migrationSym("svc-b", "InitSchema"),
		typeSym("svc-b", "app.WidgetRegistry", facts.SymbolClass),
		typeSym("svc-b", "app.PaymentLedger", facts.SymbolClass),
		typeSym("svc-b", "app.RetryPolicy", facts.SymbolClass),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	sc := findSharedCode(out, "svc-a", "svc-b")
	if sc == nil {
		t.Fatalf("genuine shared types should still couple the pair; out=%+v", out)
	}
	if c, _ := sc.Props["symbol_count"].(int); c != 3 {
		t.Errorf("symbol_count = %v, want 3 (only genuine types, boilerplate/migrations excluded)", c)
	}
}

func TestComputeLinks_SharedSymbolsNonTypesIgnored(t *testing.T) {
	// Functions/methods/variables are not the contract surface; sharing them
	// (even many) must not link repos.
	in := []facts.Fact{
		module("alpha", "core"),
		typeSym("alpha", "core.processRequest", facts.SymbolFunc),
		typeSym("alpha", "core.parseHeader", facts.SymbolFunc),
		typeSym("alpha", "core.computeChecksum", facts.SymbolFunc),
		module("beta", "lib"),
		typeSym("beta", "lib.processRequest", facts.SymbolFunc),
		typeSym("beta", "lib.parseHeader", facts.SymbolFunc),
		typeSym("beta", "lib.computeChecksum", facts.SymbolFunc),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if sc := sharedCodeFacts(out); len(sc) != 0 {
		t.Errorf("must not couple the repos either: %+v", sc)
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("shared non-type symbols should not link: %+v", edges)
	}
}

func typeSymLang(repo, name, kind, lang string) facts.Fact {
	f := typeSym(repo, name, kind)
	f.Props["language"] = lang
	return f
}

// TestComputeLinks_SharedSymbolsCrossLanguageUnqualifiedSkipped covers the parallel-
// app false positive: two apps in different languages declaring the same plain
// domain type names (e.g. Kotlin and Swift both defining LoginViewModel/FeedItem/
// AccountViewModel) are modeling the same product, not sharing code — no link.
func TestComputeLinks_SharedSymbolsCrossLanguageUnqualifiedSkipped(t *testing.T) {
	in := []facts.Fact{
		module("android", "app"),
		typeSymLang("android", "app.LoginViewModel", facts.SymbolClass, "kotlin"),
		typeSymLang("android", "app.FeedItem", facts.SymbolClass, "kotlin"),
		typeSymLang("android", "app.AccountViewModel", facts.SymbolClass, "kotlin"),
		module("ios", "Sources"),
		typeSymLang("ios", "Sources.LoginViewModel", facts.SymbolClass, "swift"),
		typeSymLang("ios", "Sources.FeedItem", facts.SymbolClass, "swift"),
		typeSymLang("ios", "Sources.AccountViewModel", facts.SymbolClass, "swift"),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if sc := sharedCodeFacts(out); len(sc) != 0 {
		t.Errorf("must not couple the repos either: %+v", sc)
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("unqualified domain types shared across languages must not link: %+v", edges)
	}
}

// TestComputeLinks_SharedSymbolsSameLanguageUnqualifiedLinks confirms the fix does
// not over-reach: enough unqualified types shared between SAME-language repos still
// link (genuine copied source).
func TestComputeLinks_SharedSymbolsSameLanguageUnqualifiedLinks(t *testing.T) {
	in := []facts.Fact{
		module("svc-a", "core"),
		typeSymLang("svc-a", "core.WidgetRegistry", facts.SymbolClass, "go"),
		typeSymLang("svc-a", "core.PaymentLedger", facts.SymbolClass, "go"),
		typeSymLang("svc-a", "core.RetryPolicy", facts.SymbolClass, "go"),
		module("svc-b", "lib"),
		typeSymLang("svc-b", "lib.WidgetRegistry", facts.SymbolClass, "go"),
		typeSymLang("svc-b", "lib.PaymentLedger", facts.SymbolClass, "go"),
		typeSymLang("svc-b", "lib.RetryPolicy", facts.SymbolClass, "go"),
	}
	if findSharedCode(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-a", "svc-b") == nil {
		t.Errorf("same-language repos sharing distinctive types should still couple")
	}
}

// TestComputeLinks_SharedSymbolsQualifiedCrossLanguageLinks confirms a namespace-
// qualified identity (the vendored/shared-source signal) links regardless of the
// per-repo dominant language.
func TestComputeLinks_SharedSymbolsQualifiedCrossLanguageLinks(t *testing.T) {
	in := []facts.Fact{
		module("repo-a", "src"),
		typeSymLang("repo-a", "src.onelab::clientA", facts.SymbolClass, "cpp"),
		typeSymLang("repo-a", "src.onelab::clientB", facts.SymbolClass, "cpp"),
		typeSymLang("repo-a", "src.onelab::clientC", facts.SymbolClass, "cpp"),
		module("repo-b", "vendor"),
		typeSymLang("repo-b", "vendor.onelab::clientA", facts.SymbolClass, "c"),
		typeSymLang("repo-b", "vendor.onelab::clientB", facts.SymbolClass, "c"),
		typeSymLang("repo-b", "vendor.onelab::clientC", facts.SymbolClass, "c"),
	}
	if findSharedCode(ComputeLinks(in, nil, allSignals(), vocab.Default()), "repo-a", "repo-b") == nil {
		t.Errorf("qualified shared identities should couple even across languages")
	}
}

// TestComputeLinks_SharedSymbolsCrossLanguageNestedTypesSkipped covers the parallel-
// app false positive that survives module stripping: Kotlin and Swift both name a
// nested type "Outer.Inner", so the residual dot is nesting, not a namespace. A
// Kotlin and a Swift app sharing no source must not link on it.
func TestComputeLinks_SharedSymbolsCrossLanguageNestedTypesSkipped(t *testing.T) {
	in := []facts.Fact{
		module("android", "app"),
		typeSymLang("android", "app.RegisterUseCase.ValidationError", facts.SymbolClass, "kotlin"),
		typeSymLang("android", "app.HandicapAnalytics.DifferentialEntry", facts.SymbolClass, "kotlin"),
		typeSymLang("android", "app.FullAnalysisDataBuilder.TimeWindow", facts.SymbolClass, "kotlin"),
		module("ios", "Sources"),
		typeSymLang("ios", "Sources.RegisterUseCase.ValidationError", facts.SymbolClass, "swift"),
		typeSymLang("ios", "Sources.HandicapAnalytics.DifferentialEntry", facts.SymbolClass, "swift"),
		typeSymLang("ios", "Sources.FullAnalysisDataBuilder.TimeWindow", facts.SymbolClass, "swift"),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if sc := sharedCodeFacts(out); len(sc) != 0 {
		t.Errorf("must not couple the repos either: %+v", sc)
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("nested types shared across languages must not link: %+v", edges)
	}
}

// TestComputeLinks_SharedSymbolsSameLanguageNestedTypesLink confirms the nested-type
// fix does not over-reach. Once a residual dot no longer counts as a namespace, a
// nested identity is unqualified and so falls under the same-language guard —
// between two same-language repos it must still link (genuine copied source).
func TestComputeLinks_SharedSymbolsSameLanguageNestedTypesLink(t *testing.T) {
	in := []facts.Fact{
		module("app-a", "app"),
		typeSymLang("app-a", "app.RegisterUseCase.ValidationError", facts.SymbolClass, "kotlin"),
		typeSymLang("app-a", "app.HandicapAnalytics.DifferentialEntry", facts.SymbolClass, "kotlin"),
		typeSymLang("app-a", "app.FullAnalysisDataBuilder.TimeWindow", facts.SymbolClass, "kotlin"),
		module("app-b", "lib"),
		typeSymLang("app-b", "lib.RegisterUseCase.ValidationError", facts.SymbolClass, "kotlin"),
		typeSymLang("app-b", "lib.HandicapAnalytics.DifferentialEntry", facts.SymbolClass, "kotlin"),
		typeSymLang("app-b", "lib.FullAnalysisDataBuilder.TimeWindow", facts.SymbolClass, "kotlin"),
	}
	if findSharedCode(ComputeLinks(in, nil, allSignals(), vocab.Default()), "app-a", "app-b") == nil {
		t.Errorf("same-language repos sharing distinctive nested types should still couple")
	}
}

// fleetRepos is a set of same-language repo labels large enough (>=
// minReposForVocabFilter) to trigger the shared-vocabulary filter.
var fleetRepos = []string{"svc-1", "svc-2", "svc-3", "svc-4", "svc-5", "svc-6", "svc-7", "svc-8"}

// TestComputeLinks_SharedSymbolsFleetVocabularyNotLinked covers the multi-repo
// fleet false positive: a set of same-language services that each independently
// model the same distinctive domain types (TranslationBundle, CategoryTree,
// FilterFacet) share those names across most of the fleet. That is parallel
// modeling, not shared code, so no pair may be linked on ubiquitous vocabulary
// alone — even though each name is distinctive enough to link a bare 2-repo pair.
func TestComputeLinks_SharedSymbolsFleetVocabularyNotLinked(t *testing.T) {
	fleetNames := []string{"TranslationBundle", "CategoryTree", "FilterFacet"}
	var in []facts.Fact
	for _, repo := range fleetRepos {
		in = append(in, module(repo, "app"))
		for _, name := range fleetNames {
			in = append(in, typeSymLang(repo, "app."+name, facts.SymbolClass, "go"))
		}
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if sc := sharedCodeFacts(out); len(sc) != 0 {
		t.Errorf("must not couple the repos either: %+v", sc)
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("fleet-wide shared vocabulary must not link any pair: %+v", edges)
	}
}

// TestComputeLinks_SharedSymbolsPairSpecificSurvivesFleetVocabulary confirms the
// vocabulary filter is surgical: amid fleet-wide vocabulary (dropped), two
// services that additionally share distinctive types declared by no other repo
// are still linked, the symbol_count reflects only those pair-specific types, and
// no other pair is fabricated.
func TestComputeLinks_SharedSymbolsPairSpecificSurvivesFleetVocabulary(t *testing.T) {
	fleetNames := []string{"TranslationBundle", "CategoryTree", "FilterFacet"}
	pairOnly := []string{"InvoiceReconciler", "ShipmentManifest", "LoyaltyLedger"}
	var in []facts.Fact
	for _, repo := range fleetRepos {
		in = append(in, module(repo, "app"))
		for _, name := range fleetNames {
			in = append(in, typeSymLang(repo, "app."+name, facts.SymbolClass, "go"))
		}
	}
	for _, name := range pairOnly {
		in = append(in, typeSymLang("svc-1", "app."+name, facts.SymbolClass, "go"))
		in = append(in, typeSymLang("svc-2", "app."+name, facts.SymbolClass, "go"))
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	sc := findSharedCode(out, "svc-1", "svc-2")
	if sc == nil {
		t.Fatalf("pair-specific distinctive types should couple svc-1/svc-2; got=%+v", sharedCodeFacts(out))
	}
	if c, _ := sc.Props["symbol_count"].(int); c != 3 {
		t.Errorf("symbol_count = %v, want 3 (only pair-specific types; fleet vocabulary dropped)", c)
	}
	// Shared code is symmetric, so the one real pair is a SINGLE coupling fact — and
	// nothing else, since every other overlap is ubiquitous vocabulary.
	if sc := sharedCodeFacts(out); len(sc) != 1 {
		t.Errorf("only the svc-1/svc-2 pair should couple, got %d: %+v", len(sc), sc)
	}
}

// TestComputeLinks_SharedSymbolsSmallRepoSetVocabularyFilterOff confirms the guard:
// below minReposForVocabFilter the filter never fires, so a type shared across all
// of a small repo set still links (a vendored header may legitimately appear in
// every one of just three repos).
func TestComputeLinks_SharedSymbolsSmallRepoSetVocabularyFilterOff(t *testing.T) {
	types := []string{"WidgetRegistry", "PaymentLedger", "RetryPolicy"}
	var in []facts.Fact
	for _, repo := range []string{"svc-a", "svc-b", "svc-c"} {
		in = append(in, module(repo, "app"))
		for _, name := range types {
			in = append(in, typeSymLang(repo, "app."+name, facts.SymbolClass, "go"))
		}
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	for _, pair := range [][2]string{{"svc-a", "svc-b"}, {"svc-a", "svc-c"}, {"svc-b", "svc-c"}} {
		if findSharedCode(out, pair[0], pair[1]) == nil {
			t.Errorf("with only 3 repos the vocabulary filter must stay off; missing %s <-> %s", pair[0], pair[1])
		}
	}
}

// genTypeSym is a struct declared in a generated client/model file (the stub a
// codegen tool emits from an upstream contract).
func genTypeSym(repo, name string) facts.Fact {
	f := typeSymLang(repo, "internal."+name, facts.SymbolClass, "go")
	f.File = repo + "/internal/clients/eventcountersapi/event_counters_client.gen.go"
	return f
}

// TestComputeLinks_SharedSymbolsGeneratedCodeIgnored covers the shared-upstream-
// contract false positive: two services that generate a client for the SAME API
// each emit identically-named generated structs. Those coincide across every
// consumer of that API but are not shared code between the consumers, so they must
// not fabricate a cross-repo edge — even many of them, in the same language.
func TestComputeLinks_SharedSymbolsGeneratedCodeIgnored(t *testing.T) {
	in := []facts.Fact{module("svc-a", "internal"), module("svc-b", "internal")}
	for _, name := range []string{"GetCountersResponse", "CounterBatchRequest", "EventCounterModel", "CountersClientWithResponses"} {
		in = append(in, genTypeSym("svc-a", name), genTypeSym("svc-b", name))
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if sc := sharedCodeFacts(out); len(sc) != 0 {
		t.Errorf("must not couple the repos either: %+v", sc)
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("generated client structs from a shared API must not link consumers: %+v", edges)
	}
}

// TestComputeLinks_SharedSymbolsGeneratedExcludedFromCount confirms the exclusion is
// surgical: generated stubs are dropped, but hand-written distinctive shared types in
// the same repos still link, and symbol_count reflects only those hand-written types.
func TestComputeLinks_SharedSymbolsGeneratedExcludedFromCount(t *testing.T) {
	in := []facts.Fact{module("svc-a", "internal"), module("svc-b", "internal")}
	for _, name := range []string{"GetCountersResponse", "CounterBatchRequest", "EventCounterModel"} {
		in = append(in, genTypeSym("svc-a", name), genTypeSym("svc-b", name))
	}
	for _, name := range []string{"WidgetRegistry", "PaymentLedger", "RetryPolicy"} {
		in = append(in,
			typeSymLang("svc-a", "internal."+name, facts.SymbolClass, "go"),
			typeSymLang("svc-b", "internal."+name, facts.SymbolClass, "go"))
	}
	e := findSharedCode(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-a", "svc-b")
	if e == nil {
		t.Fatalf("hand-written distinctive shared types should still couple")
	}
	if c, _ := e.Props["symbol_count"].(int); c != 3 {
		t.Errorf("symbol_count = %v, want 3 (generated stubs excluded)", c)
	}
}

// --- via: http-client tag and confidence ---

func TestComputeLinks_HTTPClientViaTag(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/list", "GET", map[string]any{"source": "go-http-client"}),
		serverRoute("svc-beta", "/api/items/list", "GET"),
	}
	e := findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "svc-beta")
	if e == nil {
		t.Fatal("missing svc-alpha -> svc-beta edge")
	}
	if via, _ := e.Props["via"].([]string); !reflect.DeepEqual(via, []string{"http-client"}) {
		t.Errorf("via = %v, want [http-client]", e.Props["via"])
	}
}

// TestComputeLinks_HandWrittenClientViaTag covers EVERY source registered as
// hand-written, not just the one that happened to have a test. The linker used to keep
// its own copy of this set and silently omitted the two Java sources, so a Spring
// RestTemplate or Feign call linked as a generic via="http" — indistinguishable from an
// edge implied by an OpenAPI spec. Iterating the registry rather than listing sources
// here means a source added to facts.HandWrittenClientSources is covered the moment it
// is registered, which is the property that was missing.
func TestComputeLinks_HandWrittenClientViaTag(t *testing.T) {
	for source := range facts.HandWrittenClientSources {
		t.Run(source, func(t *testing.T) {
			in := []facts.Fact{
				clientRoute("svc-alpha", "/api/items/list", "GET", map[string]any{"source": source}),
				serverRoute("svc-beta", "/api/items/list", "GET"),
			}
			e := findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "svc-beta")
			if e == nil {
				t.Fatal("missing svc-alpha -> svc-beta edge")
			}
			if via, _ := e.Props["via"].([]string); !reflect.DeepEqual(via, []string{"http-client"}) {
				t.Errorf("via = %v, want [http-client]", e.Props["via"])
			}
		})
	}
}

func TestComputeLinks_OpenAPIViaStaysHTTP(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/list", "GET", map[string]any{"source": "openapi"}),
		serverRoute("svc-beta", "/api/items/list", "GET"),
	}
	e := findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "svc-beta")
	if e == nil {
		t.Fatal("missing edge")
	}
	if via, _ := e.Props["via"].([]string); !reflect.DeepEqual(via, []string{"http"}) {
		t.Errorf("via = %v, want [http]", e.Props["via"])
	}
}

func TestComputeLinks_ConfidenceVerified(t *testing.T) {
	// Sole provider, client calls the full server path, no inferred {}.
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/list", "GET", nil),
		serverRoute("svc-beta", "/api/items/list", "GET"),
	}
	e := findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "svc-beta")
	if e == nil || e.Props["confidence"] != "verified" {
		t.Errorf("confidence = %v, want verified", e.Props["confidence"])
	}
}

func TestComputeLinks_ConfidenceProbableSuffixOnly(t *testing.T) {
	// Client calls a base-relative subpath (suffix), not the full server path.
	in := []facts.Fact{
		clientRoute("svc-alpha", "settings/feedback", "POST", nil),
		serverRoute("svc-beta", "/api/settings/feedback", "POST"),
	}
	e := findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "svc-beta")
	if e == nil || e.Props["confidence"] != "probable" {
		t.Errorf("confidence = %v, want probable", e.Props["confidence"])
	}
}

func TestComputeLinks_ConfidenceProbableInferred(t *testing.T) {
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/{id}", "GET", nil),
		serverRoute("svc-beta", "/api/items/{id}", "GET"),
	}
	e := findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "svc-beta")
	if e == nil || e.Props["confidence"] != "probable" {
		t.Errorf("confidence = %v, want probable (inferred {})", e.Props["confidence"])
	}
}

func TestComputeLinks_ConfidenceProbableHintDisambiguated(t *testing.T) {
	// Two providers serve the path; resolved only via target_hint → probable.
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/list", "GET", map[string]any{"target_hint": "svcbeta"}),
		serverRoute("svc-beta", "/api/items/list", "GET"),
		serverRoute("svc-other", "/api/items/list", "GET"),
	}
	e := findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "svc-beta")
	if e == nil {
		t.Fatal("missing svc-alpha -> svc-beta edge")
	}
	if e.Props["confidence"] != "probable" {
		t.Errorf("confidence = %v, want probable", e.Props["confidence"])
	}
	if findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "svc-other") != nil {
		t.Error("should not link to svc-other")
	}
}

func TestComputeLinks_ConfidenceMixedIsVerified(t *testing.T) {
	// One verified endpoint + one probable endpoint between the same pair → verified.
	in := []facts.Fact{
		clientRoute("svc-alpha", "/api/items/list", "GET", nil),    // verified
		clientRoute("svc-alpha", "settings/feedback", "POST", nil), // probable (suffix)
		serverRoute("svc-beta", "/api/items/list", "GET"),
		serverRoute("svc-beta", "/api/settings/feedback", "POST"),
	}
	e := findEdge(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-alpha", "svc-beta")
	if e == nil || e.Props["confidence"] != "verified" {
		t.Errorf("confidence = %v, want verified", e.Props["confidence"])
	}
}

func TestComputeLinks_TargetHintResolvesProvider(t *testing.T) {
	// target_hint "svccheckout" (from SvcCheckoutClient) resolves svc-checkout.
	in := []facts.Fact{
		clientRoute("core", "/api/purchase/build", "POST", map[string]any{"target_hint": "svccheckout"}),
		serverRoute("svc-checkout", "/api/purchase/build", "POST"),
		serverRoute("svc-other", "/api/purchase/build", "POST"),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if findEdge(out, "core", "svc-checkout") == nil {
		t.Error("target_hint should resolve provider svc-checkout")
	}
	if findEdge(out, "core", "svc-other") != nil {
		t.Error("should not link to svc-other")
	}
}

// --- server-side inverse: routes unused by loaded clients ---

func hasRouteKey(keys map[string]bool, repo, method, path string) bool {
	return keys[routeindex.RouteIdentityKey(repo, method, path)]
}

func TestServerRouteVerdicts_BasicSetDifference(t *testing.T) {
	in := []facts.Fact{
		// golf-ui calls one of golf's two routes.
		clientRoute("golf-ui", "/api/items/{id}", "GET", nil),
		serverRoute("golf", "/api/items/{id}", "GET"),      // called → matched
		serverRoute("golf", "/api/secret/cleanup", "POST"), // no caller → unmatched
	}
	_, keys := httpsignal.ServerRouteVerdicts(routeindex.New(vocab.Default()), in)

	if hasRouteKey(keys, "golf", "GET", "/api/items/{id}") {
		t.Errorf("route called by a client must not be flagged unused; keys=%v", keys)
	}
	if !hasRouteKey(keys, "golf", "POST", "/api/secret/cleanup") {
		t.Errorf("route with no caller must be flagged unused; keys=%v", keys)
	}
	if len(keys) != 1 {
		t.Errorf("expected exactly 1 unmatched route, got %d: %v", len(keys), keys)
	}
}

// The false-positive guard: a client calling with a different leading prefix than
// the server serves (golf-ui "/api/settings/x" vs golf "settings/x", and the
// reverse) must still count as matched — exactly the normalization linkHTTP does.
func TestServerRouteVerdicts_PrefixDifferenceIsMatched(t *testing.T) {
	in := []facts.Fact{
		clientRoute("golf-ui", "/api/settings/feedback", "POST", nil),
		clientRoute("ios", "settings/ai-coach/insight", "POST", nil), // base-relative
		serverRoute("golf", "/api/settings/feedback", "POST"),
		serverRoute("golf", "/api/settings/ai-coach/insight", "POST"),
	}
	_, keys := httpsignal.ServerRouteVerdicts(routeindex.New(vocab.Default()), in)
	if len(keys) != 0 {
		t.Errorf("prefix/base-relative client calls should match their server routes; "+
			"none should be flagged unused, got %v", keys)
	}
}

// A pure-consumer repo (a frontend serving its own page routes while only calling
// another repo's API) is not an HTTP provider, so its own uncalled server routes
// must NOT be flagged — only the provider's (golf's) uncalled routes are.
func TestServerRouteVerdicts_ConsumerOwnRoutesNotFlagged(t *testing.T) {
	in := []facts.Fact{
		// golf-ui calls golf's API (making golf a provider) and serves its own page.
		clientRoute("golf-ui", "/api/items/{id}", "GET", nil),
		serverRoute("golf-ui", "/dashboard/settings", "GET"), // golf-ui's own page, no caller
		serverRoute("golf", "/api/items/{id}", "GET"),        // called by golf-ui
		serverRoute("golf", "/api/secret/cleanup", "POST"),   // no caller
	}
	_, keys := httpsignal.ServerRouteVerdicts(routeindex.New(vocab.Default()), in)
	if hasRouteKey(keys, "golf-ui", "GET", "/dashboard/settings") {
		t.Errorf("a pure-consumer repo's own routes must not be flagged; keys=%v", keys)
	}
	if !hasRouteKey(keys, "golf", "POST", "/api/secret/cleanup") {
		t.Errorf("the provider's uncalled route must still be flagged; keys=%v", keys)
	}
	if len(keys) != 1 {
		t.Errorf("expected exactly 1 unmatched route (golf only), got %d: %v", len(keys), keys)
	}
}

// The evaluated set is the half that says what the pass looked at, and it is not
// the complement of the unmatched set. A UI route, a verbless route and a
// consumer repo's own routes are all absent from both — the verdict "no client
// calls this" was never reached for them, and a caller reading only the unmatched
// set has no way to tell that apart from a route a client happily calls.
func TestServerRouteVerdicts_DeclinedRoutesAreInNeitherSet(t *testing.T) {
	uiRoute := serverRoute("golf-ui", "/dashboard", "GET")
	uiRoute.Props["type"] = "page"
	verbless := serverRoute("golf", "/api/webhooks/stripe", "")
	in := []facts.Fact{
		clientRoute("golf-ui", "/api/items/{id}", "GET", nil),
		serverRoute("golf", "/api/items/{id}", "GET"),
		serverRoute("golf", "/api/secret/cleanup", "POST"),
		serverRoute("golf-ui", "/settings/profile", "GET"),
		uiRoute,
		verbless,
	}
	evaluated, unmatched := httpsignal.ServerRouteVerdicts(routeindex.New(vocab.Default()), in)

	for _, want := range []struct{ repo, method, path string }{
		{"golf", "GET", "/api/items/{id}"},
		{"golf", "POST", "/api/secret/cleanup"},
	} {
		if !hasRouteKey(evaluated, want.repo, want.method, want.path) {
			t.Errorf("%s %s should be evaluated; evaluated=%v", want.method, want.path, evaluated)
		}
	}
	for _, skip := range []struct{ repo, method, path string }{
		{"golf-ui", "GET", "/dashboard"},        // a page, not an HTTP contract
		{"golf", "", "/api/webhooks/stripe"},    // no verb to match on
		{"golf-ui", "GET", "/settings/profile"}, // consumer repo, no clients of its own
	} {
		if hasRouteKey(evaluated, skip.repo, skip.method, skip.path) {
			t.Errorf("%s %s was declined and must not be evaluated; evaluated=%v",
				skip.method, skip.path, evaluated)
		}
		if hasRouteKey(unmatched, skip.repo, skip.method, skip.path) {
			t.Errorf("%s %s was declined and must not be unmatched either; unmatched=%v",
				skip.method, skip.path, unmatched)
		}
	}
}

func TestServerRouteVerdicts_SingleRepoReturnsNil(t *testing.T) {
	in := []facts.Fact{
		serverRoute("golf", "/api/items/{id}", "GET"),
		serverRoute("golf", "/api/secret/cleanup", "POST"),
	}
	if _, keys := httpsignal.ServerRouteVerdicts(routeindex.New(vocab.Default()), in); keys != nil {
		t.Errorf("single-repo snapshot has no clients to be unused by; want nil, got %v", keys)
	}
}

func TestRouteIdentityMatchesKeyHelper(t *testing.T) {
	f := serverRoute("golf", "/api/items/{id}", "get") // lowercase method
	if got, want := routeindex.RouteIdentity(f), routeindex.RouteIdentityKey("golf", "GET", "/api/items/{id}"); got != want {
		t.Errorf("routeindex.RouteIdentity normalized mismatch: got %q want %q", got, want)
	}
}

// TestComputeLinks_ExternalClientStillMatchesLoadedServer pins the fix for the
// linker-ordering half of GAP-LK-02 (v101): isExternalClient was consulted
// before the server match and `continue`d, so a client route tagged external
// could never resolve. That was harmless while only Kotlin/Swift set external
// (always third-party hosts), but the Go extractor now tags a hardcoded
// *internal* host too. Such a route must still resolve to a loaded server:
// external is a fallback bucket for unmatched calls, not a veto on matching.
func TestComputeLinks_ExternalClientStillMatchesLoadedServer(t *testing.T) {
	in := []facts.Fact{
		// external=true, but the host is an internal service that IS loaded.
		clientRoute("consumer", "/v1/things/{id}", "GET", map[string]any{"external": true, "host": "api:8080"}),
		serverRoute("api", "/v1/things/{id}", "GET"),
		// a genuinely third-party external call: no server serves it -> external bucket.
		clientRoute("consumer", "/v1/widgets", "GET", map[string]any{"external": true, "host": "api.example.com"}),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	if !hasServiceEdge(out, "consumer", "api") {
		t.Errorf("external-tagged client route to a loaded server must produce an edge; got %+v", serviceNodes(out))
	}
	ec := edgeCoverageOf(out, "consumer")
	if ec == nil {
		t.Fatalf("no edge_coverage on consumer; got %+v", out)
	}
	// detected 2: one resolves to api, one has no server -> external bucket.
	if ec["detected"] != 2 || ec["resolved"] != 1 || ec["external"] != 1 || ec["unresolved"] != 0 {
		t.Errorf("coverage = detected:%v resolved:%v external:%v unresolved:%v; want 2/1/1/0",
			ec["detected"], ec["resolved"], ec["external"], ec["unresolved"])
	}
}

// TestUnmatchedReasonConstants pins the string values of the Reason* constants. They
// are written verbatim to a client route's "unmatched_reason" prop and are queried by
// agents (query_facts(kind=route, prop=unmatched_reason, prop_value=...)), so a rename
// that changed a value would silently break every such query and desync the doc
// comments that name them. GAP-LK-10: the value "no_match" the comments once claimed is
// deliberately absent — the resolver splits it into method_mismatch and path_unknown.
func TestUnmatchedReasonConstants(t *testing.T) {
	for _, tc := range []struct {
		got, want string
	}{
		{httpsignal.ReasonNoMethod, "no_method"},
		{httpsignal.ReasonGenericPath, "generic_path"},
		{httpsignal.ReasonMethodMismatch, "method_mismatch"},
		{httpsignal.ReasonPathUnknown, "path_unknown"},
	} {
		if tc.got != tc.want {
			t.Errorf("reason constant = %q, want %q", tc.got, tc.want)
		}
	}
}

// --- (C2) shared-symbol direction and convention-name gating ---

// TestComputeLinks_SharedSymbolsRespectsImportDirection covers the library-monorepo
// false positive: an app declares copies of types its published library also declares
// (a vendored/migrated component), so the symmetric shared-symbol signal fires — but
// the app's npm dependency on the library already fixed the direction. Materializing
// the reverse edge would make the library depend on its own consumer, which then
// fabricates paths like library -> app -> app's backend.
func TestComputeLinks_SharedSymbolsRespectsImportDirection(t *testing.T) {
	in := []facts.Fact{
		module("app", "client"),
		importDep("app", "@lib/ui"),
		typeSym("app", "client.GaugePanelProps", facts.SymbolClass),
		typeSym("app", "client.TileProps", facts.SymbolClass),
		typeSym("app", "client.ProbeSlot", facts.SymbolClass),
		module("lib", "libs/ui"),
		typeSym("lib", "libs/ui.GaugePanelProps", facts.SymbolClass),
		typeSym("lib", "libs/ui.TileProps", facts.SymbolClass),
		typeSym("lib", "libs/ui.ProbeSlot", facts.SymbolClass),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	e := findEdge(out, "app", "lib")
	if e == nil {
		t.Fatalf("missing app -> lib edge; out=%+v", out)
	}
	if via, _ := e.Props["via"].([]string); !reflect.DeepEqual(via, []string{"import", "shared_symbols"}) {
		t.Errorf("via = %v, want [import shared_symbols]", e.Props["via"])
	}
	if c, _ := e.Props["symbol_count"].(int); c != 3 {
		t.Errorf("symbol_count = %v, want 3 (shared symbols annotate the directional edge)", c)
	}
	if rev := findEdge(out, "lib", "app"); rev != nil {
		t.Errorf("library must not depend on its consumer; got reverse edge %+v", rev)
	}
	if hasServiceEdge(out, "lib", "app") {
		t.Errorf("lib service node must not carry depends_on app")
	}
}

// TestComputeLinks_SharedSymbolsRespectsHTTPDirection is the same gate for the other
// directional signal: a frontend calling a backend's HTTP API while also sharing type
// names with it must not make the backend depend on the frontend.
func TestComputeLinks_SharedSymbolsRespectsHTTPDirection(t *testing.T) {
	in := []facts.Fact{
		module("web", "client"),
		clientRoute("web", "/api/widgets/registry", "GET", nil),
		typeSym("web", "client.WidgetRegistry", facts.SymbolClass),
		typeSym("web", "client.PaymentLedger", facts.SymbolClass),
		typeSym("web", "client.RetryPolicy", facts.SymbolClass),
		module("api", "app"),
		serverRoute("api", "/api/widgets/registry", "GET"),
		typeSym("api", "app.WidgetRegistry", facts.SymbolClass),
		typeSym("api", "app.PaymentLedger", facts.SymbolClass),
		typeSym("api", "app.RetryPolicy", facts.SymbolClass),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	if e := findEdge(out, "web", "api"); e == nil {
		t.Fatalf("missing web -> api edge; out=%+v", out)
	}
	if rev := findEdge(out, "api", "web"); rev != nil {
		t.Errorf("backend must not depend on its client; got reverse edge %+v", rev)
	}
}

// TestComputeLinks_SharedSymbolsCoupleWithoutDirection pins what happens when no
// direction is known at all. Sibling forks have no import or HTTP edge either way, so
// there is nothing for the shared symbols to annotate: the pair is recorded as ONE
// symmetric coupling fact, not as two directed edges asserting a mutual dependency
// that does not exist.
func TestComputeLinks_SharedSymbolsCoupleWithoutDirection(t *testing.T) {
	in := []facts.Fact{
		module("fork-a", "client"),
		typeSym("fork-a", "client.WidgetRegistry", facts.SymbolClass),
		typeSym("fork-a", "client.PaymentLedger", facts.SymbolClass),
		typeSym("fork-a", "client.RetryPolicy", facts.SymbolClass),
		module("fork-b", "client"),
		typeSym("fork-b", "client.WidgetRegistry", facts.SymbolClass),
		typeSym("fork-b", "client.PaymentLedger", facts.SymbolClass),
		typeSym("fork-b", "client.RetryPolicy", facts.SymbolClass),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if findSharedCode(out, "fork-a", "fork-b") == nil {
		t.Fatalf("missing shared-code coupling for the fork pair; out=%+v", out)
	}
	if sc := sharedCodeFacts(out); len(sc) != 1 {
		t.Errorf("a symmetric pair must yield exactly one coupling fact, got %d: %+v", len(sc), sc)
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("a fork pair has no dependency in either direction: %+v", edges)
	}
	if anyServiceEdge(out) {
		t.Error("a fork pair must not attach a depends_on relation to any service node")
	}
}

// TestComputeLinks_SharedSymbolsComponentConventionIgnored covers the React/TS analog
// of the Rails-boilerplate false positive: "<Component>Props" is a naming convention,
// so when the component name is itself generic vocabulary the identity says nothing
// about shared code. The three names here were confirmed false positives with zero
// field overlap between their declarations.
func TestComputeLinks_SharedSymbolsComponentConventionIgnored(t *testing.T) {
	in := []facts.Fact{
		module("web-a", "client"),
		typeSym("web-a", "client.SidebarProps", facts.SymbolInterface),
		typeSym("web-a", "client.DialogProps", facts.SymbolInterface),
		typeSym("web-a", "client.PanelSectionProps", facts.SymbolInterface),
		typeSym("web-a", "client.Layout", facts.SymbolClass),
		module("web-b", "libs"),
		typeSym("web-b", "libs.SidebarProps", facts.SymbolInterface),
		typeSym("web-b", "libs.DialogProps", facts.SymbolInterface),
		typeSym("web-b", "libs.PanelSectionProps", facts.SymbolInterface),
		typeSym("web-b", "libs.Layout", facts.SymbolClass),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if sc := sharedCodeFacts(out); len(sc) != 0 {
		t.Errorf("must not couple the repos either: %+v", sc)
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("component-convention names should not link: %+v", edges)
	}
}

// TestComputeLinks_SharedSymbolsDistinctiveComponentNamesSurvive is the guard against
// over-filtering: a "<Component>Props" name whose component is NOT generic vocabulary
// still names real shared code and must keep coupling the pair. Each case here was
// verified to be genuinely migrated/vendored between the repos.
func TestComputeLinks_SharedSymbolsDistinctiveComponentNamesSurvive(t *testing.T) {
	in := []facts.Fact{
		module("web-a", "client"),
		typeSym("web-a", "client.GaugePanelProps", facts.SymbolInterface), // Pin is distinctive
		typeSym("web-a", "client.TileProps", facts.SymbolInterface),       // core "Like" is only 4 chars
		typeSym("web-a", "client.TListRow", facts.SymbolEnum),             // single-char prefix + generic words
		typeSym("web-a", "client.ProbeSlot", facts.SymbolInterface),
		module("web-b", "libs"),
		typeSym("web-b", "libs.GaugePanelProps", facts.SymbolInterface),
		typeSym("web-b", "libs.TileProps", facts.SymbolInterface),
		typeSym("web-b", "libs.TListRow", facts.SymbolEnum),
		typeSym("web-b", "libs.ProbeSlot", facts.SymbolInterface),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	e := findSharedCode(out, "web-a", "web-b")
	if e == nil {
		t.Fatalf("distinctive component names must still couple the pair; out=%+v", out)
	}
	if c, _ := e.Props["symbol_count"].(int); c != 4 {
		t.Errorf("symbol_count = %v, want 4", c)
	}
}

// TestComputeLinks_SharedSymbolsStoryAndTestFilesIgnored pins that declarations in
// stories/tests/mocks are not portable contract surface: a throwaway local type in a
// Storybook story is routinely given the same obvious name in every repo.
func TestComputeLinks_SharedSymbolsStoryAndTestFilesIgnored(t *testing.T) {
	storySym := func(repo, name, file string) facts.Fact {
		f := typeSym(repo, name, facts.SymbolInterface)
		f.File = file
		return f
	}
	in := []facts.Fact{
		module("web-a", "libs"),
		storySym("web-a", "libs.ProbeSlot", "libs/Gallery/Gallery.stories.tsx"),
		storySym("web-a", "libs.WidgetRegistry", "libs/widget/widget.test.ts"),
		storySym("web-a", "libs.PaymentLedger", "libs/__mocks__/ledger.ts"),
		module("web-b", "client"),
		storySym("web-b", "client.ProbeSlot", "client/Gallery/Gallery.stories.tsx"),
		storySym("web-b", "client.WidgetRegistry", "client/widget/widget.test.ts"),
		storySym("web-b", "client.PaymentLedger", "client/__mocks__/ledger.ts"),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())
	if sc := sharedCodeFacts(out); len(sc) != 0 {
		t.Errorf("must not couple the repos either: %+v", sc)
	}
	if edges := crossRepoEdges(out); len(edges) != 0 {
		t.Errorf("story/test/mock declarations should not link: %+v", edges)
	}
}

// TestComputeLinks_SharedSymbolsRespectsGRPCDirection covers a gap the first version of
// the direction gate had: it enumerated the directional evidence kinds and omitted
// "grpc", so a pair whose only real dependency was a gRPC call still had the reverse
// edge fabricated from shared symbols. The gate now tests for "any via that is not
// shared_symbols", which cannot miss a signal added later.
func TestComputeLinks_SharedSymbolsRespectsGRPCDirection(t *testing.T) {
	in := []facts.Fact{
		module("client", "internal"),
		clientRoute("client", "/pkg.Svc/Method", "POST", map[string]any{"framework": "grpc"}),
		typeSym("client", "internal.WidgetRegistry", facts.SymbolClass),
		typeSym("client", "internal.PaymentLedger", facts.SymbolClass),
		typeSym("client", "internal.RetryPolicy", facts.SymbolClass),
		module("server", "internal"),
		serverRoute("server", "/pkg.Svc/Method", "POST"),
		typeSym("server", "internal.WidgetRegistry", facts.SymbolClass),
		typeSym("server", "internal.PaymentLedger", facts.SymbolClass),
		typeSym("server", "internal.RetryPolicy", facts.SymbolClass),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	e := findEdge(out, "client", "server")
	if e == nil {
		t.Fatalf("missing client -> server gRPC edge; out=%+v", out)
	}
	if via, _ := e.Props["via"].([]string); !reflect.DeepEqual(via, []string{"grpc", "shared_symbols"}) {
		t.Errorf("via = %v, want [grpc shared_symbols] (shared symbols annotate the gRPC edge)", e.Props["via"])
	}
	if rev := findEdge(out, "server", "client"); rev != nil {
		t.Errorf("gRPC direction is known, so no reverse edge may be fabricated: %+v", rev)
	}
	// Direction was established, so this is a real dependency — not a coupling.
	if sc := sharedCodeFacts(out); len(sc) != 0 {
		t.Errorf("a directional pair must not also be reported as shared-code coupling: %+v", sc)
	}
}

// TestComputeLinks_SharedCodeDoesNotCompose is the regression test for the failure that
// motivated demoting this signal. A caller reaching one side of a copy-paste pair must
// NOT thereby reach the other side: dependency edges compose across hops, shared code
// does not. Concretely this is the shape "web --(http)--> api" alongside "api <shares
// code with> twin"; web must gain no route to twin.
func TestComputeLinks_SharedCodeDoesNotCompose(t *testing.T) {
	in := []facts.Fact{
		module("web", "client"),
		clientRoute("web", "/api/widgets/registry", "GET", nil),
		module("api", "app"),
		serverRoute("api", "/api/widgets/registry", "GET"),
		typeSym("api", "app.WidgetRegistry", facts.SymbolClass),
		typeSym("api", "app.PaymentLedger", facts.SymbolClass),
		typeSym("api", "app.RetryPolicy", facts.SymbolClass),
		// A sibling fork of api: same distinctive types, no import and no route.
		module("twin", "app"),
		typeSym("twin", "app.WidgetRegistry", facts.SymbolClass),
		typeSym("twin", "app.PaymentLedger", facts.SymbolClass),
		typeSym("twin", "app.RetryPolicy", facts.SymbolClass),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	if findEdge(out, "web", "api") == nil {
		t.Fatalf("the real HTTP dependency must survive; out=%+v", out)
	}
	if findSharedCode(out, "api", "twin") == nil {
		t.Errorf("the api/twin shared code should still be reported as a coupling")
	}
	// The only edge in the graph is web -> api. If api <-> twin were edges, "twin"
	// would become reachable from "web" in two hops.
	edges := crossRepoEdges(out)
	if len(edges) != 1 {
		t.Errorf("expected exactly one dependency edge (web -> api), got %d: %+v", len(edges), edges)
	}
	for _, r := range []string{"api", "twin"} {
		if hasServiceEdge(out, r, "twin") {
			t.Errorf("%s must not carry a depends_on to twin — shared code is not a dependency", r)
		}
	}
}

// --- (C3) source verification of shared type names ---

// fakeSource serves file contents by fact file path, standing in for reading the repo.
func fakeSource(files map[string]string) SourceReader {
	return func(f facts.Fact) (string, bool) {
		text, ok := files[f.File]
		return text, ok
	}
}

// symInFile is a type symbol that records which file declared it, so verification has
// something to compare.
func symInFile(repo, name, file string) facts.Fact {
	f := typeSym(repo, name, facts.SymbolClass)
	f.File = file
	return f
}

const bodyAlpha = `class WidgetRegistry
  def register(widget)
    @widgets << widget
    recompute_index
  end
  def recompute_index
    @index = @widgets.group_by(&:kind)
  end
end`

// bodyAlphaReformatted is the same code with blank lines and indentation churn: a copy
// that drifted only in formatting must still verify.
const bodyAlphaReformatted = `class WidgetRegistry

    def register(widget)
        @widgets << widget
        recompute_index
    end

    def recompute_index
        @index = @widgets.group_by(&:kind)
    end
end`

// bodyBeta shares the class NAME with bodyAlpha and nothing else — the population the
// verification exists to reject.
const bodyBeta = `class WidgetRegistry
  belongs_to :account
  validates :slug, presence: true, uniqueness: { scope: :account_id }
  scope :active, -> { where(archived_at: nil) }
  def to_param
    slug
  end
end`

// TestComputeLinks_SharedSymbolsVerifiedAgainstSource is the core of the change: three
// names match in both repos, but only the ones whose files actually match count. Here
// one pair is genuinely copied and two merely share a name, so the verified total falls
// below minSharedSymbols and no coupling is reported at all.
func TestComputeLinks_SharedSymbolsVerifiedAgainstSource(t *testing.T) {
	in := []facts.Fact{
		module("svc-a", "app"),
		symInFile("svc-a", "app.WidgetRegistry", "svc-a/app/widget_registry.rb"),
		symInFile("svc-a", "app.PaymentLedger", "svc-a/app/payment_ledger.rb"),
		symInFile("svc-a", "app.RetryPolicy", "svc-a/app/retry_policy.rb"),
		module("svc-b", "app"),
		symInFile("svc-b", "app.WidgetRegistry", "svc-b/app/widget_registry.rb"),
		symInFile("svc-b", "app.PaymentLedger", "svc-b/app/payment_ledger.rb"),
		symInFile("svc-b", "app.RetryPolicy", "svc-b/app/retry_policy.rb"),
	}
	src := fakeSource(map[string]string{
		// Copied.
		"svc-a/app/widget_registry.rb": bodyAlpha,
		"svc-b/app/widget_registry.rb": bodyAlpha,
		// Same name, unrelated code.
		"svc-a/app/payment_ledger.rb": bodyAlpha,
		"svc-b/app/payment_ledger.rb": bodyBeta,
		"svc-a/app/retry_policy.rb":   bodyAlpha,
		"svc-b/app/retry_policy.rb":   bodyBeta,
	})

	if sc := sharedCodeFacts(ComputeLinks(in, src, allSignals(), vocab.Default())); len(sc) != 0 {
		t.Errorf("only 1 of 3 names is backed by shared code, below the threshold — no coupling expected: %+v", sc)
	}
	// Without a reader the same input still couples on names alone, which is what the
	// verification is an improvement over.
	if sc := sharedCodeFacts(ComputeLinks(in, nil, allSignals(), vocab.Default())); len(sc) != 1 {
		t.Errorf("name-only matching (nil reader) should still couple the pair, got %+v", sc)
	}
}

// TestComputeLinks_SharedSymbolsVerifiedReportsBothCounts covers the surviving case:
// enough names are backed by real shared code, and the fact records both the verified
// count and how many matched by name alone.
func TestComputeLinks_SharedSymbolsVerifiedReportsBothCounts(t *testing.T) {
	in := []facts.Fact{
		module("svc-a", "app"),
		symInFile("svc-a", "app.WidgetRegistry", "svc-a/app/widget_registry.rb"),
		symInFile("svc-a", "app.PaymentLedger", "svc-a/app/payment_ledger.rb"),
		symInFile("svc-a", "app.RetryPolicy", "svc-a/app/retry_policy.rb"),
		symInFile("svc-a", "app.InvoiceReconciler", "svc-a/app/invoice_reconciler.rb"),
		module("svc-b", "app"),
		symInFile("svc-b", "app.WidgetRegistry", "svc-b/app/widget_registry.rb"),
		symInFile("svc-b", "app.PaymentLedger", "svc-b/app/payment_ledger.rb"),
		symInFile("svc-b", "app.RetryPolicy", "svc-b/app/retry_policy.rb"),
		symInFile("svc-b", "app.InvoiceReconciler", "svc-b/app/invoice_reconciler.rb"),
	}
	src := fakeSource(map[string]string{
		"svc-a/app/widget_registry.rb": bodyAlpha,
		"svc-b/app/widget_registry.rb": bodyAlpha,
		"svc-a/app/payment_ledger.rb":  bodyAlpha,
		// Reformatted copy: must still verify.
		"svc-b/app/payment_ledger.rb": bodyAlphaReformatted,
		"svc-a/app/retry_policy.rb":   bodyAlpha,
		"svc-b/app/retry_policy.rb":   bodyAlpha,
		// Name-only match: must be dropped from the count.
		"svc-a/app/invoice_reconciler.rb": bodyAlpha,
		"svc-b/app/invoice_reconciler.rb": bodyBeta,
	})

	sc := findSharedCode(ComputeLinks(in, src, allSignals(), vocab.Default()), "svc-a", "svc-b")
	if sc == nil {
		t.Fatalf("3 verified shared types should still couple the pair")
	}
	if c, _ := sc.Props["symbol_count"].(int); c != 3 {
		t.Errorf("symbol_count = %v, want 3 (verified only)", sc.Props["symbol_count"])
	}
	if n, _ := sc.Props["name_match_count"].(int); n != 4 {
		t.Errorf("name_match_count = %v, want 4 (pre-verification total)", sc.Props["name_match_count"])
	}
	if syms, _ := sc.Props["symbol_samples"].([]string); len(syms) != 3 {
		t.Errorf("samples should list only verified identities, got %v", syms)
	} else {
		for _, s := range syms {
			if s == "InvoiceReconciler" {
				t.Errorf("name-only match leaked into samples: %v", syms)
			}
		}
	}
}

// TestComputeLinks_SharedSymbolsNameMatchCountOmittedWhenEqual keeps the fact lean: with
// no reader (or when nothing was filtered) the two counts are identical and the extra
// prop would be noise.
func TestComputeLinks_SharedSymbolsNameMatchCountOmittedWhenEqual(t *testing.T) {
	in := []facts.Fact{
		module("svc-a", "app"),
		symInFile("svc-a", "app.WidgetRegistry", "svc-a/app/a.rb"),
		symInFile("svc-a", "app.PaymentLedger", "svc-a/app/a.rb"),
		symInFile("svc-a", "app.RetryPolicy", "svc-a/app/a.rb"),
		module("svc-b", "app"),
		symInFile("svc-b", "app.WidgetRegistry", "svc-b/app/a.rb"),
		symInFile("svc-b", "app.PaymentLedger", "svc-b/app/a.rb"),
		symInFile("svc-b", "app.RetryPolicy", "svc-b/app/a.rb"),
	}
	sc := findSharedCode(ComputeLinks(in, nil, allSignals(), vocab.Default()), "svc-a", "svc-b")
	if sc == nil {
		t.Fatalf("name-only matching should couple the pair")
	}
	if _, present := sc.Props["name_match_count"]; present {
		t.Errorf("name_match_count must be omitted when verification did not narrow anything: %+v", sc.Props)
	}
}

// TestComputeLinks_SharedSymbolsUnreadableSourceDropsIdentity pins the failure mode: a
// file that cannot be read is not evidence of shared code, so the identity does not
// count. Silently trusting the name here would reintroduce exactly what verification
// exists to prevent.
func TestComputeLinks_SharedSymbolsUnreadableSourceDropsIdentity(t *testing.T) {
	in := []facts.Fact{
		module("svc-a", "app"),
		symInFile("svc-a", "app.WidgetRegistry", "svc-a/app/widget_registry.rb"),
		symInFile("svc-a", "app.PaymentLedger", "svc-a/app/payment_ledger.rb"),
		symInFile("svc-a", "app.RetryPolicy", "svc-a/app/retry_policy.rb"),
		module("svc-b", "app"),
		symInFile("svc-b", "app.WidgetRegistry", "svc-b/app/widget_registry.rb"),
		symInFile("svc-b", "app.PaymentLedger", "svc-b/app/payment_ledger.rb"),
		symInFile("svc-b", "app.RetryPolicy", "svc-b/app/retry_policy.rb"),
	}
	// Only one side readable for every identity.
	src := fakeSource(map[string]string{
		"svc-a/app/widget_registry.rb": bodyAlpha,
		"svc-a/app/payment_ledger.rb":  bodyAlpha,
		"svc-a/app/retry_policy.rb":    bodyAlpha,
	})
	if sc := sharedCodeFacts(ComputeLinks(in, src, allSignals(), vocab.Default())); len(sc) != 0 {
		t.Errorf("unreadable source must not count as verified: %+v", sc)
	}
}

// TestComputeLinks_SharedSymbolsVerificationAppliesToAnnotatedEdges confirms the same
// filter governs the shared-symbol annotation on a real (directional) edge, so a
// dependency edge cannot carry an inflated symbol count either. The import edge itself
// must survive regardless — verification governs the annotation, not the dependency.
func TestComputeLinks_SharedSymbolsVerificationAppliesToAnnotatedEdges(t *testing.T) {
	in := []facts.Fact{
		module("app", "client"),
		importDep("app", "@lib/ui"),
		symInFile("app", "client.WidgetRegistry", "app/client/widget_registry.ts"),
		symInFile("app", "client.PaymentLedger", "app/client/payment_ledger.ts"),
		symInFile("app", "client.RetryPolicy", "app/client/retry_policy.ts"),
		module("lib", "libs/ui"),
		symInFile("lib", "libs/ui.WidgetRegistry", "lib/libs/ui/widget_registry.ts"),
		symInFile("lib", "libs/ui.PaymentLedger", "lib/libs/ui/payment_ledger.ts"),
		symInFile("lib", "libs/ui.RetryPolicy", "lib/libs/ui/retry_policy.ts"),
	}
	src := fakeSource(map[string]string{
		"app/client/widget_registry.ts":  bodyAlpha,
		"lib/libs/ui/widget_registry.ts": bodyBeta, // name only
		"app/client/payment_ledger.ts":   bodyAlpha,
		"lib/libs/ui/payment_ledger.ts":  bodyBeta, // name only
		"app/client/retry_policy.ts":     bodyAlpha,
		"lib/libs/ui/retry_policy.ts":    bodyBeta, // name only
	})

	out := ComputeLinks(in, src, allSignals(), vocab.Default())
	e := findEdge(out, "app", "lib")
	if e == nil {
		t.Fatalf("the import edge must survive; out=%+v", out)
	}
	if via, _ := e.Props["via"].([]string); !reflect.DeepEqual(via, []string{"import"}) {
		t.Errorf("via = %v, want [import] — no name-only shared_symbols annotation", e.Props["via"])
	}
	if _, present := e.Props["symbol_count"]; present {
		t.Errorf("edge must carry no shared-symbol count when none verified: %+v", e.Props)
	}
}

// allSignals is the production signal set, so every ComputeLinks test here exercises the
// same wiring bootstrap registers rather than a test-only subset.
func allSignals() []plugin.CrossRepoSignal {
	return []plugin.CrossRepoSignal{
		httpsignal.New(vocab.Default()), importsignal.New(), kafkasignal.New(vocab.Default()), sharedcodesignal.New(vocab.Default()),
	}
}

// TestComputeLinks_BaseRelativeClientResolvesAsProbable pins the commonest real-world
// client shape there is, which no other test here covered end to end: a frontend whose
// HTTP client is configured with a base URL ending in a mount prefix, so every call site
// in the source is written base-relative and format-suffixed ("/v2/categories.json")
// while the backend serves the full path ("/api/v2/categories").
//
// Two properties matter and they pull in opposite directions:
//
//   - The call MUST resolve. The prefix lives in a base-URL constant the extractor
//     cannot see, so demanding a full-path match here would drop every call site in the
//     repo and report the whole frontend as a coverage blind spot.
//   - The match MUST be "probable", not "verified". The client never named the
//     provider's complete path, so a second backend mounted at a different prefix could
//     serve the same suffix. Reporting it as verified would overstate what was checked.
//
// The estate deliberately contains a second backend, so the suffix match is happening
// with an alternative present rather than in a one-candidate vacuum.
func TestComputeLinks_BaseRelativeClientResolvesAsProbable(t *testing.T) {
	in := []facts.Fact{
		serverRoute("api", "/api/v2/categories", "GET"),
		serverRoute("api", "/api/public/v1/complaints", "POST"),
		// A second backend, serving something else entirely.
		serverRoute("billing", "/api/v1/invoices", "GET"),
		// Base-relative, format-suffixed call sites, as a base-URL-configured client emits.
		clientRoute("web", "/v2/categories.json", "GET", nil),
		clientRoute("web", "/public/v1/complaints.json", "POST", nil),
	}
	out := ComputeLinks(in, nil, allSignals(), vocab.Default())

	e := findEdge(out, "web", "api")
	if e == nil {
		t.Fatalf("base-relative client calls must still resolve; edges=%+v", crossRepoEdges(out))
	}
	if got := e.Props["confidence"]; got != "probable" {
		t.Errorf("confidence = %v, want probable — the client never named the provider's complete path", got)
	}
	if got := e.Props["endpoint_count"]; got != 2 {
		t.Errorf("endpoint_count = %v, want 2", got)
	}
	if findEdge(out, "web", "billing") != nil {
		t.Error("a suffix match must not spill onto the second backend")
	}

	// And the resolution must be visible as full coverage, not a blind spot: this is the
	// number a frontend team looks at to decide whether enola saw their API surface.
	for _, f := range out {
		if f.Kind != facts.KindService || f.Repo != "web" {
			continue
		}
		cov, ok := f.Props["edge_coverage"].([]map[string]any)
		if !ok || len(cov) != 1 {
			t.Fatalf("web edge_coverage = %+v, want one entry", f.Props["edge_coverage"])
		}
		if cov[0]["detected"] != 2 || cov[0]["resolved"] != 2 || cov[0]["unresolved"] != 0 {
			t.Errorf("coverage = %+v, want 2 detected / 2 resolved / 0 unresolved", cov[0])
		}
	}
}
