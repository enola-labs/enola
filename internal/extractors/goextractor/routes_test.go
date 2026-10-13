package goextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestExtractRoutes_GorillaMux_HandleFuncWithMethods(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import (
	"net/http"
	"github.com/gorilla/mux"
)

func SetupRoutes() {
	router := mux.NewRouter()
	router.HandleFunc("/api/users", GetUsers).Methods("GET")
	router.HandleFunc("/api/users", CreateUser).Methods("POST")
	router.HandleFunc("/api/users/{id}", GetUser).Methods("GET")
	_ = router
}

func GetUsers(w http.ResponseWriter, r *http.Request)  {}
func CreateUser(w http.ResponseWriter, r *http.Request) {}
func GetUser(w http.ResponseWriter, r *http.Request)    {}
`,
	})

	routes := findFactsByKind(ff, facts.KindRoute)

	// Should find 3 routes
	if len(routes) < 3 {
		t.Fatalf("expected at least 3 route facts, got %d", len(routes))
	}

	// Check GET /api/users
	found := false
	for _, r := range routes {
		if r.Name == "/api/users" && r.Props["method"] == "GET" {
			found = true
			if r.Props["handler"] != "GetUsers" {
				t.Errorf("handler = %v, want GetUsers", r.Props["handler"])
			}
			if r.Props["framework"] != "gorilla/mux" {
				t.Errorf("framework = %v, want gorilla/mux", r.Props["framework"])
			}
		}
	}
	if !found {
		t.Error("expected route fact for GET /api/users")
	}

	// Check route with parameter
	found = false
	for _, r := range routes {
		if r.Name == "/api/users/{id}" && r.Props["method"] == "GET" {
			found = true
		}
	}
	if !found {
		t.Error("expected route fact for GET /api/users/{id}")
	}
}

func TestExtractRoutes_GorillaMux_Subrouter(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import (
	"net/http"
	"github.com/gorilla/mux"
)

func SetupRoutes() {
	router := mux.NewRouter()
	apiRouter := router.PathPrefix("/api").Subrouter()
	apiRouter.HandleFunc("/users", GetUsers).Methods("GET")
	apiRouter.HandleFunc("/users/{id}", GetUser).Methods("GET")

	settingsRouter := apiRouter.PathPrefix("/settings").Subrouter()
	settingsRouter.HandleFunc("/profile", GetProfile).Methods("GET")
	_ = router
}

func GetUsers(w http.ResponseWriter, r *http.Request)   {}
func GetUser(w http.ResponseWriter, r *http.Request)     {}
func GetProfile(w http.ResponseWriter, r *http.Request)  {}
`,
	})

	routes := findFactsByKind(ff, facts.KindRoute)

	// Check /api/users has prefix resolved
	found := false
	for _, r := range routes {
		if r.Name == "/api/users" && r.Props["method"] == "GET" {
			found = true
		}
	}
	if !found {
		t.Error("expected route fact for GET /api/users (with prefix)")
	}

	// Check nested subrouter: /api/settings/profile
	found = false
	for _, r := range routes {
		if r.Name == "/api/settings/profile" && r.Props["method"] == "GET" {
			found = true
		}
	}
	if !found {
		t.Error("expected route fact for GET /api/settings/profile (nested subrouter)")
	}
}

func TestExtractRoutes_GorillaMux_Middleware(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import (
	"github.com/gorilla/mux"
)

func SetupRoutes() {
	router := mux.NewRouter()
	router.Use(AuthMiddleware)
	apiRouter := router.PathPrefix("/api").Subrouter()
	apiRouter.Use(RateLimitMiddleware)
	_ = apiRouter
}

func AuthMiddleware() {}
func RateLimitMiddleware() {}
`,
	})

	routes := findFactsByKind(ff, facts.KindRoute)

	middlewareCount := 0
	for _, r := range routes {
		if r.Props["method"] == "USE" && r.Props["type"] == "middleware" {
			middlewareCount++
		}
	}
	if middlewareCount < 2 {
		t.Errorf("expected at least 2 middleware facts, got %d", middlewareCount)
	}
}

func TestExtractRoutes_GorillaMux_HandleWithoutMethods(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import (
	"net/http"
	"github.com/gorilla/mux"
)

func SetupRoutes() {
	router := mux.NewRouter()
	router.HandleFunc("/health", HealthCheck)
	_ = router
}

func HealthCheck(w http.ResponseWriter, r *http.Request) {}
`,
	})

	routes := findFactsByKind(ff, facts.KindRoute)

	found := false
	for _, r := range routes {
		if r.Name == "/health" && r.Props["method"] == "ALL" {
			found = true
		}
	}
	if !found {
		t.Error("expected route fact for ALL /health (no .Methods() chain)")
	}
}

