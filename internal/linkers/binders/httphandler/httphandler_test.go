package httphandler

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func bind(t *testing.T, store *facts.Store) {
	t.Helper()
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatalf("Bind: %v", err)
	}
}

func routeHandledBy(f facts.Fact) (string, bool) {
	for _, r := range f.Relations {
		if r.Kind == facts.RelHandledBy {
			return r.Target, true
		}
	}
	return "", false
}

// httpRoute builds an HTTP route fact like goextractor's routes.go emits: the `handler`
// prop is rendered from the REGISTRATION site by exprToString, so it is the receiver
// VARIABLE chain, not the symbol's name.
func httpRoute(path, handler string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindRoute,
		Name: path,
		Props: map[string]any{
			"method": "GET", facts.PropFramework: "gorilla/mux", "language": "go",
			"handler": handler,
		},
	}
}

func goHandler(name string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindSymbol, Name: name,
		Props: map[string]any{
			"symbol_kind": facts.SymbolMethod, "language": "go", HandlerProp: true,
		},
	}
}

func goNonHandler(name string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindSymbol, Name: name,
		Props: map[string]any{"symbol_kind": facts.SymbolMethod, "language": "go"},
	}
}

// TestBindHTTPHandlers_BindsViaSignature is new/18. A route's handler prop names the
// receiver VARIABLE ("h.weatherHandler.GetDailyWeatherRange"); the symbol is named by
// its receiver TYPE. The two key spaces are disjoint — on fairwayhub/golf, 1397 handler
// props intersect 13482 symbol names exactly twice, and 0 of 1641 routes carried a
// handled_by edge — so the escalation that keys off them never fired.
func TestBindHTTPHandlers_BindsViaSignature(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		httpRoute("/api/weather/daily-range", "h.weatherHandler.GetDailyWeatherRange"),
		goHandler("internal/adapters/http/weather.HandlerV2.GetDailyWeatherRange"),
	)

	bind(t, store)

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/api/weather/daily-range"})
	target, ok := routeHandledBy(routes[0])
	if !ok {
		t.Fatal("route was not bound to its handler")
	}
	if want := "internal/adapters/http/weather.HandlerV2.GetDailyWeatherRange"; target != want {
		t.Errorf("bound to %q, want %q", target, want)
	}
}

// TestBindHTTPHandlers_RejectsNonHandlerBySignature is the mis-bind that killed the
// naive rule, pinned. On fairwayhub/golf, /api/weather/daily-range's handler shares its
// method name with a stub in the WIRING package (internal/bootstrap), and a
// package-scoped or name-only binder resolves the route to that stub:
//
//	binds to : internal/bootstrap.NullWeatherService.GetDailyWeatherRange   ← WRONG
//	truth    : internal/adapters/http/weather.HandlerV2.GetDailyWeatherRange
//
// A wrong handled_by edge feeds impact_analysis and find_path, so it is worse than the
// bug it fixes. The http_handler prop rejects the stub STRUCTURALLY — its signature is
// (ctx, string), not (http.ResponseWriter, *http.Request) — rather than by guessing.
func TestBindHTTPHandlers_RejectsNonHandlerBySignature(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		httpRoute("/api/weather/daily-range", "h.weatherHandler.GetDailyWeatherRange"),
		goHandler("internal/adapters/http/weather.HandlerV2.GetDailyWeatherRange"),
		// Same method name, service signature — must never win.
		goNonHandler("internal/bootstrap.NullWeatherService.GetDailyWeatherRange"),
		goNonHandler("internal/app/weather.Service.GetDailyWeatherRange"),
		goNonHandler("internal/mocks/services.MockWeatherService.GetDailyWeatherRange"),
	)

	bind(t, store)

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/api/weather/daily-range"})
	target, _ := routeHandledBy(routes[0])
	if want := "internal/adapters/http/weather.HandlerV2.GetDailyWeatherRange"; target != want {
		t.Errorf("bound to %q, want %q — the non-handler candidates must be rejected by signature", target, want)
	}
}

