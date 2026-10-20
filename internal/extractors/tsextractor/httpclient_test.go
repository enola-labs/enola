package tsextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// byNameMethod indexes emitted client routes by path -> method.
func byNameMethod(ff []facts.Fact) map[string]string {
	out := map[string]string{}
	for _, f := range ff {
		out[f.Name] = f.Props["method"].(string)
	}
	return out
}

// TestExtractHTTPClientFacts_VerbNamedCalls covers the openapi-fetch generated
// client where the method is the (uppercase) call name and the path is the first
// positional literal.
func TestExtractHTTPClientFacts_VerbNamedCalls(t *testing.T) {
	src := "async function load(id: number) {\n" +
		"  await API.getApi().GET('/api/v3/items/{id}', { params });\n" +
		"  API.getApi().POST('/api/v3/widgets/{id}/follow', { body });\n" +
		"  API.getApi().DELETE('/api/v3/sessions');\n" +
		"  const v = map.get('some-key');\n" + // lowercase .get must NOT match
		"}\n"
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "client/feed/network.ts"))

	// Literal {id} braces are left as written (the linker's normalizePath collapses
	// them to {} at match time), like the Retrofit/Swift extractors.
	if got["/api/v3/items/{id}"] != "GET" {
		t.Errorf("GET call: got %+v", got)
	}
	if got["/api/v3/widgets/{id}/follow"] != "POST" {
		t.Errorf("POST call: got %+v", got)
	}
	if got["/api/v3/sessions"] != "DELETE" {
		t.Errorf("DELETE call: got %+v", got)
	}
	if _, found := got["some-key"]; found {
		t.Errorf("lowercase map.get('some-key') must not be detected: %+v", got)
	}
}

// TestExtractHTTPClientFacts_NestedGenericTypeArg pins v141's first half: a NESTED
// type argument must not defeat detection. "<[^>]*>" stopped at the inner ">", so
// one level of generics matched and two silently did not — on the very shape
// openapi-fetch clients use. Both the verb-named and the fetch pattern shared the
// bug, so both are asserted here; the one-level cases are the control that shows
// the widening did not simply relax everything.
func TestExtractHTTPClientFacts_NestedGenericTypeArg(t *testing.T) {
	src := "async function load() {\n" +
		"  await API.getApi().GET<Resp>('/api/v3/one-level');\n" +
		"  await API.getApi().GET<ApiResponse<Item[]>>('/api/v3/nested');\n" +
		"  await API.getApi().POST<ApiResponse<Map<string, Item>>>('/api/v3/deeply-nested');\n" +
		"  await fetch<ApiResponse<Item>>('/api/v3/fetch-nested');\n" +
		"}\n"
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "client/feed/network.ts"))

	for path, want := range map[string]string{
		"/api/v3/one-level":     "GET",
		"/api/v3/nested":        "GET",
		"/api/v3/deeply-nested": "POST",
		"/api/v3/fetch-nested":  "GET",
	} {
		if got[path] != want {
			t.Errorf("%s: want %s, got %+v", path, want, got)
		}
	}
}

// TestExtractHTTPClientFacts_LowercaseVerbCalls pins v141's second half: the
// hand-written axios idiom is detected, and the collision hazard that justified
// excluding it stays excluded. The discriminator is the "/"-rooted argument, so
// every must-not case below is a real-world lowercase method whose first argument
// is an ordinary string key.
func TestExtractHTTPClientFacts_LowercaseVerbCalls(t *testing.T) {
	src := "async function load(id: string, data: Body) {\n" +
		"  await axios.get('/api/v2/slots/available', { params });\n" +
		"  await http.post<ApiResponse<string>>('/slots/reserve', data);\n" +
		"  await apiClient.put(`/calendars/${id}/credentials`, data);\n" +
		"  await client.delete('/bookings/{uid}');\n" +
		"  await api.patch(\"/me\", data);\n" +
		// Collision cases the uppercase-only rule was protecting against: a
		// lowercase verb whose argument is a plain key, not a "/"-rooted path.
		"  const a = map.get('some-key');\n" +
		"  cache.delete('session');\n" +
		"  searchParams.get('redirect');\n" +
		"  headers.get('content-type');\n" +
		"  formData.get('file');\n" +
		"}\n"
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "client/feed/network.ts"))

	for path, want := range map[string]string{
		"/api/v2/slots/available":   "GET",
		"/slots/reserve":            "POST",
		"/calendars/{}/credentials": "PUT",
		"/bookings/{uid}":           "DELETE",
		"/me":                       "PATCH",
	} {
		if got[path] != want {
			t.Errorf("%s: want %s, got %+v", path, want, got)
		}
	}
	for _, key := range []string{"some-key", "session", "redirect", "content-type", "file"} {
		if _, found := got[key]; found {
			t.Errorf("lowercase collection call %q must not be detected: %+v", key, got)
		}
	}
}

