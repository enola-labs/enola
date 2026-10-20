package rubyextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extractionFact(ff []facts.Fact, name string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Kind == facts.KindExtraction && f.Name == name {
			return f, true
		}
	}
	return facts.Fact{}, false
}

// clientRoutes filters facts to the outbound HTTP-client routes.
func clientRoutes(ff []facts.Fact) []facts.Fact {
	var out []facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Props["role"] == "client" {
			out = append(out, f)
		}
	}
	return out
}

func TestRubyHTTPClient_WrapperClient(t *testing.T) {
	src := `class PurchaseService
  def build
    SvcCheckoutClient.post('purchase/build', body: payload)
  end
end
`
	got := clientRoutes(extractRubyHTTPClientFacts([]byte(src), "app/services/purchase_service.rb"))
	if len(got) != 1 {
		t.Fatalf("got %d client routes, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Name != "purchase/build" {
		t.Errorf("path = %q, want purchase/build", f.Name)
	}
	if f.Props["method"] != "POST" {
		t.Errorf("method = %v, want POST", f.Props["method"])
	}
	if f.Props["source"] != "ruby-http-client" {
		t.Errorf("source = %v, want ruby-http-client", f.Props["source"])
	}
	if f.Props["framework"] != "http-client" {
		t.Errorf("framework = %v, want http-client", f.Props["framework"])
	}
	if f.Props["target_hint"] != "svccheckout" {
		t.Errorf("target_hint = %v, want svccheckout", f.Props["target_hint"])
	}
}

func TestRubyHTTPClient_Faraday(t *testing.T) {
	src := `conn.get('users/123')
Faraday.post("orders/new")
`
	got := clientRoutes(extractRubyHTTPClientFacts([]byte(src), "app/clients/api.rb"))
	if len(got) != 2 {
		t.Fatalf("got %d client routes, want 2: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Props["framework"] != "faraday" {
			t.Errorf("%s framework = %v, want faraday", f.Name, f.Props["framework"])
		}
	}
}

func TestRubyHTTPClient_Interpolation(t *testing.T) {
	src := `client.get("users/#{id}/posts")`
	got := clientRoutes(extractRubyHTTPClientFacts([]byte(src), "app/clients/api.rb"))
	if len(got) != 1 {
		t.Fatalf("got %d client routes, want 1", len(got))
	}
	if got[0].Name != "users/{}/posts" {
		t.Errorf("path = %q, want users/{}/posts", got[0].Name)
	}
}

func TestRubyHTTPClient_EnvVarHint(t *testing.T) {
	src := `class XendoClient
  BASE = ENV.fetch('XENDO_URL')
  def fetch_thing
    conn.get('things/list')
  end
end
`
	got := clientRoutes(extractRubyHTTPClientFacts([]byte(src), "app/clients/xendo_client.rb"))
	if len(got) != 1 {
		t.Fatalf("got %d client routes, want 1", len(got))
	}
	if got[0].Props["target_hint"] != "xendo" {
		t.Errorf("target_hint = %v, want xendo (from XENDO_URL)", got[0].Props["target_hint"])
	}
}

func TestRubyHTTPClient_SkipsNonHTTP(t *testing.T) {
	src := `User.where(active: true)
params.get(:id)
cache.get('user:123')
record.posts.get(0)
client.get('https://api.stripe.com/v1/charges')
`
	got := clientRoutes(extractRubyHTTPClientFacts([]byte(src), "app/models/user.rb"))
	if len(got) != 1 {
		t.Fatalf("got %d client routes, want only the Stripe call: %+v", len(got), got)
	}
	stripe := got[0]
	if stripe.Name != "/v1/charges" || stripe.Line != 5 {
		t.Fatalf("Stripe call = %s at line %d, want /v1/charges at line 5", stripe.Name, stripe.Line)
	}
	if stripe.Props["external"] != true || stripe.Props["host"] != "api.stripe.com" {
		t.Fatalf("Stripe call must be external to api.stripe.com, got %+v", stripe.Props)
	}
}

func TestRubyExternalHost_LiteralHostKept(t *testing.T) {
	src := `class MobileAuth
  def github
    Faraday.get("https://api.github.com/user") do |req|
    end
  end

  def mlh(token)
    Faraday.get("https://my.mlh.io/api/v3/user.json?access_token=#{token}")
  end

  def debug(key)
    Faraday.post("https://api.github.com/applications/#{key}/token")
  end

  def ping
    client.get("https://status.example.com")
  end

  def local
    conn.get("http://localhost:3000/health")
  end
end
`
	got := clientRoutes(extractRubyHTTPClientFacts([]byte(src), "app/controllers/mobile_auth.rb"))
	want := map[string]struct {
		method, host string
		line         int
	}{
		"/user":                  {"GET", "api.github.com", 3},
		"/api/v3/user.json":      {"GET", "my.mlh.io", 8},
		"/applications/{}/token": {"POST", "api.github.com", 12},
		"/":                      {"GET", "status.example.com", 16},
		"/health":                {"GET", "localhost:3000", 20},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d client routes, want %d: %+v", len(got), len(want), got)
	}
	for _, f := range got {
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
		if f.Props["source"] != facts.RouteSourceRubyHTTPClient || f.Props["role"] != facts.RoleClient {
			t.Errorf("%s must stay a ruby-http-client client route: %+v", f.Name, f.Props)
		}
	}
}

func TestRubyExternalHost_InterpolatedHostRefusedAndCounted(t *testing.T) {
	src := `class Sync
  def fetch
    conn.get("https://#{host}/v1/charges")
    Faraday.post("https://api.example.com#{path}")
    client.get("/v1/relative")
  end
end
`
	ff, _, misses := extractRubyHTTPClientFactsCounted([]byte(src), "app/services/sync.rb")
	got := clientRoutes(ff)
	if len(got) != 1 || got[0].Name != "/v1/relative" {
		t.Fatalf("only the relative call may yield a route, got %+v", got)
	}
	if misses[clientHostInterpolated] != 2 {
		t.Fatalf("interpolated hosts counted = %d, want 2 (misses %v)", misses[clientHostInterpolated], misses)
	}
}

func TestRubyExternalHost_RelativeUnchanged(t *testing.T) {
	src := `conn.get('users/123')
client.post("/v1/charges?expand=#{x}")
`
	got := clientRoutes(extractRubyHTTPClientFacts([]byte(src), "app/clients/api.rb"))
	if len(got) != 2 {
		t.Fatalf("got %d client routes, want 2: %+v", len(got), got)
	}
	for _, f := range got {
		if _, set := f.Props["external"]; set {
			t.Errorf("%s: a relative call must not be marked external: %+v", f.Name, f.Props)
		}
		if _, set := f.Props["host"]; set {
			t.Errorf("%s: a relative call names no host: %+v", f.Name, f.Props)
		}
	}
	if got[0].Name != "users/123" || got[1].Name != "/v1/charges" {
		t.Fatalf("relative names changed: %s, %s", got[0].Name, got[1].Name)
	}
}

func TestRubyExternalHost_ThroughAParameter(t *testing.T) {
	src := `class Webhooks
  def deliver(url)
    connection.post(url, body)
  end

  def run
    deliver("https://hooks.example.com/v2/events")
    deliver("https://#{ENV["HOOK_HOST"]}/v2/events")
  end
end
`
	ff, derived, misses := extractRubyHTTPClientFactsCounted([]byte(src), "app/services/webhooks.rb")
	got := clientRoutes(ff)
	if len(got) != 1 || derived != 1 {
		t.Fatalf("one external route derived through the parameter, got %d routes, %d derived: %+v", len(got), derived, got)
	}
	f := got[0]
	if f.Name != "/v2/events" || f.Props["host"] != "hooks.example.com" || f.Props["external"] != true || f.Props["derived"] != "parameter" {
		t.Fatalf("parameter-derived external route = %+v", f)
	}
	if misses[clientHostInterpolated] != 1 || misses[parameterNonLiteral] != 0 {
		t.Fatalf("the interpolated host is a host refusal, not a non-literal argument: %v", misses)
	}
}

func TestRubyHTTPClient_WrapperLiteral(t *testing.T) {
	src := []byte("class Insights\n  def pageview(attributes)\n    connection.post(build_url(\"/pageview\"), attributes)\n  end\nend\n")
	ff := extractRubyHTTPClientFacts(src, "app/services/insights.rb")
	if len(ff) != 1 || ff[0].Name != "/pageview" || ff[0].Props["method"] != "POST" {
		t.Fatalf("wrapper-literal call = %+v, want one POST /pageview", ff)
	}
	if ff[0].Props["derived"] != "wrapper-literal" {
		t.Fatalf("wrapper route must carry its derivation form, got %v", ff[0].Props["derived"])
	}
	nonPath := []byte("conn.post(build_url(\"pageview\"))\nconn.get(t(\"labels.title\"))\n")
	if got := extractRubyHTTPClientFacts(nonPath, "app/services/other.rb"); len(got) != 0 {
		t.Fatalf("non-/-rooted wrapper literals derived: %+v", got)
	}
}

// TestRubyHTTPClient_TyphoeusWrapperMethod pins the request-wrapper derivation:
// a file whose single Typhoeus sink templates "#{base}#{path}" over a def
// parameter threads the rooted path literals from same-file call sites, with
// the sink's literal verb and the file's env-var hint.
func TestRubyHTTPClient_TyphoeusWrapperMethod(t *testing.T) {
	src := []byte(`class AnalyticsClient
  def events(company_id:, date:, page:)
    make_request(
      path: "/exports/#{company_id}/events",
      params: { date: date, page: page }
    )
  end

  def visits(company_id:)
    make_request(
      path: "/exports/#{company_id}/visits",
      params: {}
    )
  end

  private

  def initialize
    @base_url = ENV["ANALYTICS_BASE_URL"]
  end

  def make_request(path:, params:)
    request = Typhoeus::Request.new(
      "#{base_url}#{path}",
      method: :get,
      params: params
    )
    request.run
  end
end
`)
	ff := extractRubyHTTPClientFacts(src, "app/models/analytics_client.rb")
	var derived []facts.Fact
	for _, f := range ff {
		if f.Props["derived"] == "wrapper-method" {
			derived = append(derived, f)
		}
	}
	if len(derived) != 2 {
		t.Fatalf("got %d wrapper-method routes, want 2: %+v", len(derived), derived)
	}
	if derived[0].Name != "/exports/{}/events" || derived[0].Props["method"] != "GET" {
		t.Fatalf("first derived route = %+v, want GET /exports/{}/events", derived[0])
	}
	if derived[0].Props["target_hint"] != "analytics" {
		t.Fatalf("env base must hint the provider, got %v", derived[0].Props["target_hint"])
	}
	if derived[0].Props["framework"] != "typhoeus" {
		t.Fatalf("framework = %v, want typhoeus", derived[0].Props["framework"])
	}
}

// TestRubyHTTPClient_TyphoeusWrapperAmbiguityDerivesNothing pins the single-sink
// rule: two Typhoeus sinks in one file, or a tail identifier that is not a
// parameter of the enclosing def, derive nothing.
func TestRubyHTTPClient_TyphoeusWrapperAmbiguityDerivesNothing(t *testing.T) {
	twoSinks := []byte(`class C
  def a(path:)
    Typhoeus::Request.new("#{base}#{path}", method: :get)
  end
  def b(path:)
    Typhoeus::Request.new("#{base}#{path}", method: :post)
  end
  def use
    a(path: "/x/y")
  end
end
`)
	if got := extractRubyHTTPClientFacts(twoSinks, "app/models/c.rb"); len(got) != 0 {
		t.Fatalf("two sinks must derive nothing, got %+v", got)
	}
	notParam := []byte(`class C
  def fire(other:)
    Typhoeus::Request.new("#{base}#{path}", method: :get)
  end
  def use
    fire(other: "/x/y")
  end
end
`)
	if got := extractRubyHTTPClientFacts(notParam, "app/models/c2.rb"); len(got) != 0 {
		t.Fatalf("tail not a def parameter must derive nothing, got %+v", got)
	}
}

// TestRubyHTTPClient_TyphoeusDirectVerb pins the direct form: Typhoeus.get with
// a literal path is a plain client call under the typhoeus framework.
func TestRubyHTTPClient_TyphoeusDirectVerb(t *testing.T) {
	src := []byte("Typhoeus.get(\"/health/check\")\n")
	ff := extractRubyHTTPClientFacts(src, "app/services/probe.rb")
	if len(ff) != 1 || ff[0].Name != "/health/check" || ff[0].Props["framework"] != "typhoeus" {
		t.Fatalf("direct Typhoeus verb = %+v, want one GET /health/check via typhoeus", ff)
	}
}

// TestRubyHTTPClient_HintOnlyFromURLShapedEnvVars pins the garbage-hint
// regression from the estate: a file whose FIRST env read is a rate limiter
// (INSIGHTS_CONCURRENT_RATE_LIMIT) must not hint at all from it, and the
// wrapper derivation must take its hint from the sink's base-identifier
// assignment instead — a wrong hint steers the matcher toward a wrong edge.
func TestRubyHTTPClient_HintOnlyFromURLShapedEnvVars(t *testing.T) {
	if h := envVarHint(`LIMIT = Sidekiq::Limiter.concurrent("x", ENV.fetch("ANALYTICS_CONCURRENT_RATE_LIMIT", 1))`); h != "" {
		t.Fatalf("a non-URL env name must yield no hint, got %q", h)
	}
	src := []byte(`LIMIT = ENV.fetch("ANALYTICS_CONCURRENT_RATE_LIMIT", 1)
class AnalyticsClient
  def events(company_id:)
    make_request(
      path: "/exports/#{company_id}/events",
      params: {}
    )
  end

  private

  def initialize
    @base_url = ENV["ANALYTICS_BASE_URL"]
  end

  def make_request(path:, params:)
    request = Typhoeus::Request.new(
      "#{base_url}#{path}",
      method: :get,
      params: params
    )
    request.run
  end
end
`)
	ff := extractRubyHTTPClientFacts(src, "app/models/analytics_client.rb")
	found := false
	for _, f := range ff {
		if f.Props["derived"] == "wrapper-method" {
			found = true
			if f.Props["target_hint"] != "analytics" {
				t.Fatalf("hint must come from the base assignment, got %v", f.Props["target_hint"])
			}
		}
	}
	if !found {
		t.Fatal("wrapper-method route not derived")
	}
}

// A path that reaches the client through a method parameter is derived from
// the literals the method's callers pass, one hop, same file: each literal
// becomes a client fact marked derived, a non-literal argument is counted,
// and the parameter position is read from the def, not assumed first.
func TestRubyHTTPClient_ParameterPath(t *testing.T) {
	src := `
class Insights
  def visitors_report
    fetch_report("/reports/visitors")
  end

  def referrers_report(sources:)
    fetch_report("/reports/referrers", sources:)
  end

  def custom_report(name)
    fetch_report(name)
  end

  def raw(kind, path)
    fetch_typed(kind, path)
  end

  def typed_devices
    fetch_typed(:json, "/reports/devices")
  end

  def fetch_report(path, params = {})
    response = connection.get(build_url(path), params)
    JSON.parse(response.body)
  end

  def fetch_typed(kind, path)
    connection.get(build_url(path))
  end

  def build_url(path)
    "#{ENV["INSIGHTS_HOST_URL"]}#{path}"
  end
end
`
	got, derived, misses := extractRubyHTTPClientFactsCounted([]byte(src), "app/services/insights.rb")
	paths := map[string]string{}
	for _, f := range clientRoutes(got) {
		paths[f.Name] = f.Props["derived"].(string) + ":" + f.Props["via_method"].(string)
	}
	want := map[string]string{
		"/reports/visitors":  "parameter:fetch_report",
		"/reports/referrers": "parameter:fetch_report",
		"/reports/devices":   "parameter:fetch_typed",
	}
	for p, w := range want {
		if paths[p] != w {
			t.Errorf("%s = %q, want %q (all: %v)", p, paths[p], w, paths)
		}
	}
	if len(paths) != 3 || derived != 3 {
		t.Fatalf("want exactly 3 derived paths, got %v (%d)", paths, derived)
	}
	if misses[parameterNonLiteral] != 2 {
		t.Fatalf("custom_report(name) and raw(kind, path) pass non-literals and must be counted: %v", misses)
	}
}

// A method defined twice in the file is ambiguous: nothing is derived and
// the method is counted once.
func TestRubyHTTPClient_ParameterPathAmbiguousMethod(t *testing.T) {
	src := `
class A
  def call
    fetch("/a")
  end
  def fetch(path)
    connection.get(path)
  end
end
class B
  def fetch(path)
    connection.post(path)
  end
end
`
	got, derived, misses := extractRubyHTTPClientFactsCounted([]byte(src), "app/services/a.rb")
	if len(clientRoutes(got)) != 0 || derived != 0 || misses[parameterAmbiguousMethod] != 1 {
		t.Fatalf("ambiguous wrapper must derive nothing: %v %d %v", clientRoutes(got), derived, misses)
	}
}

func TestRubyExternalHost_CountedOnTheExtractionRecord(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/services/sync.rb": "class Sync\n  def fetch\n    conn.get(\"https://#{host}/v1/charges\")\n  end\nend\n",
	})
	record, ok := extractionFact(ff, "ruby:http-client")
	if !ok {
		t.Fatalf("an interpolated host must reach the ruby:http-client record; facts: %+v", ff)
	}
	if got := record.Props["unresolved_macros"]; got != "host-interpolated=1" {
		t.Fatalf("unresolved_macros = %v, want host-interpolated=1", got)
	}
}

