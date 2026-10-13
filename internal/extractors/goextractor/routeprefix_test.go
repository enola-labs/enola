package goextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// routeNames returns the set of route Names (paths) with the given method.
func routeNames(ff []facts.Fact, method string) map[string]bool {
	set := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Props["method"] == method {
			set[f.Name] = true
		}
	}
	return set
}

func hasRoute(ff []facts.Fact, path, method string) bool {
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Name == path && f.Props["method"] == method {
			return true
		}
	}
	return false
}

// A subrouter created in one function and passed to a registration function in
// the SAME package must carry its prefix into the callee's bare-path routes.
func TestExtractRoutes_Interprocedural_SamePackage(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import "github.com/gorilla/mux"

func Setup() {
	router := mux.NewRouter()
	apiRouter := router.PathPrefix("/api").Subrouter()
	RegisterCourseRoutes(apiRouter)
	_ = router
}

func RegisterCourseRoutes(r *mux.Router) {
	r.HandleFunc("/courses", listCourses).Methods("GET")
	r.HandleFunc("/courses/{id}", getCourse).Methods("GET")
}

func listCourses() {}
func getCourse()   {}
`,
	})

	if !hasRoute(ff, "/api/courses", "GET") {
		t.Errorf("want /api/courses GET; routes = %v", routeNames(ff, "GET"))
	}
	if !hasRoute(ff, "/api/courses/{id}", "GET") {
		t.Errorf("want /api/courses/{id} GET; routes = %v", routeNames(ff, "GET"))
	}
	// The bare (un-prefixed) forms must NOT appear.
	if hasRoute(ff, "/courses", "GET") {
		t.Errorf("bare /courses GET should have been prefixed away")
	}
}

// Two hops across packages, with the callee itself creating a nested subrouter:
// server → (mounts /api/settings) → courses.Register (mounts /courses under it).
func TestExtractRoutes_Interprocedural_CrossPackage(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/server.go": `package server

import (
	"github.com/gorilla/mux"
	"testmod/internal/courses"
)

func Setup() {
	router := mux.NewRouter()
	apiRouter := router.PathPrefix("/api").Subrouter()
	settingsRouter := apiRouter.PathPrefix("/settings").Subrouter()
	courses.Register(settingsRouter)
	_ = router
}
`,
		"internal/courses/courses.go": `package courses

import "github.com/gorilla/mux"

func Register(r *mux.Router) {
	rulesRouter := r.PathPrefix("/courses").Subrouter()
	rulesRouter.HandleFunc("/{id}/rules", listRules).Methods("GET")
}

func listRules() {}
`,
	})

	if !hasRoute(ff, "/api/settings/courses/{id}/rules", "GET") {
		t.Errorf("want /api/settings/courses/{id}/rules GET; routes = %v", routeNames(ff, "GET"))
	}
}

// A registration function mounted at two different prefixes must emit its routes
// once per mount point.
func TestExtractRoutes_Interprocedural_MultipleMountPoints(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import "github.com/gorilla/mux"

func Setup() {
	router := mux.NewRouter()
	apiRouter := router.PathPrefix("/api").Subrouter()
	adminRouter := router.PathPrefix("/admin").Subrouter()
	register(apiRouter)
	register(adminRouter)
	_ = router
}

func register(r *mux.Router) {
	r.HandleFunc("/things", listThings).Methods("GET")
}

func listThings() {}
`,
	})

	if !hasRoute(ff, "/api/things", "GET") {
		t.Errorf("want /api/things GET; routes = %v", routeNames(ff, "GET"))
	}
	if !hasRoute(ff, "/admin/things", "GET") {
		t.Errorf("want /admin/things GET; routes = %v", routeNames(ff, "GET"))
	}
}

// The chi router interface variant must compose across functions too.
func TestExtractRoutes_Interprocedural_Chi(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import "github.com/go-chi/chi/v5"

func Setup() {
	router := chi.NewRouter()
	router.Route("/api", func(r chi.Router) {})
	registerChi(router)
	_ = router
}

func registerChi(r chi.Router) {
	r.Get("/widgets", listWidgets)
}

func listWidgets() {}
`,
	})

	// registerChi is called with the root router (prefix ""), so /widgets is bare —
	// but the point is the cross-function seed path works for a chi.Router param
	// without panicking and the route is still emitted.
	if !hasRoute(ff, "/widgets", "GET") {
		t.Errorf("want /widgets GET; routes = %v", routeNames(ff, "GET"))
	}
}

// A registration function never reached from any root keeps today's bare-path
// behavior (graceful degradation) rather than being dropped.
func TestExtractRoutes_Interprocedural_Unreached_KeepsBarePath(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import "github.com/gorilla/mux"

// Never called from a NewRouter root.
func RegisterOrphan(r *mux.Router) {
	r.HandleFunc("/orphan", h).Methods("GET")
}

func h() {}
`,
	})

	if !hasRoute(ff, "/orphan", "GET") {
		t.Errorf("want bare /orphan GET (graceful degradation); routes = %v", routeNames(ff, "GET"))
	}
}

