package goextractor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// ginRoutes parses one source file and returns its route facts keyed by "METHOD path".
func ginRoutes(t *testing.T, src string) map[string]facts.Fact {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := map[string]facts.Fact{}
	for _, fact := range extractRoutes(fset, f, "main.go", "cmd/server", routeIndex{}, nil) {
		out[fact.PropString("method")+" "+fact.Name] = fact
	}
	return out
}

// TestGinRoutes pins the registration forms.
func TestGinRoutes(t *testing.T) {
	src := `package main

import "github.com/gin-gonic/gin"

func main() {
	r := gin.Default()
	r.GET("/ping", pingHandler)
	r.POST("/events", eventHandler)
	r.DELETE("/users/:id", deleteUser)
	r.Any("/health", healthHandler)
	r.Handle("PATCH", "/settings", patchSettings)
	r.Use(authMiddleware)
	r.GET("/traced", tracingMiddleware, tracedHandler)
}
`
	got := ginRoutes(t, src)

	for _, want := range []struct{ key, handler string }{
		{"GET /ping", "pingHandler"},
		{"POST /events", "eventHandler"},
		{"DELETE /users/:id", "deleteUser"},
		{"ALL /health", "healthHandler"},
		{"PATCH /settings", "patchSettings"},
	} {
		f, ok := got[want.key]
		if !ok {
			t.Errorf("missing route %q; got %v", want.key, keys(got))
			continue
		}
		if f.PropString(facts.PropFramework) != "gin" {
			t.Errorf("%s: framework = %q, want gin", want.key, f.PropString(facts.PropFramework))
		}
		if f.PropString("handler") != want.handler {
			t.Errorf("%s: handler = %q, want %q", want.key, f.PropString("handler"), want.handler)
		}
	}

	// gin takes variadic middleware BEFORE the handler, so the last argument is the
	// one that serves the request. Taking the second would name the middleware.
	if f, ok := got["GET /traced"]; !ok {
		t.Error("missing GET /traced")
	} else if f.PropString("handler") != "tracedHandler" {
		t.Errorf("with middleware present the handler should be the LAST arg, got %q",
			f.PropString("handler"))
	}

	if _, ok := got["USE /"]; !ok {
		if _, ok := got["USE "]; !ok {
			t.Errorf("r.Use should register middleware; got %v", keys(got))
		}
	}
}

// TestGinGroupPrefixIsJoinedNotConcatenated is the guard on the decision that decides
// whether gin extraction is usable at all.
//
// gin spells a group that adds no prefix as `Group("/")`, and real code leans on it —
// ente's server opens seven of them to attach different middleware stacks. Plain
// concatenation turns every route beneath one into "//ping": a path the server does
// not serve, that no client route can match, and that therefore silently destroys the
// cross-repo edge the routes exist to produce.
func TestGinGroupPrefixIsJoinedNotConcatenated(t *testing.T) {
	src := `package main

import "github.com/gin-gonic/gin"

func main() {
	server := gin.Default()

	publicAPI := server.Group("/")
	publicAPI.GET("/ping", pingHandler)

	privateAPI := server.Group("/")
	storageAPI := privateAPI.Group("/")
	storageAPI.GET("/files/upload-url", uploadURL)

	adminAPI := server.Group("/admin")
	adminAPI.DELETE("/user/delete", deleteUser)

	castAPI := server.Group("/cast")
	castAPI.GET("/device-info/:deviceID", deviceInfo)
}
`
	got := ginRoutes(t, src)

	for _, want := range []string{
		"GET /ping",                       // under Group("/")
		"GET /files/upload-url",           // under Group("/") nested in Group("/")
		"DELETE /admin/user/delete",       // a real prefix composes
		"GET /cast/device-info/:deviceID", // and keeps path params
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q; got %v", want, keys(got))
		}
	}
	for k := range got {
		if containsDoubleSlash(k) {
			t.Errorf("route %q has a doubled separator — Group(\"/\") was concatenated "+
				"rather than joined", k)
		}
	}
}

// TestChiGroupIsNotAGinMount pins the discriminator between two identically-named
// methods that mean opposite things.
//
// chi's `r.Group(func(r chi.Router){…})` takes a FUNCTION and mounts nothing; gin's
// `r.Group("/admin")` takes a STRING and mounts a prefix. The extractor tells them
// apart by the argument's shape rather than by naming a framework, so a chi Group can
// never be mistaken for a mount even though the selector text is identical.
func TestChiGroupIsNotAGinMount(t *testing.T) {
	src := `package main

import "github.com/go-chi/chi/v5"

func main() {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Get("/dashboard", dashboard)
	})
	r.Get("/health", health)
}
`
	got := ginRoutes(t, src)
	// The chi Group binds no prefix, so the inner route keeps its own path. If Group
	// were read as a gin mount, the func-literal argument would yield an empty prefix
	// and — worse — a later string-argument Group elsewhere could compose onto it.
	if _, ok := got["GET /dashboard"]; !ok {
		t.Errorf("chi route inside Group(func) should keep its own path; got %v", keys(got))
	}
	if _, ok := got["GET /health"]; !ok {
		t.Errorf("missing GET /health; got %v", keys(got))
	}
	for k, f := range got {
		if f.PropString(facts.PropFramework) == "gin" {
			t.Errorf("route %q in a chi file was attributed to gin", k)
		}
	}
}

// TestGinHandlerBinding pins that a gin handler signature is recognised, which is what
// lets a route bind to the function serving it.
//
// It is tagged by SIGNATURE at parse time, not by name and not by the binder: the
// binder keys only on the prop, so it stays free of framework knowledge and a router
// gains binding by describing its handler shape where signatures are already read.
func TestGinHandlerBinding(t *testing.T) {
	src := `package main

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

func pingHandler(c *gin.Context) {}

func classic(w http.ResponseWriter, r *http.Request) {}

func notAHandler(c *gin.Context, extra int) {}

func alsoNot(s string) {}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tagged := map[string]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if isHTTPHandlerSignature(fn.Type) {
			tagged[fn.Name.Name] = true
		}
	}
	if !tagged["pingHandler"] {
		t.Error("func(c *gin.Context) should be recognised as an HTTP handler signature")
	}
	if !tagged["classic"] {
		t.Error("the net/http signature must keep its tag")
	}
	for _, name := range []string{"notAHandler", "alsoNot"} {
		if tagged[name] {
			t.Errorf("%s is not a handler shape but was tagged", name)
		}
	}
}

func containsDoubleSlash(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '/' && s[i+1] == '/' {
			return true
		}
	}
	return false
}