func TestRubyExternalHost_CommentIsNotACall(t *testing.T) {
	src := `module Gitlab
  #   Gitlab::HTTP.get('http://www.gitlab.com', allow_local_requests: true)
  #     Typhoeus.get("https://pages.github.com/versions.json")
  # conn.get('users/1')
  def self.versions
    Typhoeus.get("https://pages.github.com/versions.json")
  end
end
`
	got := clientRoutes(extractRubyHTTPClientFacts([]byte(src), "lib/gitlab/http.rb"))
	byName := map[string]facts.Fact{}
	for _, f := range got {
		byName[f.Name] = f
	}
	if len(got) != 2 {
		t.Fatalf("got %d client routes, want the real call and the relative comment: %+v", len(got), got)
	}
	if f, ok := byName["/versions.json"]; !ok || f.Line != 6 {
		t.Fatalf("the call on line 6 is the one external route, got %+v", byName)
	}
	if _, ok := byName["/"]; ok {
		t.Fatalf("a commented-out call to www.gitlab.com is not a call: %+v", byName)
	}
	// A relative path in a comment has always been read; this slice leaves that as
	// it was rather than change an existing fact.
	if _, ok := byName["users/1"]; !ok {
		t.Fatalf("the relative comment's route was already emitted before this slice and is left alone: %+v", byName)
	}
}
