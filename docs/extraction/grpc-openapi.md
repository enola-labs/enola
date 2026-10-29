# gRPC and OpenAPI — the contract as one end of an edge

These two are not languages. They are *declared contracts*, and enola treats them as the
authoritative server side of a cross-repository edge — which is why an edge matched
through them carries `confidence: verified` rather than `probable`.

Fixtures: [`openapi_sample`](../../internal/engine/testdata/repos/openapi_sample/) ·
[`multirepo`](../../internal/engine/testdata/repos/multirepo/) ·
[`go_grpc_multirepo`](../../internal/engine/testdata/repos/go_grpc_multirepo/) ·
[`py_grpc_multirepo`](../../internal/engine/testdata/repos/py_grpc_multirepo/)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| an OpenAPI spec's path + method | a server route with `operationId`, `summary`, `tags` and `spec_file` | `route` |
| the same spec inside a `client/` directory | the same route, stored with `role: client` | `route` |
| `service Users { rpc CreateUser(...) }` | a server route at gRPC's wire path `/package.Service/Method` | `route` |
| an rpc's streaming mode | `streaming`: `none` / client / server / bidi | props |
| a call through a generated stub (Go, Python, TypeScript) | a client route at that same wire path | `route` |
| both sides present in one snapshot | a cross-repo dependency at `confidence: verified`, not `probable` | `dependency` |
| two call sites for one rpc | two facts — collapsing them would understate the blast radius | `route` |

## Why a contract is different from a URL string

Most cross-repo linking compares a client's URL literal against a server's route literal.
That is a *heuristic*: two services can serve `/users`, and a string match cannot tell you
which one the client meant. A `.proto` service name or an OpenAPI `operationId` is a
declared identifier, so the match is an identity, not a resemblance:

```
dependency client -> server   props: type=cross_repo, via=[grpc], confidence=verified,
                                     endpoint_count=2,
                                     endpoints=["POST /users.v1.UserService/CreateUser",
                                                "POST /users.v1.UserService/GetUser"]
dependency consumer -> api    props: type=cross_repo, via=[http-client], confidence=probable
```

`verified` and `probable` are different claims, and the fact says which one you are
looking at.

## gRPC

Detected by any `.proto` file. The service definition becomes server routes; generated
client stubs, and calls through them, become client routes.

```protobuf
service UserService {
  rpc GetUser(GetUserRequest) returns (User);
  rpc CreateUser(CreateUserRequest) returns (User);
}
```

```
route  /users.v1.UserService/GetUser      server/proto/users/v1/users.proto:11
       props: role=server, source=grpc-proto, framework=grpc, method=POST,
              rpc_service=users.v1.UserService, rpc_method=GetUser, streaming=none, type=grpc
route  /users.v1.UserService/CreateUser   server/proto/users/v1/users.proto:12
       props: role=server, source=grpc-proto, …, rpc_method=CreateUser
```

The path form is gRPC's own wire path (`/package.Service/Method`), so the client and
server sides are directly comparable. `streaming` records `none` / client / server / bidi,
because a streaming RPC has different failure and back-pressure characteristics from a
unary one.

Client call sites are found in the language extractors, not the proto extractor:

```
route  /users.v1.UserService/GetUser   client/repo.go:16     props: role=client, source=go-grpc-client
route  /users.v1.UserService/GetUser   client/pkgvar.go:15   props: role=client, source=go-grpc-client
```

Two call sites for the same RPC, kept as two facts — `impact_analysis` on the RPC needs
both, and collapsing them would understate the blast radius. Python (`py-grpc-client`) and
TypeScript gRPC-web clients are recognised the same way.

Generated code is tagged rather than hidden:

```
dependency gen/users/v1 -> google.golang.org/grpc   props: generated=true, source=external
```

so a query can exclude generated stubs without an ignore-glob guess.

## OpenAPI

Detected by any spec with an `openapi:` or `swagger:` key. Each operation becomes one
route per method, carrying the metadata the spec already gives you:

```
route  /widgets       api/openapi/widgets.yaml   props: role=server, method=GET,
                                                        operationId=listWidgets, tags=[widgets],
                                                        summary="List all widgets",
                                                        spec_file=api/openapi/widgets.yaml
route  /widgets       api/openapi/widgets.yaml   props: role=server, method=POST, operationId=createWidget
route  /widgets/{id}  api/openapi/widgets.yaml   props: role=server, method=GET, operationId=getWidget
```

There is no line number: a YAML operation is a configuration entry, and `spec_file` says
which document it came from instead of inventing a position.

### Client specs

A spec that describes an API this repository *calls* rather than serves is recognised by
its location (a `client/` directory) and stored with `role: client`:

```
route  /invoices   api/openapi/client/billing.yml   props: role=client, operationId=listInvoices, tags=[billing]
```

That is what makes a spec-only repository — one with no HTTP client code at all — still
participate in the cross-repo graph:

```
route  /widgets    repoA/api/openapi/widgets.yaml         role=server, operationId=listWidgets
route  /widgets    repoB/api/openapi/client/widgets.yml   role=client, operationId=fetchWidgets

dependency repoB -> repoA   props: via=[http], confidence=verified, endpoints=["GET /widgets"]
service    repoB            props: edge_coverage=[{detected: 1, resolved: 1, unresolved: 0}]
                            --depends_on--> repoA
```

### Calls through a generated client

When the code calling a client spec's generated client is in reach, each call becomes a
client route at the code making it, with its caller, and the spec's operations become
declarations (`spec_declaration=true`) that HTTP matching skips. A repo vendoring a
100-operation spec to use two of them depends on those two.

```
route  /invoices   internal/billing/client.go:41   props: role=client, source=generated-client,
                                                          operationId=listInvoices,
                                                          caller=internal/billing.client.List
route  /invoices   api/openapi/client/billing.yml  props: role=client, spec_declaration=true
```

Two shapes are recognised. A client generated into the checkout: the package whose
generated methods (`generated=true`) implement most of the spec's operations, named after
the operationId with the generator's suffixes (`ListInvoicesWithResponse`), or after verb
and path when the spec has no operationId. And a client generated at build time: a type
imported from a package named after the spec file (`billing.yml` → `…billing.apis.…`, or
with an overlay variant, `…billingedge.apis.…`), with a call to an operation by name in
the importing file. A spec with no call seen keeps its operations as calls.

### Served operations and their implementation

A spec-first server registers its routes in generated code, so the spec's operation and
the hand-written method share only the operationId. Each served operation is bound
(`handled_by`) to the method of the hand-written type implementing more than half of the
spec's operations, spelled as the operationId or in PascalCase (`getWidget` →
`GetWidget`). Generated wrappers and mocks are never candidates, and a tie binds nothing.

## What is deliberately not extracted

- **Schema shapes.** enola records that an operation exists and who calls it, not the
  request or response body. It is an architecture graph, not an API diff tool.
- **`$ref` resolution across documents** for the purpose of route discovery — the paths
  are read where they are declared.
- **Runtime service discovery.** A gRPC channel whose target is resolved from a registry
  at startup is not traced to a service; the RPC identity still matches, which is why the
  edge resolves anyway.
- **Proto options and custom annotations** (`google.api.http` gateway mappings) are not
  yet expanded into REST routes, so a gRPC-gateway REST surface appears only if the
  generated handlers are in a language extractor's reach.

---

Measured on real repositories: [BENCHMARKS.md](../BENCHMARKS.md).