// TestExtractHTTPClientFacts_LowercaseInterpolatedBase pins both sides of the
// boundary v153 moved. v141 deliberately excluded interpolation-headed templates
// from lowercase verb calls (recorded then as GAP-TS-06); litfold's template-tail
// rule now admits exactly the shape whose tail is "/"-rooted — resolving that
// gap — while a tail-less interpolation stays out, so a collection lookup can
// still never match.
func TestExtractHTTPClientFacts_LowercaseInterpolatedBase(t *testing.T) {
	src := "async function load() {\n" +
		"  await axios.get(`${baseUrl}/v1/charges`);\n" +
		"}\n"
	ff := extractHTTPClientFacts([]byte(src), "client/pay.ts")
	if len(ff) != 1 || ff[0].Name != "/v1/charges" || ff[0].Props["derived"] != "template-tail" {
		t.Errorf("interpolated-base with /-rooted tail = %+v, want one derived /v1/charges", ff)
	}
	bare := "const v = registry.get(`${prefix}${key}`);\n"
	if got := extractHTTPClientFacts([]byte(bare), "client/reg.ts"); len(got) != 0 {
		t.Errorf("tail-less interpolation must stay excluded: %+v", got)
	}
}

// TestExtractHTTPClientFacts_TestFileCallsAreNotClientRoutes pins the guard that
// v141 needed the moment it landed. Admitting lowercase verbs made every supertest
// call in an e2e suite — request(app).get('/v2/me') — look exactly like production
// axios traffic, and on one real NestJS API that turned its own test suite into
// **500+** client routes, moved the service from `isolated` to `connected`, and
// fabricated a cross-repo `v2 -> web` dependency edge out of test traffic. The
// paths matched a real server for a real reason: they are the routes under test.
//
// The gate lives at the ts.go call site (facts.IsTestPath), so this asserts the
// predicate covers the two e2e conventions that the .spec/.test suffixes miss —
// the hyphenated forms have no leading dot and so were never matched.
func TestExtractHTTPClientFacts_TestFileCallsAreNotClientRoutes(t *testing.T) {
	for _, f := range []string{
		"src/app.e2e-spec.ts",                     // Nest CLI convention
		"src/modules/slots/e2e/slots.e2e-spec.ts", // colocated e2e suite
		"playwright/oauth-provider.e2e.ts",        // Playwright/Cypress convention
		"src/api/client.spec.ts",                  // already covered; kept as control
	} {
		if !facts.IsTestPath(f) {
			t.Errorf("IsTestPath(%q) = false; an HTTP call here would become a phantom client route", f)
		}
	}
	// A production file must not be swept up by the widened suffix list: the rule
	// this list enforces is "a convention a tool enforces", not "looks testy".
	for _, f := range []string{
		"src/lib/e2e-helpers.ts",
		"src/modules/latest/client.ts",
		"src/contest/api.ts",
	} {
		if facts.IsTestPath(f) {
			t.Errorf("IsTestPath(%q) = true; production code must stay in the graph", f)
		}
	}
}

// TestExtractHTTPClientFacts_OptionsObject covers the options-object client where
// the URL is a `url:` property and the verb is a `type:` property (with the
// action verb "query" mapping to GET), including the declarative request object.
func TestExtractHTTPClientFacts_OptionsObject(t *testing.T) {
	src := "const a = createRequest({ url: '/v2/items.json', type: 'query', token });\n" +
		"const b = createRequest({ url: `/v2/items/${id}.json`, type: 'delete', token });\n" +
		"const post = { type: 'post', payload: {}, url: '/v2/messages.json' };\n" +
		"const c = request({ query: { zip }, url: '/v2/lookup/address.json' });\n" + // no verb -> GET
		"const link = { url: '/settings/profile', label: 'Profile' };\n" // plain link -> not a request
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "client/items/network.jsx"))

	if got["/v2/items.json"] != "GET" {
		t.Errorf("query verb should map to GET: %+v", got)
	}
	if got["/v2/items/{}.json"] != "DELETE" {
		t.Errorf("delete verb + interpolation: %+v", got)
	}
	if got["/v2/messages.json"] != "POST" {
		t.Errorf("declarative request object post: %+v", got)
	}
	if got["/v2/lookup/address.json"] != "GET" {
		t.Errorf("verb-less request should default GET: %+v", got)
	}
	if _, found := got["/settings/profile"]; found {
		t.Errorf("a plain link object (no request-descriptor key) must not be detected: %+v", got)
	}
}