// TestBindHTTPHandlers_AmbiguousMethodNameSkipped mirrors grpcimpl's ambiguity guard.
// Two genuine handlers sharing a method name cannot be told apart from the registration
// site alone, so the route is left unbound. Skipping is fail-safe; guessing is not.
// On fairwayhub/golf this covers 212 of 1639 routes.
func TestBindHTTPHandlers_AmbiguousMethodNameSkipped(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		httpRoute("/api/things", "h.thingHandler.List"),
		goHandler("internal/adapters/http/things.HandlerV2.List"),
		goHandler("internal/adapters/http/users.HandlerV2.List"),
	)

	bind(t, store)

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/api/things"})
	if target, ok := routeHandledBy(routes[0]); ok {
		t.Errorf("ambiguous method name bound anyway, to %q — two handlers share it", target)
	}
}

// TestBindHTTPHandlers_MiddlewareNotBound: a USE route's handler is middleware, not a
// (w, r) handler. It carries no http_handler prop, so it cannot bind — but assert it,
// because binding middleware to a route would pollute the same edges.
func TestBindHTTPHandlers_MiddlewareNotBound(t *testing.T) {
	store := facts.NewStore()
	mw := httpRoute("/api", "h.auth.RequireAuth")
	mw.Props["method"] = "USE"
	mw.Props[facts.PropRouteType] = facts.RouteTypeMiddleware
	store.Add(mw, goHandler("internal/adapters/http/middleware.Auth.RequireAuth"))

	bind(t, store)

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/api"})
	if target, ok := routeHandledBy(routes[0]); ok {
		t.Errorf("middleware route was bound to %q", target)
	}
}

// TestBindHTTPHandlers_Idempotent: the pass reruns on every snapshot and append, so it
// must not accumulate duplicate edges.
func TestBindHTTPHandlers_Idempotent(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		httpRoute("/api/things", "h.thingHandler.List"),
		goHandler("internal/adapters/http/things.HandlerV2.List"),
	)

	bind(t, store)
	bind(t, store)

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/api/things"})
	n := 0
	for _, r := range routes[0].Relations {
		if r.Kind == facts.RelHandledBy {
			n++
		}
	}
	if n != 1 {
		t.Errorf("handled_by edges = %d after two passes, want 1", n)
	}
}

// TestBindHTTPHandlers_CrossRepoNotBound: routes never bind across repos, exactly as
// grpcimpl's per-repo index guarantees.
func TestBindHTTPHandlers_CrossRepoNotBound(t *testing.T) {
	store := facts.NewStore()
	route := httpRoute("/api/things", "h.thingHandler.List")
	route.Repo = "api"
	handler := goHandler("internal/adapters/http/things.HandlerV2.List")
	handler.Repo = "other"
	store.Add(route, handler)

	bind(t, store)

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/api/things"})
	if target, ok := routeHandledBy(routes[0]); ok {
		t.Errorf("route bound to a handler in a different repo: %q", target)
	}
}

// TestBindHTTPHandlers_GRPCRouteNotBound guards the one hazard created by dropping the
// old `language == "go"` gate.
//
// That gate excluded gRPC routes only by accident — they carry language="grpc". But by
// the time both binders have run, grpcimpl has written a `handler` prop holding a fully
// qualified method name, whose last segment can collide with an HTTP handler's method
// name. Without an explicit route-type gate the result would depend on which binder ran
// first, which the Binder contract forbids.
//
// The fixture is the collision itself: a gRPC route already bound to
// "users.UserService.GetUser", beside an unrelated HTTP handler whose method is also
// GetUser. Only the gRPC edge may survive.
func TestBindHTTPHandlers_GRPCRouteNotBound(t *testing.T) {
	store := facts.NewStore()
	grpcRoute := facts.Fact{
		Kind: facts.KindRoute,
		Name: "/users.v1.UserService/GetUser",
		Props: map[string]any{
			facts.PropRouteType: facts.RouteTypeGRPC,
			facts.PropRole:      facts.RoleServer,
			"language":          "grpc",
			"handler":           "users.UserService.GetUser", // written by grpcimpl
		},
		Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "users.UserService.GetUser"}},
	}
	store.Add(grpcRoute, goHandler("internal/adapters/http/users.HandlerV2.GetUser"))

	bind(t, store)

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/users.v1.UserService/GetUser"})
	var targets []string
	for _, r := range routes[0].Relations {
		if r.Kind == facts.RelHandledBy {
			targets = append(targets, r.Target)
		}
	}
	if len(targets) != 1 || targets[0] != "users.UserService.GetUser" {
		t.Errorf("handled_by = %v, want exactly [users.UserService.GetUser] — a gRPC route is grpcimpl's to bind", targets)
	}
}

