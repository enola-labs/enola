package binders_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/binders"
	"github.com/enola-labs/enola/internal/linkers/binders/grpcclientfqn"
	"github.com/enola-labs/enola/internal/linkers/binders/grpcimpl"
	"github.com/enola-labs/enola/internal/linkers/binders/httphandler"
	"github.com/enola-labs/enola/internal/linkers/binders/unmatchedroutes"
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
