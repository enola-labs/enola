package grpcextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// extractAll writes the given files to a temp dir and runs the extractor over
// them, mirroring the map-fixture pattern used by the other extractor tests.
func extractAll(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	var rel []string
	for name, content := range files {
		abs := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, name)
	}
	ff, err := New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return ff
}

func factsByKind(ff []facts.Fact, kind string) []facts.Fact {
	var out []facts.Fact
	for _, f := range ff {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func routeByName(ff []facts.Fact, name string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Name == name {
			return f, true
		}
	}
	return facts.Fact{}, false
}

const usersProto = `syntax = "proto3";

package users.v1;

message User {
  string id = 1;
  string name = 2;
}

service UserService {
  rpc GetUser(GetUserRequest) returns (GetUserResponse);
  rpc CreateUser(CreateUserRequest) returns (CreateUserResponse);
}

message GetUserRequest { string user_id = 1; }
message GetUserResponse { User user = 1; }
message CreateUserRequest { string name = 1; }
message CreateUserResponse { User user = 1; }
`

func TestExtract_ServerRoutesPerRPC(t *testing.T) {
	ff := extractAll(t, map[string]string{"proto/users/v1/users.proto": usersProto})

	routes := factsByKind(ff, facts.KindRoute)
	if len(routes) != 2 {
		t.Fatalf("expected 2 route facts, got %d: %+v", len(routes), routes)
	}

	get, ok := routeByName(ff, "/users.v1.UserService/GetUser")
	if !ok {
		t.Fatal("missing route /users.v1.UserService/GetUser")
	}
	if get.Props["method"] != "POST" {
		t.Errorf("method = %v, want POST", get.Props["method"])
	}
	if get.Props["role"] != "server" {
		t.Errorf("role = %v, want server", get.Props["role"])
	}
	if get.Props["framework"] != "grpc" {
		t.Errorf("framework = %v, want grpc", get.Props["framework"])
	}
	if get.Props["rpc_service"] != "users.v1.UserService" {
		t.Errorf("rpc_service = %v", get.Props["rpc_service"])
	}
	if get.Props["rpc_method"] != "GetUser" {
		t.Errorf("rpc_method = %v", get.Props["rpc_method"])
	}
	if get.Props["streaming"] != "none" {
		t.Errorf("streaming = %v, want none", get.Props["streaming"])
	}

	if _, ok := routeByName(ff, "/users.v1.UserService/CreateUser"); !ok {
		t.Fatal("missing route /users.v1.UserService/CreateUser")
	}
}

func TestExtract_ServiceAndMessageSymbols(t *testing.T) {
	ff := extractAll(t, map[string]string{"proto/users/v1/users.proto": usersProto})

	var sawService, sawRPCMethod, sawMessage bool
	for _, f := range factsByKind(ff, facts.KindSymbol) {
		switch f.Name {
		case "proto/users/v1.UserService":
			sawService = true
			if f.Props["symbol_kind"] != facts.SymbolInterface {
				t.Errorf("service symbol_kind = %v", f.Props["symbol_kind"])
			}
		case "proto/users/v1.UserService.GetUser":
			sawRPCMethod = true
			if f.Props["symbol_kind"] != facts.SymbolMethod {
				t.Errorf("rpc symbol_kind = %v", f.Props["symbol_kind"])
			}
			hasHasMethod := false
			for _, r := range f.Relations {
				if r.Kind == facts.RelHasMethod && r.Target == "proto/users/v1.UserService" {
					hasHasMethod = true
				}
			}
			if !hasHasMethod {
				t.Errorf("rpc method missing has_method edge to service: %+v", f.Relations)
			}
		case "proto/users/v1.User":
			sawMessage = true
			if f.Props["symbol_kind"] != facts.SymbolStruct {
				t.Errorf("message symbol_kind = %v", f.Props["symbol_kind"])
			}
		}
	}
	if !sawService || !sawRPCMethod || !sawMessage {
		t.Fatalf("missing symbols: service=%v rpcMethod=%v message=%v", sawService, sawRPCMethod, sawMessage)
	}
}

func TestExtract_StreamingKinds(t *testing.T) {
	proto := `syntax = "proto3";
package chat.v1;
service Chat {
  rpc Send(Msg) returns (Ack);
  rpc Subscribe(Sub) returns (stream Msg);
  rpc Upload(stream Chunk) returns (Ack);
  rpc Pipe(stream Msg) returns (stream Msg);
}
`
	ff := extractAll(t, map[string]string{"chat.proto": proto})
	want := map[string]string{
		"/chat.v1.Chat/Send":      "none",
		"/chat.v1.Chat/Subscribe": "server",
		"/chat.v1.Chat/Upload":    "client",
		"/chat.v1.Chat/Pipe":      "bidi",
	}
	for path, streaming := range want {
		r, ok := routeByName(ff, path)
		if !ok {
			t.Fatalf("missing route %s", path)
		}
		if r.Props["streaming"] != streaming {
			t.Errorf("%s streaming = %v, want %v", path, r.Props["streaming"], streaming)
		}
	}
}

func TestExtract_NoPackage(t *testing.T) {
	proto := `syntax = "proto3";
service Ping { rpc Ping(Empty) returns (Empty); }
message Empty {}
`
	ff := extractAll(t, map[string]string{"ping.proto": proto})
	if _, ok := routeByName(ff, "/Ping/Ping"); !ok {
		t.Fatalf("expected bare-service path /Ping/Ping, got %+v", factsByKind(ff, facts.KindRoute))
	}
}

func TestExtract_ImportsAndComments(t *testing.T) {
	proto := `syntax = "proto3";
package a.b;
import "google/protobuf/timestamp.proto";
import "other/thing.proto";

// service Ghost { rpc Nope(X) returns (Y); }  <- commented out, must be ignored
/* rpc AlsoNope(X) returns (Y); */

service Real {
  rpc Do(Req) returns (Resp); // trailing comment
}
`
	ff := extractAll(t, map[string]string{"a/b/svc.proto": proto})

	if _, ok := routeByName(ff, "/a.b.Real/Do"); !ok {
		t.Fatal("missing route /a.b.Real/Do")
	}
	if len(factsByKind(ff, facts.KindRoute)) != 1 {
		t.Fatalf("commented-out RPCs leaked into routes: %+v", factsByKind(ff, facts.KindRoute))
	}

	var external, internal bool
	for _, d := range factsByKind(ff, facts.KindDependency) {
		if d.Name == "google/protobuf/timestamp.proto" && d.Props["external"] == true {
			external = true
		}
		if d.Name == "other/thing.proto" && d.Props["external"] == false {
			internal = true
		}
	}
	if !external || !internal {
		t.Errorf("import classification wrong: external=%v internal=%v", external, internal)
	}
}

func TestDetect(t *testing.T) {
	dir := t.TempDir()
	if ok, _ := New().Detect(dir); ok {
		t.Fatal("Detect true on empty dir")
	}
	if err := os.WriteFile(filepath.Join(dir, "x.proto"), []byte("syntax=\"proto3\";"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := New().Detect(dir); err != nil || !ok {
		t.Fatalf("Detect = %v, %v; want true, nil", ok, err)
	}
}

// Detect runs before the engine's ignore-glob-filtered file list exists, so it
// walks the repo itself and config.Default()'s "**/testdata/**" cannot reach it.
// A repo whose only .proto are fixtures does not serve gRPC, and enabling the
// extractor on that basis attributes fixture services to the host repo.
// (Extract needs no such guard — it consumes the already-filtered list.)
func TestDetect_IgnoresTestdataFixtures(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(usersProto), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("internal/engine/testdata/repos/svc/proto/users.proto")
	if ok, err := New().Detect(dir); err != nil || ok {
		t.Errorf("Detect = %v, %v; want false, nil (only a testdata fixture present)", ok, err)
	}

	// A first-party .proto alongside the fixture still enables the extractor.
	write("proto/users/v1/users.proto")
	if ok, err := New().Detect(dir); err != nil || !ok {
		t.Errorf("Detect = %v, %v; want true, nil (first-party .proto present)", ok, err)
	}
}

// A message another message holds, or an RPC takes or returns, is used by it,
// across files of the repository; a scalar or an external type adds nothing.
func TestMessageRefs_FieldsAndRPCsNameTheirMessages(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("proto/events.proto", `syntax = "proto3";
package shop;
import "google/protobuf/timestamp.proto";
message LineEvent { string sku = 1; }
message OrderEvent {
  repeated LineEvent lines = 1;
  google.protobuf.Timestamp at = 2;
  map<string, LineEvent> by_sku = 3;
}
`)
	write("proto/api.proto", `syntax = "proto3";
package shop;
message Ack { bool ok = 1; }
service Orders {
  rpc Publish(shop.OrderEvent) returns (Ack);
}
`)
	ff, err := New().Extract(context.Background(), repo, []string{"proto/events.proto", "proto/api.proto"})
	if err != nil {
		t.Fatal(err)
	}
	names := func(name string) []string {
		var out []string
		for _, f := range ff {
			if f.Name == name {
				for _, r := range f.Relations {
					if r.Kind == facts.RelNames {
						out = append(out, r.Target)
					}
				}
				if _, leaked := f.Props[propMessageRefs]; leaked {
					t.Errorf("%s carries %s", name, propMessageRefs)
				}
			}
		}
		return out
	}
	if got := names("proto.OrderEvent"); len(got) != 1 || got[0] != "proto.LineEvent" {
		t.Errorf("OrderEvent names %v, want only LineEvent", got)
	}
	if got := names("proto.Orders.Publish"); len(got) != 2 {
		t.Errorf("Publish names %v, want its request and response", got)
	}
}