func TestExtractRoutes_Chi(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import (
	"net/http"
	"github.com/go-chi/chi/v5"
)

func SetupRoutes() {
	r := chi.NewRouter()
	r.Get("/api/users", GetUsers)
	r.Post("/api/users", CreateUser)
	r.Delete("/api/users/{id}", DeleteUser)
}

func GetUsers(w http.ResponseWriter, r *http.Request)    {}
func CreateUser(w http.ResponseWriter, r *http.Request)  {}
func DeleteUser(w http.ResponseWriter, r *http.Request)  {}
`,
	})

	routes := findFactsByKind(ff, facts.KindRoute)
	if len(routes) < 3 {
		t.Fatalf("expected at least 3 chi route facts, got %d", len(routes))
	}

	found := false
	for _, r := range routes {
		if r.Name == "/api/users" && r.Props["method"] == "GET" && r.Props["framework"] == "chi" {
			found = true
		}
	}
	if !found {
		t.Error("expected chi route fact for GET /api/users")
	}
}

func TestExtractRoutes_NetHTTP(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"cmd/main.go": `package main

import "net/http"

func main() {
	http.HandleFunc("/health", healthHandler)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {}
`,
	})

	routes := findFactsByKind(ff, facts.KindRoute)

	found := false
	for _, r := range routes {
		if r.Name == "/health" && r.Props["framework"] == "net/http" {
			found = true
		}
	}
	if !found {
		t.Error("expected net/http route fact for /health")
	}
}

func TestExtractRoutes_NoRoutes(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/util.go": `package util

func Add(a, b int) int {
	return a + b
}
`,
	})

	routes := findFactsByKind(ff, facts.KindRoute)
	if len(routes) != 0 {
		t.Errorf("expected 0 route facts for non-router file, got %d", len(routes))
	}
}

func TestExtractRoutes_ConditionalRegistration(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import (
	"net/http"
	"github.com/gorilla/mux"
)

type Handler struct{}

func SetupRoutes(h *Handler) {
	router := mux.NewRouter()
	if h != nil {
		router.HandleFunc("/api/feature", h.GetFeature).Methods("GET")
	}
	_ = router
}

func (h *Handler) GetFeature(w http.ResponseWriter, r *http.Request) {}
`,
	})

	routes := findFactsByKind(ff, facts.KindRoute)

	found := false
	for _, r := range routes {
		if r.Name == "/api/feature" && r.Props["method"] == "GET" {
			found = true
		}
	}
	if !found {
		t.Error("expected route fact for GET /api/feature inside if block")
	}
}

