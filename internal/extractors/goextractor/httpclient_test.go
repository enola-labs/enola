package goextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// goClientRoutes filters facts to the outbound HTTP-client routes (excludes
// server routes emitted by extractRoutes).
func goClientRoutes(ff []facts.Fact) []facts.Fact {
	var out []facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Props["role"] == "client" {
			out = append(out, f)
		}
	}
	return out
}

func clientRouteByPath(ff []facts.Fact, path string) (facts.Fact, bool) {
	for _, f := range goClientRoutes(ff) {
		if f.Name == path {
			return f, true
		}
	}
	return facts.Fact{}, false
}

func TestGoHTTPClient_NewRequestAndHelpers(t *testing.T) {
	src := `package svc

import (
	"context"
	"net/http"
)

type C struct{ baseURL string }

func (c *C) calls(ctx context.Context) {
	http.NewRequest("POST", c.baseURL+"/api/checkout/build", nil)
	http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+"/api/items/update", nil)
	http.Get(c.baseURL + "/api/items/list")
	http.Post(c.baseURL+"/api/orders/new", "application/json", nil)
}
`
	ff := extractAll(t, map[string]string{"svc/client.go": src})

	cases := map[string]string{
		"/api/checkout/build": "POST",
		"/api/items/update":   "PUT",
		"/api/items/list":     "GET",
		"/api/orders/new":     "POST",
	}
	for path, method := range cases {
		f, ok := clientRouteByPath(ff, path)
		if !ok {
			t.Errorf("missing client route %s", path)
			continue
		}
		if f.Props["method"] != method {
			t.Errorf("%s method = %v, want %s", path, f.Props["method"], method)
		}
		if f.Props["source"] != "go-http-client" {
			t.Errorf("%s source = %v, want go-http-client", path, f.Props["source"])
		}
	}
}

func TestGoHTTPClient_WrapperSingleArg(t *testing.T) {
	src := `package svc

import "net/http"

var _ = http.MethodGet

type api struct{ client interface{ Get(string) error } }

func (a *api) load(baseURL string) {
	a.client.Get(baseURL + "/api/profile/me")
}
`
	ff := extractAll(t, map[string]string{"svc/api.go": src})
	if _, ok := clientRouteByPath(ff, "/api/profile/me"); !ok {
		t.Errorf("missing wrapper client route /api/profile/me: %+v", goClientRoutes(ff))
	}
}

func TestGoHTTPClient_EnvVarHint(t *testing.T) {
	src := `package svc

import (
	"net/http"
	"os"
)

func call() {
	http.Get(os.Getenv("XENDO_URL") + "/api/things/list")
}
`
	ff := extractAll(t, map[string]string{"svc/x.go": src})
	f, ok := clientRouteByPath(ff, "/api/things/list")
	if !ok {
		t.Fatalf("missing client route /api/things/list")
	}
	if f.Props["target_hint"] != "xendo" {
		t.Errorf("target_hint = %v, want xendo", f.Props["target_hint"])
	}
}

func TestGoHTTPClient_NotServerHandler(t *testing.T) {
	// chi route registration (2 args) and handler writes must NOT become client
	// routes; only the server route from extractRoutes should exist.
	src := `package svc

import "net/http"

func register(r interface {
	Get(string, http.HandlerFunc)
}) {
	r.Get("/api/widgets", handleWidgets)
}

func handleWidgets(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(200)
	w.Write([]byte("ok"))
}
`
	ff := extractAll(t, map[string]string{"svc/server.go": src})
	for _, f := range goClientRoutes(ff) {
		t.Errorf("unexpected client route from server code: %s", f.Name)
	}
}

func TestGoHTTPClient_SkipsExternal(t *testing.T) {
	src := `package svc

import "net/http"

func call() {
	http.Get("https://api.stripe.com/v1/charges")
}
`
	ff := extractAll(t, map[string]string{"svc/ext.go": src})
	if rs := goClientRoutes(ff); len(rs) != 0 {
		t.Errorf("got %d client routes for external URL, want 0: %+v", len(rs), rs)
	}
}

// --- external base-URL resolution (GAP-LK-02 / GAP-XL-13, v101) ---
//
// A `baseURL + "/path"` concatenation keeps only the path, so the host was lost
// before anything could mark the call third-party. The linker then counted the
// call site as an unresolved *internal* edge and flipped an isolated service to
// coverage_gap. These pin the recovery of the host, for both idioms golf uses.

