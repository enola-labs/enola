package goextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// ownRouterModule is an application that carries its own router: a web package with
// the verbs and no framework import, and a routing package that registers on it.
func ownRouterModule(extra map[string]string) map[string]string {
	files := map[string]string{
		"pkg/web/router.go": `package web

import "net/http"

type Handler any

type Router struct{}

func (r *Router) Handle(method, pattern string, h []Handler) {}
func (r *Router) Get(pattern string, h ...Handler)    { r.Handle("GET", pattern, h) }
func (r *Router) Post(pattern string, h ...Handler)   { r.Handle("POST", pattern, h) }
func (r *Router) Put(pattern string, h ...Handler)    { r.Handle("PUT", pattern, h) }
func (r *Router) Delete(pattern string, h ...Handler) { r.Handle("DELETE", pattern, h) }

var _ http.Handler
`,
		"pkg/routing/register.go": `package routing

import "example.com/app/pkg/web"

type RouteRegister interface {
	Get(string, ...web.Handler)
	Post(string, ...web.Handler)
	Put(string, ...web.Handler)
	Delete(string, ...web.Handler)
	Group(string, func(RouteRegister), ...web.Handler)
}

func Wrap(h any) web.Handler { return h }
`,
	}
	for k, v := range extra {
		files[k] = v
	}
	return files
}

// goRouteTargets is goRoutes for the handler target each route resolved.
func goRouteTargets(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	repo := t.TempDir()
	files["go.mod"] = "module example.com/app\n\ngo 1.22\n"
	rels := make([]string, 0, len(files))
	for rel, content := range files {
		abs := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	ff, err := New().Extract(context.Background(), repo, rels)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute {
			out[f.PropString("method")+" "+f.Name] = f.PropString(facts.PropHandlerTarget)
		}
	}
	return out
}

// Routes registered on a router field, on a variable assigned from it, and inside
// its groups are read at their full path and name the receiver's method.
func TestOwnRouter_RoutesOnAFieldAndInGroups(t *testing.T) {
	got := goRouteTargets(t, ownRouterModule(map[string]string{
		"pkg/api/api.go": `package api

import (
	"example.com/app/pkg/routing"
	"example.com/app/pkg/web"
)

type HTTPServer struct {
	RouteRegister routing.RouteRegister
}

func (hs *HTTPServer) registerRoutes() {
	r := hs.RouteRegister
	r.Get("/login", hs.LoginView)
	r.Post("/login", quota("auth"), routing.Wrap(hs.LoginPost))
	r.Group("/api", func(apiRoute routing.RouteRegister) {
		apiRoute.Post("/ds/query", authorize(), hs.queryEndpoint())
		apiRoute.Group("/dashboards", func(d routing.RouteRegister) {
			d.Post("/db", routing.Wrap(hs.PostDashboard))
			d.Get("/wrapped", routing.Wrap(hs.guard(hs.Inner, "message")))
		})
	})
	hs.RouteRegister.Delete("/logout", hs.Logout)
}

func (hs *HTTPServer) LoginView(c *web.Router)     {}
func (hs *HTTPServer) LoginPost(c *web.Router)     {}
func (hs *HTTPServer) PostDashboard(c *web.Router) {}
func (hs *HTTPServer) Logout(c *web.Router)        {}
func (hs *HTTPServer) Inner(c *web.Router)         {}
func (hs *HTTPServer) queryEndpoint() web.Handler  { return nil }
func (hs *HTTPServer) guard(h any, msg string) any { return h }
`,
	}))
	wantGoRoutes(t, got, map[string]string{
		"GET /login":                  "pkg/api.HTTPServer.LoginView",
		"POST /login":                 "pkg/api.HTTPServer.LoginPost",
		"POST /api/ds/query":          "pkg/api.HTTPServer.queryEndpoint",
		"POST /api/dashboards/db":     "pkg/api.HTTPServer.PostDashboard",
		"GET /api/dashboards/wrapped": "",
		"DELETE /logout":              "pkg/api.HTTPServer.Logout",
	})
}

// The application's web package is imported for more than routing. A call that
// shares a verb's name on anything but a router is not a route, and the router's own
// implementation registers none.
func TestOwnRouter_OnlyAKnownRouterRegisters(t *testing.T) {
	got := goRouteTargets(t, ownRouterModule(map[string]string{
		"pkg/cache/cache.go": `package cache

import (
	"net/http"

	"example.com/app/pkg/web"
)

type Store struct{ items map[string]any }

func (s *Store) Get(key string, into any) {}

func warm(s *Store, c *web.Router, settings *Store) {
	s.Get("users", &struct{}{})
	settings.Get("/section/key", "fallback")
}

func mount(mux *http.ServeMux) {
	mux.HandleFunc("/resources", serve)
}

func serve(w http.ResponseWriter, r *http.Request) {}
`,
	}))
	for key := range got {
		switch key {
		case "ALL /resources":
		default:
			t.Errorf("unexpected route %q; have %v", key, got)
		}
	}
	if _, ok := got["ALL /resources"]; !ok {
		t.Errorf("the net/http route of a file that imports the web package was lost: %v", got)
	}
}

// A type with a client's verbs is not a router: its Get takes a URL and returns.
func TestOwnRouter_AnHTTPClientIsNotARouter(t *testing.T) {
	got := goRouteTargets(t, map[string]string{
		"pkg/client/client.go": `package client

type Client struct{}

type Option func()

func (c *Client) Get(path string, opts ...Option) (*Response, error)    { return nil, nil }
func (c *Client) Post(path string, opts ...Option) (*Response, error)   { return nil, nil }
func (c *Client) Put(path string, opts ...Option) (*Response, error)    { return nil, nil }
func (c *Client) Delete(path string, opts ...Option) (*Response, error) { return nil, nil }

type Response struct{}
`,
		"pkg/sync/sync.go": `package sync

import "example.com/app/pkg/client"

func run(c *client.Client, retry client.Option) {
	c.Get("/api/users", retry)
	c.Post("/api/users", retry)
}
`,
	})
	if len(got) != 0 {
		t.Errorf("an HTTP client's calls were read as routes: %v", got)
	}
}

func TestRouteHandlerExpr_FactoryBuiltFromAValue(t *testing.T) {
	got := goRoutes(t, map[string]string{
		"routers/web.go": `package routers

import "github.com/go-chi/chi/v5"

func Routes() chi.Router {
	m := chi.NewRouter()
	m.Get("/actions", redirect("./actions/general"))
	m.Get("/orgs", list, requireScope(scopeOrganization))
	return m
}
`,
	})
	wantGoRoutes(t, got, map[string]string{
		"GET /actions": "redirect",
		"GET /orgs":    "scopeOrganization",
	})
}
