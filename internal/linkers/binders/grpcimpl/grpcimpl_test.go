package grpcimpl

import (
	"context"
	"regexp"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// bind runs the binder over the store, failing the test on error.
func bind(t *testing.T, store *facts.Store) {
	t.Helper()
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatalf("Bind: %v", err)
	}
}

// grpcServerRoute builds a server-role gRPC route fact like the grpc extractor emits.
func grpcServerRoute(service, method string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindRoute,
		Name: "/" + service + "/" + method,
		Props: map[string]any{
			facts.PropRouteType: facts.RouteTypeGRPC,
			facts.PropRole:      facts.RoleServer,
			facts.PropFramework: facts.FrameworkGRPC,
			"rpc_service":       service,
			"rpc_method":        method,
		},
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

func TestBindGRPCHandlers_BindsViaEmbedConvention(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		grpcServerRoute("users.v1.UserService", "GetUser"),
		grpcServerRoute("users.v1.UserService", "CreateUser"),
		facts.Fact{
			Kind: facts.KindSymbol, Name: "users.UserService",
			Props: map[string]any{"symbol_kind": facts.SymbolStruct},
			Relations: []facts.Relation{
				{Kind: facts.RelImplements, Target: "usersv1.UnimplementedUserServiceServer"},
			},
		},
		facts.Fact{Kind: facts.KindSymbol, Name: "users.UserService.GetUser", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
		facts.Fact{Kind: facts.KindSymbol, Name: "users.UserService.CreateUser", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
	)

	bind(t, store)

	for _, method := range []string{"GetUser", "CreateUser"} {
		routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/users.v1.UserService/" + method})
		if len(routes) != 1 {
			t.Fatalf("route lookup for %s = %d, want 1", method, len(routes))
		}
		target, ok := routeHandledBy(routes[0])
		if !ok {
			t.Fatalf("%s route has no handled_by relation", method)
		}
		want := "users.UserService." + method
		if target != want {
			t.Errorf("handled_by target = %q, want %q", target, want)
		}
		if routes[0].Props["handler"] != want {
			t.Errorf("handler prop = %v, want %q", routes[0].Props["handler"], want)
		}
	}
}

// TestBindGRPCHandlers_BindsPythonServicer: a Python servicer class subclassing a
// generated <Service>Servicer base binds the proto route to its handler method,
// exactly as the Go embed convention does.
func TestBindGRPCHandlers_BindsPythonServicer(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		grpcServerRoute("vosk.stt.v1.SttService", "StreamingRecognize"),
		facts.Fact{
			Kind: facts.KindSymbol, Name: "grpc/stt_server.SttServiceServicer",
			Props: map[string]any{"symbol_kind": facts.SymbolClass},
			Relations: []facts.Relation{
				{Kind: facts.RelImplements, Target: "stt_service_pb2_grpc.SttServiceServicer"},
			},
		},
		facts.Fact{
			Kind: facts.KindSymbol, Name: "grpc/stt_server.SttServiceServicer.StreamingRecognize",
			Props: map[string]any{"symbol_kind": facts.SymbolMethod},
		},
	)

	bind(t, store)

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/vosk.stt.v1.SttService/StreamingRecognize"})
	if len(routes) != 1 {
		t.Fatalf("route lookup = %d, want 1", len(routes))
	}
	target, ok := routeHandledBy(routes[0])
	want := "grpc/stt_server.SttServiceServicer.StreamingRecognize"
	if !ok || target != want {
		t.Errorf("handled_by = %q (ok=%v), want %q", target, ok, want)
	}
	if routes[0].Props["handler"] != want {
		t.Errorf("handler prop = %v, want %q", routes[0].Props["handler"], want)
	}
}

func TestBindGRPCHandlers_NoEmbedNoBind(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		grpcServerRoute("users.v1.UserService", "GetUser"),
		// Struct implements some unrelated interface, not Unimplemented*Server.
		facts.Fact{
			Kind: facts.KindSymbol, Name: "users.UserService",
			Props:     map[string]any{"symbol_kind": facts.SymbolStruct},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "io.Closer"}},
		},
		facts.Fact{Kind: facts.KindSymbol, Name: "users.UserService.GetUser", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
	)

	bind(t, store)

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/users.v1.UserService/GetUser"})
	if _, ok := routeHandledBy(routes[0]); ok {
		t.Error("route was bound despite no Unimplemented*Server embed")
	}
}

func TestBindGRPCHandlers_MissingMethodNotBound(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		grpcServerRoute("users.v1.UserService", "GetUser"),
		facts.Fact{
			Kind: facts.KindSymbol, Name: "users.UserService",
			Props:     map[string]any{"symbol_kind": facts.SymbolStruct},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "usersv1.UnimplementedUserServiceServer"}},
		},
		// No users.UserService.GetUser method symbol present.
	)

	bind(t, store)

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/users.v1.UserService/GetUser"})
	if _, ok := routeHandledBy(routes[0]); ok {
		t.Error("route bound to a nonexistent method")
	}
}

