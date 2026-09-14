# Teaching enola your own HTTP client

Three repositories. `backend` needs `gateway`, but it never calls it directly. It goes
through a connector in a shared SDK, and the connector makes its requests through the
company's own HTTP abstraction:

```ts
this.httpRequestService.sendRequest(this.serviceName, url, { method: GET });
```

enola knows `fetch`, axios and Angular's `HttpClient`. It does not know `sendRequest`, so
out of the box it cannot see these calls, and the dependency on `gateway` is missing from
the graph. One block of config fixes that.

Run it:

```bash
./run.sh
```

You need `enola` on your `PATH`, or `ENOLA=../../enola ./run.sh` after building it from the
repository root. No `node_modules` are needed: enola reads the source, it does not run it.

## Layout

```text
gateway/   NestJS controllers: /v1/catalog/imports and /v1/resources/:type/items
sdk/       ResourceConnector, calling gateway through IHttpRequestService.sendRequest
backend/   CatalogService, which uses the connector from @example/sdk
cluster.yaml                the three repositories, nothing declared
cluster-with-client.yaml    plus the in-house client
cluster-with-params.yaml    plus literal-against-parameter matching
```

## 1. Nothing declared

```text
Cross-repo edges:
    backend -> sdk

  service  classification  detected  resolved  unresolved
  backend  connected             0         0           0
  gateway  isolated              0         0           0
  sdk      isolated              0         0           0
```

`backend -> sdk` comes from the import of `@example/sdk`. `sdk` reads as isolated: enola
detected no outbound calls, because nothing tells it that `sendRequest` makes one.

## 2. Declare the client

`cluster-with-client.yaml` adds:

```yaml
clients:
  - name: sdk-http
    language: typescript
    receiver_types: [IHttpRequestService]
    methods:
      - name: sendRequest
        service_arg: 0
        path_arg: 1
        options_arg: 2
        default_verb: GET

service_aliases:
  resource-api: gateway
```

| Key | What it says |
|---|---|
| `receiver_types` | The type a class injects the client as, by constructor parameter or `inject()` field. The type decides: a `sendRequest` on any other type is not read. |
| `methods[].name` | The method that makes a request. |
| `service_arg` | The argument naming the target service. |
| `path_arg` | The argument holding the path. It is folded through the class's own fields and through locals assigned once. |
| `options_arg`, `default_verb` | Where the verb is read from (`method:` by default), and the verb to use when the call states none. |
| `service_aliases` | Which repository serves a service name the client passes, when the two differ. |

```text
Cross-repo edges:
    backend -> sdk
    sdk -> gateway

  service  classification  detected  resolved  unresolved
  backend  connected             0         0           0
  gateway  isolated              0         0           0
  sdk      connected             2         1           1

Declared clients (clients: in the config)

  client    receivers call_sites  routes  skipped
  sdk-http          1          3       2        1

sdk-http skipped 1 call: dynamic_path ×1
```

The connector makes three calls, and each lands somewhere different:

| Call | What enola read | Result |
|---|---|---|
| `getCatalogItems` | `${this.basePath}catalog/items`, with `basePath` from the class field: `GET /v1/resources/catalog/items` | a route, unresolved: gateway serves it only under a `:type` parameter |
| `startImport` | `POST /v1/catalog/imports` | a route, linked to gateway |
| `getByKey` | `this.pathFor(key)`, a method's return value | no route: skipped as `dynamic_path` |

The service table counts two calls and the client account three. The skipped call never
became a route, so only the account can report it, and it says why.

## 3. Let a parameter stand in for a literal segment

`cluster-with-params.yaml` adds:

```yaml
linking:
  match_literal_against_params: true
```

```text
  service  classification  detected  resolved  unresolved
  sdk      connected             2         2           0
```

`GET /v1/resources/catalog/items` now reaches `/v1/resources/:type/items`. The rule is off
by default, because a parameter matches anything, and it is held to more than an exact
match. It is tried only when no exact match exists, the whole server path must be matched,
two literal segments must agree, a parameter may not lead the server path, and if more
than one server route pattern fits, nothing matches.

The endpoint matched this way is listed in `param_segment_endpoints` on the `sdk -> gateway`
dependency. Read that list to weigh it: an edge reports the strongest confidence among its
endpoints, and here the literal `POST` is a verified match.

The graph now holds `backend -> sdk -> gateway`, which is the answer to "does backend
depend on gateway" that the call style hid.

## Why this is config and not an edge

The `clients:` block says which call is a request and where its path is. It does not say
who depends on whom. An edge still needs a loaded repository that serves the path, so a
wrong entry can at worst produce a route that matches nothing, and `enola coverage` shows
it: a client that matched no call site is named, with the likely cause (its
`receiver_types`, or its method names).

Two settings do influence matching, and both are bounded. A service alias only chooses
among repositories that already serve the path, and it is a constraint: if the aliased
repository serves none of them, no edge is drawn. Parameter matching is opt-in and follows
the rules above.

## What enola still cannot read

- **A path no source states.** A method's return value or a value from runtime config is
  skipped and counted, never guessed.
- **A constant on another class.** `Other.BASE` is not resolved for a declared client; the
  class's own fields and locals are.
- **Other languages.** Declared clients are read in TypeScript only.
- **A direct `backend -> gateway` edge.** The call is made in the SDK, so the dependency is
  the path through it.

For the rules behind cross-repo edges, see [docs/CLUSTERS.md](../../docs/CLUSTERS.md) and
[docs/EXTENDING.md](../../docs/EXTENDING.md).