func TestGoHTTPClient_PackageConstAbsoluteBaseURL_TaggedExternalWithHost(t *testing.T) {
	src := `package svc

import "net/http"

const baseURL = "https://connect.mailerlite.com/api"

func call() {
	http.NewRequest("POST", baseURL+"/subscribers", nil)
}
`
	ff := extractAll(t, map[string]string{"svc/client.go": src})
	f, ok := clientRouteByPath(ff, "/subscribers")
	if !ok {
		t.Fatalf("missing client route /subscribers; got %+v", goClientRoutes(ff))
	}
	if ext, _ := f.Props["external"].(bool); !ext {
		t.Errorf("external = %v, want true", f.Props["external"])
	}
	if f.Props["host"] != "connect.mailerlite.com" {
		t.Errorf("host = %v, want connect.mailerlite.com", f.Props["host"])
	}
}

// A const declared in a sibling file of the same package must resolve too.
func TestGoHTTPClient_PackageScopedConstFromSiblingFile_TaggedExternal(t *testing.T) {
	konst := `package svc

const baseURL = "https://api.example.com"
`
	src := `package svc

import "net/http"

func call() {
	http.Get(baseURL + "/v1/widgets")
}
`
	ff := extractAll(t, map[string]string{"svc/const.go": konst, "svc/client.go": src})
	f, ok := clientRouteByPath(ff, "/v1/widgets")
	if !ok {
		t.Fatalf("missing client route /v1/widgets; got %+v", goClientRoutes(ff))
	}
	if ext, _ := f.Props["external"].(bool); !ext {
		t.Errorf("external = %v, want true", f.Props["external"])
	}
	if f.Props["host"] != "api.example.com" {
		t.Errorf("host = %v, want api.example.com", f.Props["host"])
	}
}

// golf's zeptomail idiom: a struct field fed by a region switch. Every literal
// bound to the name is absolute, so the call is external — but the hosts differ,
// so no single host may be claimed.
func TestGoHTTPClient_StructFieldAbsoluteBaseURL_TaggedExternalNoHostWhenAmbiguous(t *testing.T) {
	src := `package svc

import "net/http"

type S struct{ baseURL string }

func New(region string) *S {
	var baseURL string
	switch region {
	case "eu":
		baseURL = "https://api.zeptomail.eu/v1.1"
	case "in":
		baseURL = "https://api.zeptomail.in/v1.1"
	default:
		baseURL = "https://api.zeptomail.com/v1.1"
	}
	return &S{baseURL: baseURL}
}

func (s *S) send() {
	http.NewRequest("POST", s.baseURL+"/email", nil)
}
`
	ff := extractAll(t, map[string]string{"svc/zepto.go": src})
	f, ok := clientRouteByPath(ff, "/email")
	if !ok {
		t.Fatalf("missing client route /email; got %+v", goClientRoutes(ff))
	}
	if ext, _ := f.Props["external"].(bool); !ext {
		t.Errorf("external = %v, want true", f.Props["external"])
	}
	if h, has := f.Props["host"]; has {
		t.Errorf("host = %v, want absent (three regions disagree)", h)
	}
}

// The oapi-codegen control. options.BaseURL is injected from config and has no
// string-literal binding anywhere in the package, so the call stays internal and
// must remain visible as an unresolved edge.
func TestGoHTTPClient_ConfigInjectedBaseURL_NotTaggedExternal(t *testing.T) {
	src := `package svc

import "net/http"

type Options struct{ BaseURL string }

type C struct{ options Options }

func (c *C) call() {
	http.Get(c.options.BaseURL + "/api/v1/things")
}
`
	ff := extractAll(t, map[string]string{"svc/client.go": src})
	f, ok := clientRouteByPath(ff, "/api/v1/things")
	if !ok {
		t.Fatalf("missing client route /api/v1/things; got %+v", goClientRoutes(ff))
	}
	if _, has := f.Props["external"]; has {
		t.Errorf("external = %v, want absent for a config-injected base URL", f.Props["external"])
	}
	if _, has := f.Props["host"]; has {
		t.Errorf("host = %v, want absent", f.Props["host"])
	}
}