// The dominant real-world pattern: routes registered by a METHOD on a handler
// struct (`func (h *Handler) RegisterRoutes(r *mux.Router)`), called via a local
// variable receiver. Resolving it requires the receiver's type, which the shared
// resolveChain resolver recovers from the constructor convention.
func TestExtractRoutes_Interprocedural_MethodLocalReceiver(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/courses/handler.go": `package courses

import "github.com/gorilla/mux"

type Handler struct{}

func NewHandler() *Handler { return &Handler{} }

func (h *Handler) RegisterRoutes(r *mux.Router) {
	r.HandleFunc("/courses/{id}", h.get).Methods("GET")
}

func (h *Handler) get() {}
`,
		"internal/server/server.go": `package server

import (
	"github.com/gorilla/mux"
	"testmod/internal/courses"
)

func Setup() {
	router := mux.NewRouter()
	api := router.PathPrefix("/api/settings").Subrouter()
	h := courses.NewHandler()
	h.RegisterRoutes(api)
	_ = router
}
`,
	})

	if !hasRoute(ff, "/api/settings/courses/{id}", "GET") {
		t.Errorf("want /api/settings/courses/{id} GET (method via local receiver); routes = %v", routeNames(ff, "GET"))
	}
}

// The golf pattern exactly: a registration method reached through a FIELD of the
// caller's receiver (`h.sub.RegisterRoutes(subrouter)`), resolved via fieldTypes.
func TestExtractRoutes_Interprocedural_MethodFieldReceiver(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/courses/handler.go": `package courses

import "github.com/gorilla/mux"

type Handler struct{}

func (h *Handler) RegisterRoutes(r *mux.Router) {
	r.HandleFunc("/rules/{id}", h.get).Methods("GET")
}

func (h *Handler) get() {}
`,
		"internal/server/server.go": `package server

import (
	"github.com/gorilla/mux"
	"testmod/internal/courses"
)

type App struct {
	coursesHandler *courses.Handler
}

func (a *App) Setup() {
	router := mux.NewRouter()
	api := router.PathPrefix("/api/settings").Subrouter()
	a.coursesHandler.RegisterRoutes(api)
	_ = router
}
`,
	})

	if !hasRoute(ff, "/api/settings/rules/{id}", "GET") {
		t.Errorf("want /api/settings/rules/{id} GET (method via field receiver); routes = %v", routeNames(ff, "GET"))
	}
}

// Mutually-recursive registration functions must terminate (the fixpoint's
// visited-set guards the cycle) and still compose the reachable prefix.
func TestExtractRoutes_Interprocedural_Cycle(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import "github.com/gorilla/mux"

func Setup() {
	router := mux.NewRouter()
	apiRouter := router.PathPrefix("/api").Subrouter()
	a(apiRouter)
	_ = router
}

func a(r *mux.Router) {
	r.HandleFunc("/a", h).Methods("GET")
	b(r)
}

func b(r *mux.Router) {
	r.HandleFunc("/b", h).Methods("GET")
	a(r)
}

func h() {}
`,
	})

	if !hasRoute(ff, "/api/a", "GET") || !hasRoute(ff, "/api/b", "GET") {
		t.Errorf("want /api/a and /api/b GET; routes = %v", routeNames(ff, "GET"))
	}
}

// Extraction must be deterministic: the multiset of route names is identical
// across runs (guards against map-iteration nondeterminism feeding the goldens).
func TestExtractRoutes_Interprocedural_Deterministic(t *testing.T) {
	files := map[string]string{
		"internal/server/routes.go": `package server

import "github.com/gorilla/mux"

func Setup() {
	router := mux.NewRouter()
	apiRouter := router.PathPrefix("/api").Subrouter()
	adminRouter := router.PathPrefix("/admin").Subrouter()
	register(apiRouter)
	register(adminRouter)
	_ = router
}

func register(r *mux.Router) {
	r.HandleFunc("/things", h).Methods("GET")
	r.HandleFunc("/things/{id}", h).Methods("GET")
}

func h() {}
`,
	}
	first := routeNames(extractAll(t, files), "GET")
	for i := 0; i < 5; i++ {
		got := routeNames(extractAll(t, files), "GET")
		if len(got) != len(first) {
			t.Fatalf("run %d: route count %d != %d", i, len(got), len(first))
		}
		for name := range first {
			if !got[name] {
				t.Fatalf("run %d: missing route %q", i, name)
			}
		}
	}
	// Sanity: both mount points present.
	for _, want := range []string{"/api/things", "/admin/things", "/api/things/{id}", "/admin/things/{id}"} {
		if !first[want] {
			t.Errorf("want %s in %v", want, first)
		}
	}
}