func TestExtractHTTPClientFacts(t *testing.T) {
	src := "class FeedbackApi {\n" +
		"  async createFeedback(data: any) {\n" +
		"    return this.makeRequest<any>('/api/settings/feedback', { method: 'POST' });\n" +
		"  }\n" +
		"  async respond(id: number) {\n" +
		"    return this.makeRequest<any>(`/api/settings/feedback/${id}/respond`, {\n" +
		"      method: 'PUT',\n" +
		"    });\n" +
		"  }\n" +
		"  async stats() {\n" +
		"    return this.makeRequest<any>('/api/settings/feedback/statistics');\n" + // default GET
		"  }\n" +
		"  async external() {\n" +
		"    return fetch('https://third.party/v1/thing', { method: 'GET' });\n" + // external
		"  }\n" +
		"}\n"

	ff := extractHTTPClientFacts([]byte(src), "src/lib/api/feedback.ts")

	byName := map[string]string{} // name -> method
	for _, f := range ff {
		if f.Props["role"] != "client" || f.Props["framework"] != "fetch" {
			t.Errorf("%s wrong props: %+v", f.Name, f.Props)
		}
		if f.Props["api"] != "feedback" {
			t.Errorf("%s api hint = %v, want feedback", f.Name, f.Props["api"])
		}
		byName[f.Name] = f.Props["method"].(string)
		external := f.Name == "/v1/thing"
		if (f.Props["external"] == true) != external {
			t.Errorf("%s external = %v, want %v", f.Name, f.Props["external"], external)
		}
		if external && f.Props["host"] != "third.party" {
			t.Errorf("%s host = %v, want third.party", f.Name, f.Props["host"])
		}
	}

	if len(byName) != 4 {
		t.Fatalf("expected 3 backend client routes and 1 external, got %d: %+v", len(byName), byName)
	}
	if byName["/v1/thing"] != "GET" {
		t.Errorf("external: want GET /v1/thing, got %+v", byName)
	}
	if byName["/api/settings/feedback"] != "POST" {
		t.Errorf("createFeedback: want POST, got %+v", byName)
	}
	if byName["/api/settings/feedback/{}/respond"] != "PUT" {
		t.Errorf("respond: want PUT with {} param, got %+v", byName)
	}
	if byName["/api/settings/feedback/statistics"] != "GET" {
		t.Errorf("stats: want default GET, got %+v", byName)
	}
	if _, found := byName["https://third.party/v1/thing"]; found {
		t.Errorf("an external URL is named by its path, never by the whole URL: %+v", byName)
	}
}

func TestTSExternalHost_LiteralHostKept(t *testing.T) {
	src := "export async function sync(id: string) {\n" +
		"  await fetch('https://api.example.com/v1/charges', { method: 'POST' });\n" +
		"  await fetch(`https://api.example.com/v1/charges/${id}?expand=true`);\n" +
		"  await fetch('https://status.example.com');\n" +
		"  await fetch('http://localhost:8080/health');\n" +
		"  await request({ url: 'https://hooks.example.com/v2/events', method: 'PUT' });\n" +
		"}\n"
	ff, hosts := extractHTTPClientFactsCounted([]byte(src), "src/lib/sync.ts")
	want := map[string]struct {
		method, host string
		line         int
	}{
		"/v1/charges":    {"POST", "api.example.com", 2},
		"/v1/charges/{}": {"GET", "api.example.com", 3},
		"/":              {"GET", "status.example.com", 4},
		"/health":        {"GET", "localhost:8080", 5},
		"/v2/events":     {"PUT", "hooks.example.com", 6},
	}
	if len(ff) != len(want) {
		t.Fatalf("got %d client routes, want %d: %+v", len(ff), len(want), ff)
	}
	for _, f := range ff {
		w, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected route %q", f.Name)
			continue
		}
		if f.Props["method"] != w.method || f.Line != w.line {
			t.Errorf("%s = %v at line %d, want %s at line %d", f.Name, f.Props["method"], f.Line, w.method, w.line)
		}
		if f.Props["external"] != true || f.Props["host"] != w.host {
			t.Errorf("%s external/host = %v/%v, want true/%s", f.Name, f.Props["external"], f.Props["host"], w.host)
		}
		if f.Props["source"] != facts.RouteSourceTSHTTPClient || f.Props["role"] != facts.RoleClient {
			t.Errorf("%s must stay a typescript client route: %+v", f.Name, f.Props)
		}
	}
	if hosts.kept != len(want) || hosts.interpolated != 0 {
		t.Fatalf("hosts counted = %+v, want %d kept and none refused", hosts, len(want))
	}
}