// A relative literal base ("/api") is not a host. Mixed relative+absolute
// bindings must also bail, rather than guess.
func TestGoHTTPClient_RelativeLiteralBaseURL_NotTaggedExternal(t *testing.T) {
	src := `package svc

import "net/http"

const prefix = "/api"

func call() {
	http.Get(prefix + "/v1/things")
}
`
	ff := extractAll(t, map[string]string{"svc/client.go": src})
	f, ok := clientRouteByPath(ff, "/v1/things")
	if !ok {
		t.Fatalf("missing client route /v1/things; got %+v", goClientRoutes(ff))
	}
	if _, has := f.Props["external"]; has {
		t.Errorf("external = %v, want absent for a relative base", f.Props["external"])
	}
}

func TestGoHTTPClient_MixedAbsoluteAndRelativeBindings_NotTaggedExternal(t *testing.T) {
	src := `package svc

import "net/http"

type S struct{ baseURL string }

func New(local bool) *S {
	var baseURL string
	if local {
		baseURL = "/internal"
	} else {
		baseURL = "https://api.example.com"
	}
	return &S{baseURL: baseURL}
}

func (s *S) call() {
	http.Get(s.baseURL + "/v1/things")
}
`
	ff := extractAll(t, map[string]string{"svc/client.go": src})
	f, ok := clientRouteByPath(ff, "/v1/things")
	if !ok {
		t.Fatalf("missing client route /v1/things; got %+v", goClientRoutes(ff))
	}
	if _, has := f.Props["external"]; has {
		t.Errorf("external = %v, want absent when a binding is relative", f.Props["external"])
	}
}

// Tagging must not move the route name: fact identity is keyed on it, and the
// linker matches server routes by path suffix.
func TestGoHTTPClient_RouteNameUnchangedByExternalTagging(t *testing.T) {
	src := `package svc

import "net/http"

const baseURL = "https://api.example.com/v2"

func call() {
	http.Get(baseURL + "/v1/things")
}
`
	ff := extractAll(t, map[string]string{"svc/client.go": src})
	if _, ok := clientRouteByPath(ff, "/v1/things"); !ok {
		t.Fatalf("route name changed; got %+v", goClientRoutes(ff))
	}
}

// A value in the middle of a concatenated URL is a path parameter. Reading only
// the rightmost absolute literal turned the first call into "/status" and the
// second into the collection route "/api/v1/orders/".
func TestGoHTTPClient_ConcatenatedPathParameters(t *testing.T) {
	src := `package svc

import (
	"net/http"
	"net/url"
)

type C struct{ baseURL string }

func (c *C) calls(id, sku, q, name, suffix string) {
	http.NewRequest("POST", c.baseURL+"/api/v1/orders/"+id+"/status", nil)
	http.Get(c.baseURL + "/api/v1/orders/" + id)
	http.Get(c.baseURL + "/api/v1/inventory/" + url.PathEscape(sku))
	http.Get(c.baseURL + "/api/v1/search?q=" + q)
	http.Get(c.baseURL + "/api/v1/files/" + name + ".json")
	http.Get(c.baseURL + "/api/v1/items" + suffix)
	http.Get(c.baseURL + "/api" + "/v2/things")
	http.Get((c.baseURL + "/api/v1/customers/") + id + "/orders/" + id)
	http.Get(c.baseURL + "/" + id)
	http.Get(c.baseURL + "/" + id + "/" + sku)
}
`
	ff := extractAll(t, map[string]string{"svc/client.go": src})

	want := map[string]string{
		"/api/v1/orders/{}/status":       "POST",
		"/api/v1/orders/{}":              "GET",
		"/api/v1/inventory/{}":           "GET",
		"/api/v1/search":                 "GET",
		"/api/v1/files/{}":               "GET",
		"/api/v1/items":                  "GET",
		"/api/v2/things":                 "GET",
		"/api/v1/customers/{}/orders/{}": "GET",
	}
	for path, method := range want {
		f, ok := clientRouteByPath(ff, path)
		if !ok {
			t.Errorf("missing client route %s", path)
			continue
		}
		if f.Props["method"] != method {
			t.Errorf("%s method = %v, want %s", path, f.Props["method"], method)
		}
	}
	if got := goClientRoutes(ff); len(got) != len(want) {
		var names []string
		for _, f := range got {
			names = append(names, f.Name)
		}
		t.Errorf("client routes = %v, want exactly %d", names, len(want))
	}
}
