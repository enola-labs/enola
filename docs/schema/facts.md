# facts.jsonl

One JSON fact per line. Facts and their relations are sorted before
serialization.

## Fact object

| Field | Type | Present when | Meaning |
|---|---|---|---|
| `kind` | string | always | Fact kind — [Fact kinds](#fact-kinds) |
| `name` | string | always | Canonical name. For path-shaped kinds (`module`, `test_ref`, `file_ref`), this is a repo-relative path |
| `file` | string | when set | Source file, relative to the repo root; prefixed `<repo>/` in multi-repo mode |
| `line` | int | when measured | 1-based start line |
| `end_line` | int | when measured | 1-based end line of the span |
| `column` | int | when measured | 1-based start column |
| `end_column` | int | when measured | 1-based column one past the span's end |
| `repo` | string | always | Repository label: the repository's own name taken from its git remote, or its directory name when there is no usable remote. A single-repo snapshot carries it too, with the same value on every fact — a snapshot is multi-repo when more than one distinct value appears, not when the field is present |
| `props` | object | when set | Kind-specific properties — per-kind sections below |
| `relations` | array of Relation | when set | Directed edges to other facts |
| `id` | string, 32 lowercase hex chars | current writers: always; historical version-1 writers: when supported | Fact identity — [Identity and IDs](#identity-and-ids) |

## Relation object

| Field | Type | Meaning |
|---|---|---|
| `kind` | string | Relation kind — [Relation kinds](#relation-kinds) |
| `target` | string | The target fact's **name** |
| `target_id` | string, 32 lowercase hex chars, when set | Resolved target identity — [Relation target resolution](#relation-target-resolution) |

Use `target_id` when present. When absent, preserve `target` as an unresolved
reference. It may name an external target, be ambiguous, or come from a
historical writer that did not emit IDs.

### Relation target resolution

Current writers resolve `target_id` using the source fact's repository:

1. Find facts named `target` in the source fact's repository.
2. Emit an ID if all local matches have the same identity.
3. If there is no local match, emit an ID only if all matches across the
   snapshot have the same identity.

Do not choose an arbitrary fact when `target_id` is absent.

## Identity and IDs

Current writers compute `id` as `sha256(repo \0 kind \0 name \0 file)`, truncated
to 128 bits and written as 32 lowercase hex characters. The ID excludes source
positions, so it remains stable when code moves within the same file.

The ID identifies `(repo, kind, name, file)`; it does not uniquely identify a
JSONL record. Facts with the same four values share an ID. A consumer
materializing one node per ID must combine relations and define how it retains
locations and conflicting props. Preserve the original JSONL records when raw
record fidelity is required.

ID stability requires all four inputs to remain stable. `repo` normally comes
from the Git remote's repository name and falls back to the checkout directory
name when no usable remote exists. Remote-less checkouts under different
directory names therefore produce different IDs.

### The name rules underneath

Enola's internal graph resolves relation targets by name. `facts.jsonl` is not a
deduplicated node set, so `(repo, kind, name)` and `name` are not uniqueness
guarantees.

Use `id` rather than `(repo, kind, name)` to keep same-named facts from different
files separate. Retain `repo`; removing it merges identities across repositories.

Name normalization: for path-shaped kinds (`module`, `test_ref`, `file_ref`)
the store normalizes `name` to forward slashes. No other kind's name is
normalized — PHP symbol names use backslashes as namespace separators
(`App\Http\Controllers\UserController`), and rewriting them would break
resolution. `Relation.Target` is never normalized: it must spell the target
fact's name exactly as that fact carries it.

## Fact kinds

| Kind | Name is | Carries relations | Notes |
|---|---|---|---|
| `module` | repo-relative directory path | yes | A package-level grouping (Go package, TS module dir, ...) |
| `symbol` | canonical symbol name (`pkg.Type`, `Owner#method`) | yes | Functions, methods, types, classes, variables, ... |
| `route` | endpoint path or wire path (`/v1/x`, `/pkg.Svc/M`) | yes | HTTP/gRPC/GraphQL surface, client or server side |
| `storage` | table / bucket / topic name | yes (`declares`) | Databases, object stores, messaging topics |
| `dependency` | `<importer> -> <target>` for an import edge; the package name for a declared external package | yes (`imports`) | Import edges and declared packages |
| `service` | repository label | yes (`depends_on`) | A whole repo — multi-repo (append) mode only |
| `intent` | the declared entry's name | yes | Architecture declared in `enola-intent.yaml`, not measured from source |
| `extraction` | `<extractor>:<account>` (e.g. `ruby:calls`) | no | An extractor's own coverage account for one repo |
| `association` | `Model#macro` (e.g. `Order#items`) or `Child<Parent` for an STI chain | no | A framework model relationship (Rails belongs_to/has_many, ...) |
| `test_ref` | test file path | yes (`calls`, `instantiates`) | Reference-only: which production symbols a test exercises. Test files are otherwise excluded from indexing |
| `file_ref` | source file path | yes (`calls`, `instantiates`) | Reference-only: call edges made in file-scope (top-level) code with no enclosing symbol |
| `lint` | the linter finding's id | no | A finding an external linter reported through the provider seam |

### symbol

Contract props:

- `symbol_kind`: one of `function`, `method`, `getter`, `struct`,
  `interface`, `type`, `class`, `variable`, `constant`, `enum`

### module

- `module_role`: `production` | `test` | `tooling` | `unknown`. Absent means
  included in production analyses.
- Compilation-unit props, at most one set: `crate` (Rust), `project` (C#),
  `jvm_module` (Scala), `pub_package` (Dart) — the build unit a module compiles
  into. Several modules may share one value; that is what makes a cycle between
  them legal or not.

### route

- `source`: which pass or contract format produced the route —
  [Route sources](#route-sources). Absent on routes from framework passes that
  register no provenance (most server-side route declarations carry none), so a
  consumer must treat it as optional rather than as the route's classifier.
- `role`: `client` | `server`. Absent means server: a route declaration found
  without a call site is a served endpoint.
- `type`: sub-classifies beyond HTTP — `grpc` | `graphql` | `middleware`.
  Absent means a plain HTTP route. A gRPC route's path is the wire path
  `/pkg.Service/Method`.
- `framework`: mostly descriptive; the value `grpc` is branched on (it labels
  cross-repo edges via=grpc).
- `method`: the HTTP verb; `*` means the route handles every verb (a raw
  servlet, or a mapping declared without one).

### dependency

- `source`: where the import resolves — `internal` | `external` | `stdlib`.
  This is a second, unrelated vocabulary on the same prop key as route's
  `source`, discriminated by the fact's kind. Reading `source` without first
  checking kind gets a value from the wrong vocabulary.
- `type`: `package` marks a declared external package read from a manifest
  rather than an import edge between this repository's own modules. In
  multi-repo mode two further values name cross-repo facts: `cross_repo` is a
  real directional edge (one repo imports or calls the other) and is the only
  one carrying a `depends_on` relation. `cross_repo_shared_code` is a
  symmetric coupling: two repos declaring the same distinctive type names,
  with no relation attached, because shared code is not a dependency and does
  not compose across hops.
- `ecosystem`: the packaging system a declared package was declared in — `go` |
  `npm` | `rubygems` | `cargo` | `pub` | `pypi`.
- `target_file`: repo-relative file the producer reports the target to be
  defined in; disambiguates a name several files declare. Absent, the target
  resolves by name alone.
- `via`: how a cross-repo edge was established — [Via kinds](#via-kinds)
  (multi-repo mode).
- `param_segment_endpoints`, `param_segment_endpoint_count`: on an HTTP cross-repo
  edge, the endpoints that matched only because a server path parameter stood in for
  a literal client segment (`linking.match_literal_against_params`). Each is also in
  `endpoints`. The edge's `confidence` is the strongest among its endpoints, so this
  list is how to weigh the looser ones.
- `coupling_kind`: on a synthetic module-coupling edge derived from
  references rather than read from an import statement), which reference
  produced it — `reference` (constant-receiver call), `inheritance`, `mixin`,
  `association` (ActiveRecord has_many/belongs_to, which is bidirectional by
  nature and must not be read as a cycle), `require`, `packwerk`, or
  `symbol-rollup` (rolled up from resolved symbol edges, for languages with no
  import statement). Absent means unclassified — a normal edge. Where several
  references produce one edge the hardest kind wins.

### storage

- `storage_kind`: e.g. `topic` — a messaging topic reference, as opposed to a
  database table or object store. The topic name is the fact's `name`.
- `messaging_role`: `producer` | `consumer`. Absent when only a bare topic
  reference was seen; the direction is then inferred from the topic name's
  owning-service prefix.
- `messaging_operation`: `publish` | `subscribe`.
- `source` on a topic fact: a third vocabulary on the `source` key, again
  discriminated by kind — `asyncapi` (read from an AsyncAPI spec),
  `go-kafka-call` or `typescript-kafka-call` (a call site).
- `messaging`: the protocol carried by the topic operation (e.g. `kafka`).
  Security suffixes describe the transport, not a different broker, so
  `kafka-secure` is read as the same family as `kafka`.

Contract-bound topic operations additionally carry `messaging_*` binding props
written by the contract binder. Two of them have a fixed vocabulary:
`messaging_contract_status` is `bound`, `undeclared`, `ambiguous` or
`protocol_mismatch`, and `messaging_implementation_status` is `implemented` or
`unimplemented` — a missing binding is explainable rather than a bare absence.
The remaining `messaging_*` props (`messaging_contract_bound`,
`messaging_contract_file`, ...) carry ids, counts and file paths.

### service (multi-repo mode)

- `edge_coverage`: array of `{edge_type, detected, resolved, unresolved}` —
  how many outbound call sites of each type were detected and resolved to a
  loaded service.

### intent

- `intent_kind`: the declared entry's kind — e.g. `service`, `seam` /
  `consumes`, `layer`, `claim`, plus knowledge-page kinds (`page`, `relation`,
  `anchor`). Kind-specific props follow the declaration (a layer carries
  `layer_name`, `order`, `paths`).
- `source`: the declaration file (e.g. `enola-intent.yaml`).
- `overridden`: set when a cluster config replaced the repo's own declaration.

### extraction

- `extractor`, `language`
- `edge_coverage`: same shape as on service facts — the extractor's own account
  of detected vs resolved edges.
- `unresolved_*`: per-cause counts the extractor could not resolve (e.g.
  `unresolved_macros`).
- A declared in-house client's account, one per repository and named
  `<extractor>:client:<name>`, carries `client_spec`, `receivers` (class members declared
  with one of its receiver types), `edge_coverage` with edge type `configured_http_call`
  (detected: calls through the client, resolved: routes they became, unresolved:
  skipped) and `skipped`, per-cause counts written `cause=n,...` (`dynamic_path`,
  `missing_path_argument`, `non_route_path`). It is emitted even when the client found
  nothing, so a declaration that matches nothing is visible.

### association

- `model`, `macro` (e.g. `has_many`), `target`; `through` when set.
- STI chains are emitted as associations with `macro: inherits`.

### lint

- `lint_engine`, `lint_rule`, `lint_severity`, `line`, `message`

### test_ref / file_ref

Reference-only kinds. They carry only reference edges — `calls`, and
`instantiates` where the reference is a constructor call — naming the
production symbols they exercise; nothing counts them as coupling. A consumer
that builds a call graph should treat their targets as "referenced" without
adding them to coupling metrics.

## Relation kinds

| Kind | Meaning |
|---|---|
| `declares` | Source is declared in target. A symbol, route, or storage fact points to its containing `module`; no inverse module-to-child relation is emitted |
| `imports` | Source imports/depends on target (module or package) |
| `calls` | Source calls target |
| `implements` | Source implements/extends target (an interface, a base class) |
| `depends_on` | Source depends on target — used for cross-repo service edges (multi-repo mode) |
| `instantiates` | Source constructs an instance of target via a constructor call |
| `injects` | Source declares target as a DI-injected constructor parameter |
| `has_method` | Owner type (struct/interface/class) declares target as a method. Synthesized, not read from source |
| `handled_by` | A route/endpoint is served by target (e.g. a gRPC RPC route to its handler method). Added post-extraction |
| `implemented_by` | A declared contract operation is implemented by a code symbol. Added post-extraction |
| `names` | Source names target by symbol literal without calling it — a method name passed as data for something else to dispatch. A reference, not a call |

## Route sources

The `source` prop on a route fact: which extractor pass or contract format
produced it.

HTTP client call sites:

| Value | Meaning |
|---|---|
| `go-http-client` | Go net/http call site |
| `ts-http-client` | TypeScript fetch/axios call site |
| `ruby-http-client` | Ruby HTTP call site |
| `php-http-client` | PHP HTTP call site |
| `java-http-client` | Spring RestTemplate / WebClient |
| `feign` | Spring Cloud @FeignClient interface |
| `retrofit` | Kotlin/Java Retrofit service interface |
| `urlsession` | Swift URLSession |
| `swift-endpoint` | Swift endpoint enum / protocol extension |
| `scala-http-client` | sttp / Play WS / http4s client |
| `dart-http-client` | Dart package:http / dio / chopper call site or annotated interface |
| `configured-http-client` | call site through an in-house client declared under `clients:` in the config; carries `client_spec` (the declaring spec's name) and, when the call names a service, `target_hint` |

gRPC client call sites: `go-grpc-client`, `ts-grpc-client`,
`python-grpc-client`.

Contract-derived routes (read from a spec or IDL — an interface, not a call
site): `grpc-proto` (.proto service definition), `openapi`,
`openapi-typescript` (generated TS client from a spec).

GraphQL: `graphql-ruby-dsl` (server field DSL), `graphql-sdl` (schema-first
server SDL), `graphql-code-first-decorator` (NestJS GraphQL or TypeGraphQL root
decorator), `graphql-nexus` and `graphql-pothos` (code-first builder APIs),
`graphql-tag` (client operations in tagged templates), `graphql-client-call`
(static document passed through a GraphQL client or HTTP request),
`graphql-operation-file` (standalone .graphql documents), and
`graphql-ruby-string` (Ruby operation-string literals).

Framework route declarations: `symfony-config` (Symfony YAML/XML),
`play-routes` (Play conf/routes), `pekko-http`, `http4s`, `angular-router`.

## Via kinds

The `via` prop on a cross-repo dependency fact (multi-repo mode): how the
edge was established.

| Value | Meaning |
|---|---|
| `http` | OpenAPI/spec-derived client call |
| `http-client` | hand-written HTTP client call site |
| `grpc` | gRPC call site or service stub |
| `graphql` | GraphQL operation matched to a root field |
| `kafka` | topic produced by one repo, consumed by another |
| `import` | shared-library import |
| `shared_symbols` | two repos declaring the same exported symbols — evidence of shared code, not a call |
| `object-storage` | one repo writes objects to a bucket path another reads (declared seams; no linker measures it yet) |

## Descriptive props

Props outside the contract — `language`, `exported`, `handler`, `superclass`,
line counts, complexity metrics — are metadata. Extractors can add or remove
them without changing `format_version`; consumers must not require them. The
contract props are the ones documented above.