// chi mounts a subrouter with Route and hands it to a function literal. The
// routes inside it are served under the mount, however deep it nests.
func TestExtractRoutes_FuncLitMount_ChiRoute(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import "github.com/go-chi/chi/v5"

func Setup() {
	r := chi.NewRouter()
	r.Get("/health", health)
	r.Route("/api", func(r chi.Router) {
		r.Get("/users", listUsers)
		r.Route("/admin", func(sub chi.Router) {
			sub.Post("/keys", createKey)
		})
		r.Group(func(guarded chi.Router) {
			guarded.Delete("/users/{id}", deleteUser)
		})
	})
	r.Get("/after", after)
}

func health()     {}
func listUsers()  {}
func createKey()  {}
func deleteUser() {}
func after()      {}
`,
	})
	for _, want := range [][2]string{
		{"/health", "GET"}, {"/api/users", "GET"}, {"/api/admin/keys", "POST"},
		{"/api/users/{id}", "DELETE"}, {"/after", "GET"},
	} {
		if !hasRoute(ff, want[0], want[1]) {
			t.Errorf("want %s %s; GET %v POST %v DELETE %v", want[1], want[0],
				routeNames(ff, "GET"), routeNames(ff, "POST"), routeNames(ff, "DELETE"))
		}
	}
	// The mount ends with its literal: a route after it is not under /api.
	if hasRoute(ff, "/api/after", "GET") {
		t.Errorf("the /api prefix leaked past the end of its Route")
	}
}

// A router whose Group takes a path and a literal with no parameter registers
// on the receiver itself, so the receiver carries the prefix inside the literal.
func TestExtractRoutes_FuncLitMount_GroupOnTheReceiver(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import "github.com/go-chi/chi/v5"

type router struct{ chi.Router }

func (r *router) Group(pattern string, fn func(), mw ...any) {}
func (r *router) Post(pattern string, h ...any)             {}
func (r *router) Get(pattern string, h ...any)              {}

func Setup(m *router) {
	m.Group("/user", func() {
		m.Post("/login", bind(), signIn)
		m.Group("/settings", func() {
			m.Get("", settings)
		}, reqSignIn)
		m.Group("", func() {
			m.Get("/logout", signOut)
		})
	}, reqSignOut)
	m.Get("/explore", explore)
}

func bind() any   { return nil }
func reqSignIn()  {}
func reqSignOut() {}
func signIn()     {}
func signOut()    {}
func settings()   {}
func explore()    {}
`,
	})
	for _, want := range [][2]string{
		{"/user/login", "POST"}, {"/user/settings", "GET"}, {"/user/logout", "GET"}, {"/explore", "GET"},
	} {
		if !hasRoute(ff, want[0], want[1]) {
			t.Errorf("want %s %s; GET %v POST %v", want[1], want[0], routeNames(ff, "GET"), routeNames(ff, "POST"))
		}
	}
}

// A closure that registers routes registers them where it is called. Called
// under two mounts, its routes exist under both; never called, under neither.
func TestExtractRoutes_FuncLitMount_LocalClosureCalledUnderEachMount(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import "github.com/go-chi/chi/v5"

type router struct{ chi.Router }

func (r *router) Group(pattern string, fn func(), mw ...any) {}
func (r *router) Get(pattern string, h ...any)              {}

func Setup(m *router) {
	addSecrets := func() {
		m.Get("/secrets", listSecrets)
	}
	neverCalled := func() {
		m.Get("/ghost", listSecrets)
	}
	m.Group("/org", func() {
		addSecrets()
	})
	m.Group("/repo", func() {
		addSecrets()
	})
	_ = neverCalled
}

func listSecrets() {}
`,
	})
	got := routeNames(ff, "GET")
	if !hasRoute(ff, "/org/secrets", "GET") || !hasRoute(ff, "/repo/secrets", "GET") {
		t.Errorf("want /org/secrets and /repo/secrets; got %v", got)
	}
	for _, name := range []string{"/secrets", "/ghost", "/org/ghost", "/repo/ghost"} {
		if got[name] {
			t.Errorf("%s was registered by a closure where it was defined, not where it was called; got %v", name, got)
		}
	}
}

// A registration function called inside a mount's literal receives the mounted
// router, so its routes are under the mount too.
func TestExtractRoutes_FuncLitMount_ReachesACalledFunction(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import "github.com/go-chi/chi/v5"

func Setup() {
	router := chi.NewRouter()
	router.Route("/api", func(r chi.Router) {
		registerWidgets(r)
	})
}

func registerWidgets(r chi.Router) {
	r.Get("/widgets", listWidgets)
}

func listWidgets() {}
`,
	})
	if !hasRoute(ff, "/api/widgets", "GET") {
		t.Errorf("want /api/widgets GET; routes = %v", routeNames(ff, "GET"))
	}
}

// A Route whose path is not a literal mounts under something that cannot be
// read. Its routes keep their own path rather than gaining a guessed one.
func TestExtractRoutes_FuncLitMount_DynamicPathIsNotGuessed(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/routes.go": `package server

import "github.com/go-chi/chi/v5"

func Setup(base string) {
	r := chi.NewRouter()
	r.Route(base, func(r chi.Router) {
		r.Get("/users", listUsers)
	})
}

func listUsers() {}
`,
	})
	if !hasRoute(ff, "/users", "GET") {
		t.Errorf("want the bare /users GET; routes = %v", routeNames(ff, "GET"))
	}
}