func TestBindGRPCHandlers_AmbiguousShortNameSkipped(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		grpcServerRoute("users.v1.UserService", "GetUser"),
		// Two distinct structs both embed UnimplementedUserServiceServer.
		facts.Fact{
			Kind: facts.KindSymbol, Name: "a.UserService", Repo: "",
			Props:     map[string]any{"symbol_kind": facts.SymbolStruct},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "usersv1.UnimplementedUserServiceServer"}},
		},
		facts.Fact{
			Kind: facts.KindSymbol, Name: "b.UserService",
			Props:     map[string]any{"symbol_kind": facts.SymbolStruct},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "usersv1.UnimplementedUserServiceServer"}},
		},
		facts.Fact{Kind: facts.KindSymbol, Name: "a.UserService.GetUser", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
		facts.Fact{Kind: facts.KindSymbol, Name: "b.UserService.GetUser", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
	)

	bind(t, store)

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/users.v1.UserService/GetUser"})
	if _, ok := routeHandledBy(routes[0]); ok {
		t.Error("ambiguous service short name should not bind")
	}
}

func TestBindGRPCHandlers_Idempotent(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		grpcServerRoute("users.v1.UserService", "GetUser"),
		facts.Fact{
			Kind: facts.KindSymbol, Name: "users.UserService",
			Props:     map[string]any{"symbol_kind": facts.SymbolStruct},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "usersv1.UnimplementedUserServiceServer"}},
		},
		facts.Fact{Kind: facts.KindSymbol, Name: "users.UserService.GetUser", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
	)

	bind(t, store)
	bind(t, store) // second run must not duplicate the relation

	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/users.v1.UserService/GetUser"})
	n := 0
	for _, r := range routes[0].Relations {
		if r.Kind == facts.RelHandledBy {
			n++
		}
	}
	if n != 1 {
		t.Errorf("handled_by relations = %d, want 1 (idempotent)", n)
	}
}

// TestServerBases_AreExtensible pins the point of the convention table: a gRPC codegen
// enola has never seen is supported by adding a row, with no change to the binding
// logic. It uses a fabricated convention deliberately — if this ever needs the binder
// edited to pass, the table has stopped being the extension point it claims to be.
func TestServerBases_AreExtensible(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		grpcServerRoute("users.v1.UserService", "GetUser"),
		facts.Fact{
			Kind: facts.KindSymbol, Name: "users.UserService",
			Props:     map[string]any{"symbol_kind": facts.SymbolClass},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "gen.UserServiceGrpcBase"}},
		},
		facts.Fact{Kind: facts.KindSymbol, Name: "users.UserService.GetUser", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
	)

	// Not matched by either default convention.
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	routes, _ := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/users.v1.UserService/GetUser"})
	if _, ok := routeHandledBy(routes[0]); ok {
		t.Fatal("unknown convention should not bind with the default table")
	}

	extended := append([]ServerBase{}, DefaultServerBases...)
	extended = append(extended, ServerBase{
		Generator: "test-codegen",
		Pattern:   regexp.MustCompile(`^(?:.*\.)?(.+)GrpcBase$`),
	})
	if err := NewWith(extended).Bind(context.Background(), store); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	routes, _ = store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindRoute, Name: "/users.v1.UserService/GetUser"})
	target, ok := routeHandledBy(routes[0])
	if !ok || target != "users.UserService.GetUser" {
		t.Errorf("handled_by = %q (ok=%v), want users.UserService.GetUser", target, ok)
	}
}

// A Java implementation extends the generated <Service>ImplBase, of grpc-java's
// <Service>Grpc or Dubbo Triple's Dubbo<Service>Triple, and names each rpc in lower
// camel case: rpc Check is check().
func TestBindGRPCHandlers_JavaImplBase(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		grpcServerRoute("grpc.health.v1.Health", "Check"),
		grpcServerRoute("helloworld.Greeter", "SayHello"),
		grpcServerRoute("org.apache.dubbo.metadata.MetadataServiceV2", "GetMetadataInfo"),
		facts.Fact{
			Kind: facts.KindSymbol, Name: "tri/service.TriHealthImpl",
			Props:     map[string]any{"symbol_kind": facts.SymbolClass},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "DubboHealthTriple.HealthImplBase"}},
		},
		facts.Fact{Kind: facts.KindSymbol, Name: "tri/service.TriHealthImpl.check", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
		facts.Fact{
			Kind: facts.KindSymbol, Name: "greeter.GreeterImpl",
			Props:     map[string]any{"symbol_kind": facts.SymbolClass},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "GreeterGrpc.GreeterImplBase"}},
		},
		facts.Fact{Kind: facts.KindSymbol, Name: "greeter.GreeterImpl.sayHello", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
		facts.Fact{
			Kind: facts.KindSymbol, Name: "metadata.MetadataServiceDelegationV2",
			Props:     map[string]any{"symbol_kind": facts.SymbolClass},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "dubbo-metadata/src.DubboMetadataServiceV2Triple.MetadataServiceV2ImplBase"}},
		},
		facts.Fact{Kind: facts.KindSymbol, Name: "metadata.MetadataServiceDelegationV2.getMetadataInfo", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
	)
	bind(t, store)
	want := map[string]string{
		"/grpc.health.v1.Health/Check":                                 "tri/service.TriHealthImpl.check",
		"/helloworld.Greeter/SayHello":                                 "greeter.GreeterImpl.sayHello",
		"/org.apache.dubbo.metadata.MetadataServiceV2/GetMetadataInfo": "metadata.MetadataServiceDelegationV2.getMetadataInfo",
	}
	for _, f := range store.ByKind(facts.KindRoute) {
		got, _ := routeHandledBy(f)
		if got != want[f.Name] {
			t.Errorf("%s handled_by = %q, want %q", f.Name, got, want[f.Name])
		}
	}
}