func TestTSExternalHost_InterpolatedHostRefusedAndCounted(t *testing.T) {
	src := "export async function sync(host: string, path: string) {\n" +
		"  await fetch(`https://${host}/v1/charges`);\n" +
		"  await fetch(`https://api.example.com${path}`);\n" +
		"  await fetch('/v1/relative');\n" +
		"}\n"
	ff, hosts := extractHTTPClientFactsCounted([]byte(src), "src/lib/sync.ts")
	if len(ff) != 1 || ff[0].Name != "/v1/relative" {
		t.Fatalf("only the relative call may yield a route, got %+v", ff)
	}
	if hosts.interpolated != 2 || hosts.kept != 0 {
		t.Fatalf("hosts counted = %+v, want 2 refused and none kept", hosts)
	}
}

func TestTSExternalHost_RelativeUnchanged(t *testing.T) {
	src := "export async function load(id: number) {\n" +
		"  await fetch(`${API_BASE_URL}/api/user/current`);\n" +
		"  await fetch('/api/items?page=2');\n" +
		"  await axios.get('/api/things');\n" +
		"}\n"
	ff := extractHTTPClientFacts([]byte(src), "src/lib/load.ts")
	got := byNameMethod(ff)
	if len(got) != 3 || got["/api/user/current"] != "GET" || got["/api/items"] != "GET" || got["/api/things"] != "GET" {
		t.Fatalf("relative routes changed: %+v", got)
	}
	for _, f := range ff {
		if _, set := f.Props["external"]; set {
			t.Errorf("%s: a relative call must not be marked external: %+v", f.Name, f.Props)
		}
		if _, set := f.Props["host"]; set {
			t.Errorf("%s: a relative call names no host: %+v", f.Name, f.Props)
		}
	}
}

func TestTSExternalHost_CountedOnTheExtractionRecord(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", `{"name":"sync"}`)
	write("src/sync.ts", "export async function sync(host: string) {\n"+
		"  await fetch(`https://${host}/v1/charges`);\n"+
		"  await fetch('https://api.example.com/v1/charges');\n"+
		"}\n")
	ff, err := New().Extract(context.Background(), dir, []string{"package.json", "src/sync.ts"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range ff {
		if f.Kind != facts.KindExtraction || f.Name != "typescript:http-client" {
			continue
		}
		if got := f.Props["unresolved_macros"]; got != "host-interpolated=1" {
			t.Fatalf("unresolved_macros = %v, want host-interpolated=1", got)
		}
		entry := f.Props["edge_coverage"].([]map[string]any)[0]
		if entry["resolved"] != 1 || entry["unresolved"] != 1 {
			t.Fatalf("edge_coverage = %+v, want 1 host read and 1 refused", entry)
		}
		return
	}
	t.Fatalf("no typescript:http-client extraction record: %+v", ff)
}

// TestExtractHTTPClientFacts_PrefetchNotMatched covers the left word-boundary on
// the fetch()/makeRequest() matcher: a call whose name merely ends in "fetch"
// (router.prefetch / query.refetch — navigation and cache primitives) must NOT be
// captured, while a genuine `window.fetch(` / member `this.makeRequest(` still is.
func TestExtractHTTPClientFacts_PrefetchNotMatched(t *testing.T) {
	src := "function onFocus() {\n" +
		"  router.prefetch('/dashboard/premium');\n" + // navigation, not HTTP
		"  router.prefetch('/dashboard/maintenance');\n" +
		"  query.refetch('/api/should-not-appear');\n" + // react-query cache, not HTTP
		"  window.fetch('/api/real/thing');\n" + // genuine fetch -> kept
		"}\n"
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "src/app/login/page.tsx"))

	for _, junk := range []string{"/dashboard/premium", "/dashboard/maintenance", "/api/should-not-appear"} {
		if _, found := got[junk]; found {
			t.Errorf("prefetch/refetch target %q must not be detected: %+v", junk, got)
		}
	}
	if got["/api/real/thing"] != "GET" {
		t.Errorf("window.fetch should still be detected: %+v", got)
	}
}