// TestBindHTTPHandlers_NoLanguageGate pins the generalization: binding is keyed on the
// structural HandlerProp, never on a language name. A route and handler in a language
// the engine has no special knowledge of bind exactly as Go's do, which is what lets a
// new extractor opt in without this file changing.
func TestBindHTTPHandlers_NoLanguageGate(t *testing.T) {
	store := facts.NewStore()
	route := httpRoute("/api/things", "ctrl.thingsController.list")
	route.Props["language"] = "typescript"
	handler := facts.Fact{
		Kind: facts.KindSymbol, Name: "src/things/controller.ThingsController.list",
		Props: map[string]any{
			"symbol_kind": facts.SymbolMethod, "language": "typescript", HandlerProp: true,
		},
	}
	store.Add(route, handler)

	bind(t, store)

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/api/things"})
	target, ok := routeHandledBy(routes[0])
	if !ok || target != "src/things/controller.ThingsController.list" {
		t.Errorf("handled_by = %q (ok=%v), want the TypeScript handler — binding must not be gated on language", target, ok)
	}
}

// A route whose handler names its symbol outright binds without a handler
// signature: a package function by PropHandlerTarget, and a routes-file handler
// `<type fqn>.<method>` through the type's fqn. A name no symbol carries, and a
// type fqn two types claim, bind nothing.
func TestBind_ExactlyNamedHandlers(t *testing.T) {
	route := func(name string, props map[string]any) facts.Fact {
		p := map[string]any{"method": "GET"}
		for k, v := range props {
			p[k] = v
		}
		return facts.Fact{Kind: facts.KindRoute, Name: name, Repo: "r", Props: p}
	}
	sym := func(name, kind string, props map[string]any) facts.Fact {
		p := map[string]any{"symbol_kind": kind}
		for k, v := range props {
			p[k] = v
		}
		return facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "r", Props: p}
	}
	store := facts.NewStore()
	store.Add(
		route("/diff", map[string]any{"handler": "repo.DownloadPullDiff", facts.PropHandlerTarget: "routers/web/repo.DownloadPullDiff"}),
		route("/missing", map[string]any{"handler": "repo.Gone", facts.PropHandlerTarget: "routers/web/repo.Gone"}),
		route("/", map[string]any{"handler": "controllers.Lobby.home", "framework": "play"}),
		route("/dup", map[string]any{"handler": "controllers.Dup.show", "framework": "play"}),
		sym("routers/web/repo.DownloadPullDiff", facts.SymbolFunc, nil),
		sym("app/controllers.Lobby", "class", map[string]any{"fqn": "controllers.Lobby"}),
		sym("app/controllers.Lobby.home", facts.SymbolMethod, nil),
		sym("a/controllers.Dup", "class", map[string]any{"fqn": "controllers.Dup"}),
		sym("b/controllers.Dup", "class", map[string]any{"fqn": "controllers.Dup"}),
		sym("a/controllers.Dup.show", facts.SymbolMethod, nil),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"/diff": "routers/web/repo.DownloadPullDiff", "/": "app/controllers.Lobby.home", "/missing": "", "/dup": ""}
	for _, f := range store.ByKind(facts.KindRoute) {
		got := ""
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				got = r.Target
			}
		}
		if got != want[f.Name] {
			t.Errorf("%s handled_by = %q, want %q", f.Name, got, want[f.Name])
		}
	}
}
