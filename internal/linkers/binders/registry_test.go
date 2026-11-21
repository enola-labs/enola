package binders_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/binders"
	"github.com/enola-labs/enola/internal/linkers/binders/generatedclients"
	"github.com/enola-labs/enola/internal/linkers/binders/grpcclientfqn"
	"github.com/enola-labs/enola/internal/linkers/binders/grpcimpl"
	"github.com/enola-labs/enola/internal/linkers/binders/httphandler"
	"github.com/enola-labs/enola/internal/linkers/binders/openapiimpl"
	"github.com/enola-labs/enola/internal/linkers/binders/unmatchedroutes"
	"github.com/enola-labs/enola/internal/linkers/binders/vendoredspecs"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// fixture builds a store exercising all four binders at once, and deliberately seeds the
// collision that makes order-independence non-trivial: a gRPC service method and an
// HTTP handler that share the method name GetUser. If httphandler did not gate on
// route type, the result would depend on whether grpcimpl had already written the
// gRPC route's `handler` prop.
func fixture() *facts.Store {
	store := facts.NewStore()
	store.Add(
		// gRPC server route + its Go implementation.
		facts.Fact{
			Kind: facts.KindRoute, Name: "/users.v1.UserService/GetUser", Repo: "api",
			Props: map[string]any{
				facts.PropRouteType: facts.RouteTypeGRPC,
				facts.PropRole:      facts.RoleServer,
				"language":          "grpc",
				"rpc_service":       "users.v1.UserService",
				"rpc_method":        "GetUser",
			},
		},
		facts.Fact{
			Kind: facts.KindSymbol, Name: "users.UserService", Repo: "api",
			Props:     map[string]any{"symbol_kind": facts.SymbolStruct},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "usersv1.UnimplementedUserServiceServer"}},
		},
		facts.Fact{
			Kind: facts.KindSymbol, Name: "users.UserService.GetUser", Repo: "api",
			Props: map[string]any{"symbol_kind": facts.SymbolMethod},
		},
		// HTTP route + handler whose METHOD NAME collides with the gRPC one.
		facts.Fact{
			Kind: facts.KindRoute, Name: "/api/users/{id}", Repo: "api",
			Props: map[string]any{
				facts.PropRole: facts.RoleServer, "method": "GET", "language": "go",
				"handler": "h.userHandler.GetUser",
			},
		},
		facts.Fact{
			Kind: facts.KindSymbol, Name: "internal/adapters/http/users.Handler.GetUser", Repo: "api",
			Props: map[string]any{
				"symbol_kind": facts.SymbolMethod, "language": "go", httphandler.HandlerProp: true,
			},
		},
		// A client call site in a second repo, so the cross-repo passes engage.
		facts.Fact{
			Kind: facts.KindRoute, Name: "/api/users/{id}", Repo: "web",
			Props: map[string]any{facts.PropRole: facts.RoleClient, "method": "GET"},
		},
		facts.Fact{
			Kind: facts.KindRoute, Name: "/api/missing/{id}", Repo: "web",
			Props: map[string]any{facts.PropRole: facts.RoleClient, "method": "GET"},
		},
	)
	return store
}

func runAll(t *testing.T, order []binders.Binder) []byte {
	t.Helper()
	reg := binders.NewRegistry()
	for _, b := range order {
		reg.Register(b)
	}
	store := fixture()
	for _, stage := range []plugin.BindStage{plugin.StagePreLink, plugin.StagePostLink} {
		for _, b := range reg.Stage(stage) {
			if err := b.Bind(context.Background(), store); err != nil {
				t.Fatalf("%s.Bind: %v", b.Name(), err)
			}
		}
	}
	var buf bytes.Buffer
	if err := store.WriteJSONL(&buf); err != nil {
		t.Fatalf("WriteJSONL: %v", err)
	}
	return buf.Bytes()
}

// TestBinders_OutputIsIndependentOfRegistrationOrder is the load-bearing test for the
// Binder contract.
//
// Binders run as a plain loop over a registry, so if any two of them interacted, the
// facts written would depend on registration order — and facts.jsonl is hashed into
// the snapshot ID, so an unchanged tree would produce a different snapshot from run to
// run depending on how the engine happened to be wired. Asserting the serialized store
// is byte-identical across a reversed registration order is what makes "order within a
// stage carries no meaning" a checked property rather than a comment.
func TestBinders_OutputIsIndependentOfRegistrationOrder(t *testing.T) {
	forward := []binders.Binder{
		grpcclientfqn.New(), grpcimpl.New(), httphandler.New(), unmatchedroutes.New(vocab.Default()),
	}
	reverse := []binders.Binder{
		unmatchedroutes.New(vocab.Default()), httphandler.New(), grpcimpl.New(), grpcclientfqn.New(),
	}

	a := runAll(t, forward)
	b := runAll(t, reverse)

	if !bytes.Equal(a, b) {
		t.Errorf("binder output depends on registration order\n--- forward ---\n%s\n--- reverse ---\n%s", a, b)
	}
}