// TestExtractHTTPClientFacts_SEOMetadataNotRequest covers Pass 3's tightened
// request-descriptor test: a Next.js `openGraph` block carries a `url:` and a
// non-verb `type: 'website'` but no request-payload key, so it is metadata, not an
// outbound call. A sibling request object with a real verb still resolves.
func TestExtractHTTPClientFacts_SEOMetadataNotRequest(t *testing.T) {
	src := "export const metadata = {\n" +
		"  openGraph: {\n" +
		"    type: 'website',\n" +
		"    url: `${baseUrl}/journal`,\n" +
		"    siteName: 'Example',\n" +
		"    locale: 'en_US',\n" +
		"  },\n" +
		"};\n" +
		"const call = request({ url: '/api/settings/app', type: 'post', payload: {} });\n"
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "src/app/journal/layout.tsx"))

	if _, found := got["/journal"]; found {
		t.Errorf("openGraph SEO url (type:'website') must not be detected as a call: %+v", got)
	}
	if got["/api/settings/app"] != "POST" {
		t.Errorf("a real request object with a verb should still resolve: %+v", got)
	}
}

// TestExtractHTTPClientFacts_NonPathLiteralSkipped covers cleanTSPath's leading-"/"
// requirement: a non-path string literal reaching the matcher (e.g. an analysis
// script's own source scanning for `fetch(`) must not become a phantom route.
func TestExtractHTTPClientFacts_NonPathLiteralSkipped(t *testing.T) {
	src := "const markers = [ 'fetch(', ',' ];\n" + // fetch(',' -> URL literal is ","
		"if (line.includes('fetch(')) { hasAPICall = true; }\n"
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "fitness-functions.js"))

	for name := range got {
		if len(name) == 0 || name[0] != '/' {
			t.Errorf("non-path literal %q must be skipped, got routes: %+v", name, got)
		}
	}
}

// TestExtractHTTPClientFacts_GluedQueryPlaceholderStripped covers cleanTSPath's
// query-string-placeholder strip: a `${queryParams}` fused to the final segment
// collapses to a trailing "{}" glued to text, which must be dropped so the path
// matches its server route — while an own-segment "/{}" path param is preserved.
func TestExtractHTTPClientFacts_GluedQueryPlaceholderStripped(t *testing.T) {
	src := "class A {\n" +
		"  a() { return this.makeRequest(`/api/settings/analytics/role-distribution${queryParams}`); }\n" +
		"  b() { return this.makeRequest(`${this.baseUrl}/api/settings/vat-rates/active${qs}`); }\n" +
		"  c() { return this.makeRequest(`/api/settings/files/${id}`); }\n" + // real path param -> keep {}
		"  d() { return this.makeRequest(`/api/settings/files/${id}/versions`); }\n" + // {} mid-path -> keep
		"}\n"
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "src/lib/api/analyticsApi.ts"))

	if _, found := got["/api/settings/analytics/role-distribution"]; !found {
		t.Errorf("glued query placeholder should be stripped to a clean path: %+v", got)
	}
	if _, found := got["/api/settings/vat-rates/active"]; !found {
		t.Errorf("glued query placeholder after a base-URL token should be stripped: %+v", got)
	}
	if _, found := got["/api/settings/files/{}"]; !found {
		t.Errorf("own-segment /{} path param must be preserved: %+v", got)
	}
	if _, found := got["/api/settings/files/{}/versions"]; !found {
		t.Errorf("mid-path {} param must be preserved: %+v", got)
	}
}