// A collection-root route registered as HandleFunc("", h) on a subrouter must be
// emitted at the subrouter's prefix — not dropped by the empty-path guard.
func TestExtractRoutes_GorillaMux_EmptyPathCollectionRoot(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import (
	"net/http"
	"github.com/gorilla/mux"
)

func SetupRoutes() {
	router := mux.NewRouter()
	coursesRouter := router.PathPrefix("/api/courses").Subrouter()
	coursesRouter.HandleFunc("", GetAllCourses).Methods("GET")
	coursesRouter.HandleFunc("", CreateCourse).Methods("POST")
	coursesRouter.HandleFunc("/{id}", GetCourse).Methods("GET")
	_ = router
}

func GetAllCourses(w http.ResponseWriter, r *http.Request) {}
func CreateCourse(w http.ResponseWriter, r *http.Request)  {}
func GetCourse(w http.ResponseWriter, r *http.Request)      {}
`,
	})

	if !hasRoute(ff, "/api/courses", "GET") {
		t.Errorf("want collection-root GET /api/courses; routes = %v", routeNames(ff, "GET"))
	}
	if !hasRoute(ff, "/api/courses", "POST") {
		t.Errorf("want collection-root POST /api/courses; routes = %v", routeNames(ff, "POST"))
	}
	if !hasRoute(ff, "/api/courses/{id}", "GET") {
		t.Errorf("want GET /api/courses/{id}; routes = %v", routeNames(ff, "GET"))
	}
}

// An empty-path registration on a prefix-less router normalizes to the root "/".
func TestExtractRoutes_GorillaMux_EmptyPathRoot(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import (
	"net/http"
	"github.com/gorilla/mux"
)

func SetupRoutes() {
	router := mux.NewRouter()
	router.HandleFunc("", Home).Methods("GET")
	_ = router
}

func Home(w http.ResponseWriter, r *http.Request) {}
`,
	})

	if !hasRoute(ff, "/", "GET") {
		t.Errorf("want root GET /; routes = %v", routeNames(ff, "GET"))
	}
}

// A dynamic (non-literal) first argument yields an unknown path and must still be
// dropped — the empty-path relaxation only applies to static string literals.
func TestExtractRoutes_GorillaMux_DynamicPathDropped(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import (
	"net/http"
	"github.com/gorilla/mux"
)

func SetupRoutes(pathVar string) {
	router := mux.NewRouter()
	router.HandleFunc(pathVar, Dyn).Methods("GET")
	_ = router
}

func Dyn(w http.ResponseWriter, r *http.Request) {}
`,
	})

	// pathVar is not statically known → no route emitted (and definitely not "/").
	routes := findFactsByKind(ff, facts.KindRoute)
	for _, r := range routes {
		if r.Props["method"] == "GET" {
			t.Errorf("expected no GET route for dynamic path arg, got %q", r.Name)
		}
	}
}

// A router with chi's verb names that takes middleware before the handler puts
// the handler last. Read from the second argument, POST /login was served by
// the form binder.
func TestExtractRoutes_ChiVerbWithMiddlewareTakesTheLastArgument(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import (
	"net/http"
	"github.com/go-chi/chi/v5"
)

type router struct{ chi.Router }

func (r *router) Post(pattern string, h ...any) {}
func (r *router) Get(pattern string, h ...any)  {}

func SetupRoutes(m *router) {
	m.Post("/login", Bind(Form{}), SignInPost)
	m.Get("/repo", RequireSignIn, RepoAssignment, Home)
	m.Get("/plain", Plain)
}

type Form struct{}

func Bind(v any) func(http.Handler) http.Handler                  { return nil }
func RequireSignIn(next http.Handler) http.Handler                { return next }
func RepoAssignment(next http.Handler) http.Handler               { return next }
func SignInPost(w http.ResponseWriter, r *http.Request)           {}
func Home(w http.ResponseWriter, r *http.Request)                 {}
func Plain(w http.ResponseWriter, r *http.Request)                {}
`,
	})
	want := map[string]string{"/login": "SignInPost", "/repo": "Home", "/plain": "Plain"}
	seen := 0
	for _, r := range findFactsByKind(ff, facts.KindRoute) {
		handler, ok := want[r.Name]
		if !ok {
			continue
		}
		seen++
		if got := r.PropString("handler"); got != handler {
			t.Errorf("%s: handler = %q, want %q", r.Name, got, handler)
		}
		// The post-link binder turns this into the route's handled_by edge.
		if got := r.PropString(facts.PropHandlerTarget); got != "internal/server."+handler {
			t.Errorf("%s: handler target = %q, want internal/server.%s", r.Name, got, handler)
		}
	}
	if seen != len(want) {
		t.Errorf("saw %d of %d routes", seen, len(want))
	}
}
