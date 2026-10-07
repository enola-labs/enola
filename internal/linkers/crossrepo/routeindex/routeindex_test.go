package routeindex

import (
	"github.com/enola-labs/enola/internal/facts"
	"testing"

	"github.com/enola-labs/enola/internal/linkers/vocab"
)

func TestNormalizePath(t *testing.T) {
	m := New(vocab.Default())
	cases := map[string]string{
		"/api/items/{id}":         "/api/items/{}",
		"/api/items/:id":          "/api/items/{}",
		"/api/items/<id>":         "/api/items/{}",
		"/api/items/[id]":         "/api/items/{}",
		"/api/items/":             "/api/items",
		"/api/items":              "/api/items",
		"/":                       "/",
		"/users/{uid}/pets/{pid}": "/users/{}/pets/{}",
		// Response-format suffix stripped from the final segment (Rails ".:format").
		"/v2/devices/{id}.json": "/v2/devices/{}",
		"/v2/bookmarks.json":    "/v2/bookmarks",
		"/v2/report.xml":        "/v2/report",
		// A version-like or genuinely dotted segment is not a format suffix.
		"/api/v2.5/items": "/api/v2.5/items",
	}
	for in, want := range cases {
		if got := m.NormalizePath(in); got != want {
			t.Errorf("m.NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsGenericPath(t *testing.T) {
	m := New(vocab.Default())
	generic := []string{"/health", "/status", "/metrics", "/"}
	for _, p := range generic {
		if !m.IsGenericPath(m.NormalizePath(p)) {
			t.Errorf("m.IsGenericPath(%q) = false, want true", p)
		}
	}
	specific := []string{"/api/items", "/items/{id}", "/a/b"}
	for _, p := range specific {
		if m.IsGenericPath(m.NormalizePath(p)) {
			t.Errorf("m.IsGenericPath(%q) = true, want false", p)
		}
	}
	// The rest of the infra/mount vocabulary: a lone segment is generic because
	// of what it is NAMED, so every one of these must stay unlinkable.
	for _, p := range []string{
		"/healthz", "/healthcheck", "/ping", "/ready", "/readyz",
		"/live", "/livez", "/version", "/info", "/api", "/graphql", "/HEALTH",
	} {
		if !m.IsGenericPath(m.NormalizePath(p)) {
			t.Errorf("m.IsGenericPath(%q) = false, want true (generic vocabulary)", p)
		}
	}
	// Named single-segment endpoints are linkable: a pure segment count used to
	// make these unlinkable, silently dropping real cross-repo edges (an
	// /activate client could never resolve to its /activate server).
	for _, p := range []string{"/activate", "/checkout", "/subscribe", "/{id}"} {
		if m.IsGenericPath(m.NormalizePath(p)) {
			t.Errorf("m.IsGenericPath(%q) = true, want false (named endpoint)", p)
		}
	}
}

func TestSingleSegmentPath(t *testing.T) {
	m := New(vocab.Default())
	for _, p := range []string{"/activate", "/checkout", "activate"} {
		if !SingleSegmentPath(m.NormalizePath(p)) {
			t.Errorf("SingleSegmentPath(%q) = false, want true", p)
		}
	}
	// Multi-segment, parameterized, and empty paths do not take the stricter
	// unambiguous-provider gate in linkHTTP.
	for _, p := range []string{"/a/b", "/items/{id}", "/{id}", "/"} {
		if SingleSegmentPath(m.NormalizePath(p)) {
			t.Errorf("SingleSegmentPath(%q) = true, want false", p)
		}
	}
}

func TestIsUIRoute_PageRoutesExcludedFromServerIndex(t *testing.T) {
	page := facts.Fact{Kind: facts.KindRoute, Name: "/catalog", Props: map[string]any{
		"type": "page", "method": "GET", "framework": "ember"}}
	api := facts.Fact{Kind: facts.KindRoute, Name: "/api/users", Props: map[string]any{
		"type": "route", "method": "GET", "framework": "nextjs"}}
	rails := facts.Fact{Kind: facts.KindRoute, Name: "/app/companies", Props: map[string]any{
		"method": "GET", "framework": "rails"}}
	if !IsUIRoute(page) {
		t.Error("a page-type route must be a UI route")
	}
	if IsUIRoute(api) || IsUIRoute(rails) {
		t.Error("API and backend routes must stay server-indexable")
	}
	// Every Next.js App Router convention basename the TS extractor emits, except
	// "route". "loading" was the one omission, and it was not cosmetic: a
	// loading.tsx under a single dynamic segment extracts as "/{}", which matches
	// any one-segment client call carrying a path parameter, handing a frontend
	// inbound dependencies it has no endpoint for.
	for _, typ := range []string{"page", "layout", "loading", "error"} {
		f := facts.Fact{Kind: facts.KindRoute, Name: "/{}", Props: map[string]any{
			"type": typ, "method": "GET", "framework": "nextjs"}}
		if !IsUIRoute(f) {
			t.Errorf("type %q must be a UI route — it renders, it does not serve", typ)
		}
	}
	page.Repo = "web"
	m := New(vocab.Default())
	if got := m.IndexServerRoutes([]facts.Fact{page}); len(got) != 0 {
		t.Errorf("server index = %v, want empty — a browser navigation URL is not a served endpoint", got)
	}
}

func TestLookupClientMatches_ClientMethodAny(t *testing.T) {
	m := New(vocab.Default())
	server := m.IndexServerRoutes([]facts.Fact{{
		Kind: facts.KindRoute, Name: "/mcp", Repo: "backend",
		Props: map[string]any{"method": "POST", "role": "server"},
	}})
	refs, _ := m.LookupClientMatches(server, "/mcp", facts.MethodAny)
	if len(refs) != 1 || refs[0].Repo != "backend" {
		t.Fatalf("method-less client did not match the path's server: %+v", refs)
	}
	if refs, _ := m.LookupClientMatches(server, "/absent", facts.MethodAny); len(refs) != 0 {
		t.Fatalf("method-less client matched an unserved path: %+v", refs)
	}
}

// The suffix join drops a leading prefix on either side. Dropping one on both
// joined "/api/v1/orders/{}" to "/api/v2/orders/{}" at "/orders/{}": after a
// route moved to v2, every v1 caller still resolved to it.
func TestLookupClientMatches_VersionsThatDisagreeDoNotJoin(t *testing.T) {
	m := New(vocab.Default())
	route := func(path string) facts.Fact {
		return facts.Fact{Kind: facts.KindRoute, Name: path, Repo: "backend",
			Props: map[string]any{"method": "GET", "role": "server"}}
	}
	cases := []struct {
		name, server, client string
		want                 bool
	}{
		{"another version", "/api/v2/orders/{id}", "/api/v1/orders/{}", false},
		{"another version, no shared prefix", "/api/v2/orders/{id}", "/v1/orders/{}", false},
		{"dotted and cased versions", "/api/V2.1/orders/{id}", "/api/v2/orders/{}", false},
		{"same version", "/api/v1/orders/{id}", "/api/v1/orders/{}", true},
		{"same version under a gateway prefix", "/v1/orders/{id}", "/gateway/v1/orders/{}", true},
		{"client states no version", "/api/v2/orders/{id}", "/gateway/orders/{}", true},
		{"server states no version", "/internal/orders/{id}", "/api/v1/orders/{}", true},
		{"base-relative client", "/api/v2/orders/{id}", "/orders/{}", true},
		{"a number is an id, not a version", "/tenants/2/orders/{id}", "/tenants/1/orders/{}", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := m.IndexServerRoutes([]facts.Fact{route(tc.server)})
			refs, _ := m.LookupClientMatches(server, tc.client, "GET")
			if got := len(refs) > 0; got != tc.want {
				t.Errorf("%s against %s: matched = %v, want %v", tc.client, tc.server, got, tc.want)
			}
			if got := m.ServedAtAnotherVersion(server, tc.client, "GET"); got == tc.want {
				t.Errorf("%s against %s: ServedAtAnotherVersion = %v, want %v", tc.client, tc.server, got, !tc.want)
			}
		})
	}
}

// With both versions served, the call reaches its own and only its own.
func TestLookupClientMatches_PicksItsOwnVersion(t *testing.T) {
	m := New(vocab.Default())
	var all []facts.Fact
	for _, p := range []string{"/api/v1/orders/{id}", "/api/v2/orders/{id}"} {
		all = append(all, facts.Fact{Kind: facts.KindRoute, Name: p, Repo: "backend",
			Props: map[string]any{"method": "GET", "role": "server"}})
	}
	server := m.IndexServerRoutes(all)
	refs, _ := m.LookupClientMatches(server, "/gateway/v2/orders/{}", "GET")
	if len(refs) != 1 || refs[0].Path != "/api/v2/orders/{id}" {
		t.Fatalf("refs = %+v, want only the v2 route", refs)
	}
}