// TestExtractHTTPClientFacts_BaseLiteralResolved covers file-local base-URL
// resolution: a leading `${...}` base interpolated at the head of a call path is
// rebuilt from a "/"-rooted literal declared in the same file — as a class field,
// a file-scope const, or a constructor default param — so the full path (which
// matches the server route) is emitted instead of a bare single-segment suffix.
func TestExtractHTTPClientFacts_BaseLiteralResolved(t *testing.T) {
	src := "const ROOT = '/api/v2/things';\n" +
		"class PricingApi {\n" +
		"  private readonly basePath = '/api/settings/pricing';\n" +
		"  constructor(private baseUrl: string = '/api/settings/engagement') {}\n" +
		"  calc() { return this.makeRequest(`${this.basePath}/calculate`, { method: 'POST' }); }\n" +
		"  send() { return this.makeRequest(`${this.baseUrl}/send`, { method: 'POST' }); }\n" +
		"  list() { return fetch(`${ROOT}/list`); }\n" +
		"}\n"
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "src/lib/api/pricingEngine.ts"))

	if got["/api/settings/pricing/calculate"] != "POST" {
		t.Errorf("class-field base should resolve to full path: %+v", got)
	}
	if got["/api/v2/things/list"] != "GET" {
		t.Errorf("file-const base should resolve to full path: %+v", got)
	}
	if got["/api/settings/engagement/send"] != "POST" {
		t.Errorf("constructor-default base should resolve to full path: %+v", got)
	}
	if _, found := got["/calculate"]; found {
		t.Errorf("bare suffix must not survive once the base resolves: %+v", got)
	}
}

// TestExtractHTTPClientFacts_BaseLiteralUnresolvedFallsBackToSuffix covers the
// fallback: a base that is not a known "/"-rooted literal (injected via a
// non-defaulted param, or an absolute/env base) is still stripped, yielding the
// suffix exactly as before — the resolver never fabricates a base.
func TestExtractHTTPClientFacts_BaseLiteralUnresolvedFallsBackToSuffix(t *testing.T) {
	src := "class A {\n" +
		"  constructor(private baseURL: string) {}\n" + // injected, no literal
		"  ext = 'https://third.party/v1';\n" + // absolute -> not "/"-rooted, not captured
		"  a() { return fetch(`${this.baseURL}/quick-action-preferences`); }\n" +
		"  b() { return fetch(`${this.ext}/thing`); }\n" +
		"}\n"
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "src/lib/api/quickActionPreferences.ts"))

	if _, found := got["/quick-action-preferences"]; !found {
		t.Errorf("injected base should fall back to the stripped suffix: %+v", got)
	}
	if _, found := got["/thing"]; !found {
		t.Errorf("absolute base is not a resolvable literal; suffix kept: %+v", got)
	}
}

// TestExtractHTTPClientFacts_BaseLiteralAmbiguousNotResolved covers the ambiguity
// guard: an identifier bound to two different literals in the same file is dropped
// from the base map, so the resolver falls back to stripping rather than guessing.
func TestExtractHTTPClientFacts_BaseLiteralAmbiguousNotResolved(t *testing.T) {
	src := "const base = '/api/one';\n" +
		"const base = '/api/two';\n" + // conflicting binding -> ambiguous
		"function go() { return fetch(`${base}/x`); }\n"
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "src/lib/api/thing.ts"))

	if _, found := got["/x"]; !found {
		t.Errorf("ambiguous base must fall back to the suffix: %+v", got)
	}
	for _, bad := range []string{"/api/one/x", "/api/two/x"} {
		if _, found := got[bad]; found {
			t.Errorf("ambiguous base must not resolve to %q: %+v", bad, got)
		}
	}
}

// A call's verb must come from ITS OWN options object. Scanning a flat byte
// window forward let a later call's `method:` bleed backwards, so a plain
// fetch(url) sitting above a POST reported POST — a wrong verb on a real path,
// which then mis-resolves in the cross-repo linker.
func TestExtractHTTPClientFacts_MethodDoesNotBleedFromNextCall(t *testing.T) {
	src := "export async function fetchResults() {\n" +
		"  const res = await fetch('/api/v1/search/results');\n" +
		"  return res.json();\n" +
		"}\n" +
		"export async function search(q: string) {\n" +
		"  const res = await fetch('/api/v1/search', {\n" +
		"    method: 'POST',\n" +
		"    body: JSON.stringify({ query: q }),\n" +
		"  });\n" +
		"  return res.json();\n" +
		"}\n"
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "src/api.ts"))

	if got["/api/v1/search/results"] != "GET" {
		t.Errorf("optionless fetch = %v, want GET (bled from the POST below): %+v",
			got["/api/v1/search/results"], got)
	}
	if got["/api/v1/search"] != "POST" {
		t.Errorf("fetch with method: 'POST' = %v, want POST: %+v", got["/api/v1/search"], got)
	}
}