// TestBinders_RepeatedRunsAreStable pins idempotency at the registry level rather than
// per-binder: every binder re-runs on every snapshot and every append, so running the
// whole set twice must produce exactly what running it once did.
func TestBinders_RepeatedRunsAreStable(t *testing.T) {
	order := []binders.Binder{
		grpcclientfqn.New(), grpcimpl.New(), httphandler.New(), unmatchedroutes.New(vocab.Default()),
	}
	once := runAll(t, order)
	twice := runAll(t, append(order, order...))

	if !bytes.Equal(once, twice) {
		t.Errorf("a second binder pass changed the store\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

// TestRegistry_StagePartitions asserts the registry hands each binder to exactly one
// stage, and that of the four binders these tests run, grpc-client-fqn alone declares
// the earlier one. It rewrites route NAMES, and the cross-repo linker matches routes by
// name, so if it ever moved to post-link, gRPC client calls would be matched against
// their unresolved short paths and silently link to nothing.
//
// The four are the registry's original set, not the production one: pkg/bootstrap
// registers more binders, several of them pre-link, and none of the tests in this file
// runs them.
func TestRegistry_StagePartitions(t *testing.T) {
	reg := binders.NewRegistry()
	for _, b := range []binders.Binder{
		grpcclientfqn.New(), grpcimpl.New(), httphandler.New(), unmatchedroutes.New(vocab.Default()),
	} {
		reg.Register(b)
	}

	pre := reg.Stage(plugin.StagePreLink)
	post := reg.Stage(plugin.StagePostLink)
	if len(pre)+len(post) != len(reg.All()) {
		t.Errorf("stages partition %d+%d binders, registry holds %d", len(pre), len(post), len(reg.All()))
	}
	if len(pre) != 1 || pre[0].Name() != "grpc-client-fqn" {
		var names []string
		for _, b := range pre {
			names = append(names, b.Name())
		}
		t.Errorf("pre-link stage of the four = %v, want exactly [grpc-client-fqn]", names)
	}
}

// openAPIFixture exercises the three pre-link binders that settle OpenAPI roles, each
// reading what another writes: vendored-specs demotes the specs of a native app,
// openapi-impl binds a spec-first service's operations and demotes the spec it
// implements none of, and generated-clients finds the calls through clients generated
// from the specs the other two left as client specs.
func openAPIFixture() *facts.Store {
	spec := func(repo, file, path, id string) facts.Fact {
		return facts.Fact{
			Kind: facts.KindRoute, Name: path, File: file, Repo: repo,
			Props: map[string]any{
				facts.PropSource: facts.RouteSourceOpenAPI, facts.PropRole: facts.RoleServer,
				"method": "GET", "operationId": id, "spec_file": file,
			},
		}
	}
	method := func(repo, name, file string, rels ...facts.Relation) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Repo: repo, Line: 3,
			Props: map[string]any{"symbol_kind": facts.SymbolMethod}, Relations: rels}
	}
	dep := func(repo, file, target string) facts.Fact {
		return facts.Fact{Kind: facts.KindDependency, Name: "mod -> " + target, File: file, Repo: repo}
	}
	calls := func(target string) facts.Relation { return facts.Relation{Kind: facts.RelCalls, Target: target} }

	store := facts.NewStore()
	store.Add(
		// A spec-first service: one spec it implements, one it calls.
		spec("svc", "api/openapi/service.yml", "/items", "items"),
		spec("svc", "api/openapi/service.yml", "/healthz", "isLive"),
		spec("svc", "api/openapi/feature_store.yml", "/v1/features", "getFeatures"),
		spec("svc", "api/openapi/feature_store.yml", "/healthz", "isReady"),
		method("svc", "server.S.Items", "server/s.go"),
		method("svc", "server.S.IsLive", "server/s.go"),
		facts.Fact{Kind: facts.KindRoute, Name: "/metrics", Repo: "svc", Props: map[string]any{"method": "GET"}},
		dep("svc", "client/fs.go", "featurestore.api.FeatureStoreApi"),
		method("svc", "client.FS.Load", "client/fs.go", calls("getFeatures")),
		// A native app vendoring the spec of a service it calls.
		facts.Fact{Kind: facts.KindRoute, Name: "/sessions", Repo: "app",
			Props: map[string]any{facts.PropSource: facts.RouteSourceRetrofit, facts.PropRole: facts.RoleClient, "method": "GET"}},
		spec("app", "openapi/orders.yml", "/orders/{id}", "getOrder"),
		dep("app", "feature/Repo.kt", "com.app.orders.apis.OrdersApi"),
		method("app", "feature.Repo.load", "feature/Repo.kt", calls("getOrder")),
	)
	return store
}

func runOn(t *testing.T, store *facts.Store, order []binders.Binder) []byte {
	t.Helper()
	reg := binders.NewRegistry()
	for _, b := range order {
		reg.Register(b)
	}
	for _, b := range reg.Stage(plugin.StagePreLink) {
		if err := b.Bind(context.Background(), store); err != nil {
			t.Fatalf("%s.Bind: %v", b.Name(), err)
		}
	}
	var buf bytes.Buffer
	if err := store.WriteJSONL(&buf); err != nil {
		t.Fatalf("WriteJSONL: %v", err)
	}
	return buf.Bytes()
}

// TestBinders_OpenAPIChainIsIndependentOfRegistrationOrder registers the three
// OpenAPI binders in every order and asserts one output. They read each other's
// roles, so only the order they declare (RunsAfter) can make this hold.
func TestBinders_OpenAPIChainIsIndependentOfRegistrationOrder(t *testing.T) {
	mk := []func() binders.Binder{
		func() binders.Binder { return vendoredspecs.New() },
		func() binders.Binder { return openapiimpl.New() },
		func() binders.Binder { return generatedclients.New() },
	}
	perms := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	var want []byte
	for _, p := range perms {
		order := []binders.Binder{mk[p[0]](), mk[p[1]](), mk[p[2]]()}
		got := runOn(t, openAPIFixture(), order)
		if want == nil {
			want = got
			// The fixture must exercise every link of the chain, or agreeing proves nothing.
			for _, needle := range []string{`"vendored_spec":true`, `"handler_binding":"operation"`, `"source":"generated-client"`} {
				if !bytes.Contains(got, []byte(needle)) {
					t.Fatalf("fixture does not exercise %s:\n%s", needle, got)
				}
			}
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("order %v: output differs from order %v\n--- want ---\n%s\n--- got ---\n%s", p, perms[0], want, got)
		}
	}
	// Repeated: a second pass over the settled store changes nothing.
	store := openAPIFixture()
	once := runOn(t, store, []binders.Binder{generatedclients.New(), openapiimpl.New(), vendoredspecs.New()})
	if again := runOn(t, store, []binders.Binder{vendoredspecs.New(), generatedclients.New(), openapiimpl.New()}); !bytes.Equal(once, again) {
		t.Errorf("a second pass changed the store\n--- once ---\n%s\n--- again ---\n%s", once, again)
	}
}

type fakeBinder struct {
	name  string
	stage plugin.BindStage
	after []string
}

func (f fakeBinder) Name() string                             { return f.name }
func (f fakeBinder) Stage() plugin.BindStage                  { return f.stage }
func (f fakeBinder) Bind(context.Context, *facts.Store) error { return nil }
func (f fakeBinder) RunsAfter() []string                      { return f.after }

// TestRegistry_StageRunsDeclaredDependenciesFirst pins the ordering rule: a binder
// runs after the stage-mates it names, everything else keeps registration order, a
// name outside the stage is ignored, and a cycle still returns every binder once.
func TestRegistry_StageRunsDeclaredDependenciesFirst(t *testing.T) {
	names := func(bs []binders.Binder) string {
		var out []string
		for _, b := range bs {
			out = append(out, b.Name())
		}
		return strings.Join(out, ",")
	}
	cases := []struct {
		reg  []fakeBinder
		want string
	}{
		{[]fakeBinder{{"a", plugin.StagePreLink, nil}, {"b", plugin.StagePreLink, nil}, {"c", plugin.StagePreLink, nil}}, "a,b,c"},
		{[]fakeBinder{{"c", plugin.StagePreLink, []string{"b"}}, {"b", plugin.StagePreLink, []string{"a"}}, {"x", plugin.StagePreLink, nil}, {"a", plugin.StagePreLink, nil}}, "x,a,b,c"},
		{[]fakeBinder{{"b", plugin.StagePreLink, []string{"post", "missing"}}, {"a", plugin.StagePreLink, nil}, {"post", plugin.StagePostLink, nil}}, "b,a"},
		{[]fakeBinder{{"a", plugin.StagePreLink, []string{"b"}}, {"b", plugin.StagePreLink, []string{"a"}}, {"c", plugin.StagePreLink, nil}}, "c,a,b"},
	}
	for i, c := range cases {
		reg := binders.NewRegistry()
		for _, b := range c.reg {
			reg.Register(b)
		}
		if got := names(reg.Stage(plugin.StagePreLink)); got != c.want {
			t.Errorf("case %d: Stage = %s, want %s", i, got, c.want)
		}
	}
}