// A multi-line options object with nested braces (a JSON.stringify payload, a
// spread of defaults) is still read to its matching close brace.
func TestExtractHTTPClientFacts_MethodFromNestedOptionsObject(t *testing.T) {
	src := "const r = fetch('/api/v1/items', {\n" +
		"  ...defaults,\n" +
		"  headers: { 'Content-Type': 'application/json' },\n" +
		"  body: JSON.stringify({ nested: { deep: 1 } }),\n" +
		"  method: 'PUT',\n" +
		"});\n"
	if got := byNameMethod(extractHTTPClientFacts([]byte(src), "src/api.ts")); got["/api/v1/items"] != "PUT" {
		t.Errorf("method after nested objects = %v, want PUT: %+v", got["/api/v1/items"], got)
	}
}

// Options passed as a variable carry no readable verb, so the call falls back to
// GET rather than picking up an unrelated `method:` elsewhere in the file.
func TestExtractHTTPClientFacts_VariableOptionsDefaultsToGet(t *testing.T) {
	src := "const opts = { method: 'DELETE' };\n" +
		"const a = fetch('/api/v1/things', opts);\n" +
		"const b = fetch('/api/v1/others', { method: 'POST' });\n"
	got := byNameMethod(extractHTTPClientFacts([]byte(src), "src/api.ts"))
	if got["/api/v1/things"] != "GET" {
		t.Errorf("variable options = %v, want GET: %+v", got["/api/v1/things"], got)
	}
	if got["/api/v1/others"] != "POST" {
		t.Errorf("literal options = %v, want POST: %+v", got["/api/v1/others"], got)
	}
}

func TestHTTPClient_SingleAssignmentFoldedFetch(t *testing.T) {
	src := []byte("const url = `${config.ACME_HOST}/mcp`;\nconst res = await fetch(url, { method: 'POST' });\n")
	ff := extractHTTPClientFacts(src, "src/index.ts")
	if len(ff) != 1 || ff[0].Name != "/mcp" || ff[0].Props["method"] != "POST" {
		t.Fatalf("folded fetch = %+v, want one POST /mcp", ff)
	}
	if ff[0].Props["derived"] != "single-assignment" {
		t.Fatalf("folded route must carry its derivation form, got %v", ff[0].Props["derived"])
	}
	dup := []byte("let url = '/a';\nurl = '/b';\nfetch(url);\n")
	if got := extractHTTPClientFacts(dup, "src/dup.ts"); len(got) != 0 {
		t.Fatalf("a reassigned name folded: %+v", got)
	}
}

func TestHTTPClient_TemplateTailLowerVerb(t *testing.T) {
	src := []byte("const r = await apiClient.get(`${this.baseUrl}/widgets/${id}`);\n")
	ff := extractHTTPClientFacts(src, "src/widgets.ts")
	if len(ff) != 1 || ff[0].Name != "/widgets/{}" || ff[0].Props["method"] != "GET" {
		t.Fatalf("template-tail verb call = %+v, want one GET /widgets/{}", ff)
	}
	if ff[0].Props["derived"] != "template-tail" {
		t.Fatalf("template-tail route must carry its derivation form, got %v", ff[0].Props["derived"])
	}
	collection := []byte("const v = cache.get(`item-${id}`);\nconst w = map.get(key);\n")
	if got := extractHTTPClientFacts(collection, "src/cache.ts"); len(got) != 0 {
		t.Fatalf("collection lookups matched: %+v", got)
	}
}

func TestHTTPClient_UrlPropertyIdentifierFolded(t *testing.T) {
	src := []byte("const url = `${config.ACME_HOST}/mcp`;\nconst server = new MCPClient({\n  name: \"x\",\n  url: url,\n  requestInit: { headers: { authorization: token } },\n});\n")
	ff := extractHTTPClientFacts(src, "src/index.ts")
	if len(ff) != 1 || ff[0].Name != "/mcp" || ff[0].Props["derived"] != "single-assignment" {
		t.Fatalf("identifier url: property = %+v, want one derived /mcp", ff)
	}
	if ff[0].Props["method"] != facts.MethodAny {
		t.Fatalf("verb-less options object must carry MethodAny, got %v", ff[0].Props["method"])
	}
	plain := []byte("const target = compute();\nconst opts = { url: target, headers: {} };\n")
	if got := extractHTTPClientFacts(plain, "src/other.ts"); len(got) != 0 {
		t.Fatalf("unresolvable identifier url: derived something: %+v", got)
	}
}

func TestHTTPClient_StrippedBaseCarriesTargetHint(t *testing.T) {
	src := []byte("const url = `${config.ACME_HOST}/mcp`;\nconst server = new MCPClient({\n  url: url,\n  requestInit: { headers: { authorization: token } },\n});\n")
	ff := extractHTTPClientFacts(src, "src/index.ts")
	if len(ff) != 1 || ff[0].Props["target_hint"] != "acme" {
		t.Fatalf("stripped base must hint its host: %+v", ff)
	}
	inline := []byte("const r = await fetch(`${API_BASE_URL}/api/user/current`);\n")
	got := extractHTTPClientFacts(inline, "src/user.ts")
	if len(got) != 1 || got[0].Props["target_hint"] != "api" {
		t.Fatalf("inline interpolated base hints too: %+v", got)
	}
}

// A base that names no provider must hint nothing. Every case here was taken
// from the corpus, and each one resolved to no loaded repo — which is how a
// blind spot gets filed as an expected third-party call.
func TestHTTPClient_NonProviderBasesHintNothing(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"bare url", "const r = await fetch(`${url}/widgets/list`);\n"},
		{"bare base", "const r = await fetch(`${base}/widgets/list`);\n"},
		{"call expression", "const r = await fetch(`${getRootUrl()}/widgets/list`);\n"},
		{"quoted lookup", "const r = await fetch(`${Cypress.env('baseUrl')}/widgets/list`);\n"},
		{"no url suffix", "const r = await fetch(`${DEFAULT_REMOTE}/widgets/list`);\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := extractHTTPClientFacts([]byte(tc.src), "src/client.ts")
			if len(got) != 1 {
				t.Fatalf("want one client route, got %+v", got)
			}
			if h, ok := got[0].Props["target_hint"]; ok {
				t.Fatalf("a base naming no provider must not hint: got %q", h)
			}
		})
	}
}

func TestTSExternalHost_CommentIsNotACall(t *testing.T) {
	src := "export async function load() {\n" +
		"  // await fetch('https://api.example.com/v1/old');\n" +
		"  /* fetch('https://api.example.com/v1/older') */\n" +
		"   * fetch('https://api.example.com/v1/oldest')\n" +
		"  await fetch('https://api.example.com/v1/current');\n" +
		"}\n"
	ff, hosts := extractHTTPClientFactsCounted([]byte(src), "src/lib/load.ts")
	if len(ff) != 1 || ff[0].Name != "/v1/current" || ff[0].Line != 5 {
		t.Fatalf("only the call on line 5 is a call, got %+v", ff)
	}
	if hosts.kept != 1 || hosts.interpolated != 0 {
		t.Fatalf("hosts counted = %+v, want 1 kept", hosts)
	}
}

func TestTSExternalHost_DataObjectIsNotACall(t *testing.T) {
	src := "export function payload() {\n" +
		"  return {\n" +
		"    comments: [\n" +
		"      {\n" +
		"        nodeId: 'COMMENT_1',\n" +
		"        url: 'https://example.com/pr/1#discussion_r1',\n" +
		"        body: 'note',\n" +
		"      },\n" +
		"    ],\n" +
		"  };\n" +
		"}\n" +
		"export async function hook() {\n" +
		"  await request({ url: 'https://hooks.example.com/v2/events', method: 'PUT' });\n" +
		"}\n"
	ff, _ := extractHTTPClientFactsCounted([]byte(src), "src/lib/payload.ts")
	if len(ff) != 1 || ff[0].Name != "/v2/events" || ff[0].Props["host"] != "hooks.example.com" {
		t.Fatalf("only the object that states a verb is a request to a host, got %+v", ff)
	}
}
