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
| `end_line` | int | when measured | 1-based end line of the span. Symbols from the TypeScript, Python, Ruby, Go, Java, Kotlin and Scala extractors carry it: the declaration's last line, or the file's for a symbol that stands for its whole file (a Vue, Svelte or Ember component) |
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
4. If no fact carries the name, and the target is another loaded repository's
   symbol or package as the consumer's source spells it (a Go import path under
   that repository's module path, an npm package name a repository's
   `package.json` declares, or a Java/Scala/.NET fully qualified type name a
   type's `fqn` prop carries), emit the ID of the one fact that repository
   declares under its own name for it. The target keeps the consumer's spelling:
   `github.com/acme/auth.AuthService.Login` resolves to the `auth` repository's
   `..AuthService.Login`.

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
| `association` | `Model#macro` (e.g. `Order#items`), `Model::method` in PHP, or `Child<Parent` for an STI chain | no | A framework model relationship (Rails belongs_to/has_many, Eloquent belongsTo/hasMany, ...) |
| `test_ref` | test file path | yes (`calls`, `instantiates`) | Reference-only: which production symbols a test exercises. Test files are otherwise excluded from indexing |
| `file_ref` | source file path | yes (`calls`, `instantiates`) | Reference-only: call edges made in file-scope (top-level) code with no enclosing symbol |
| `lint` | the linter finding's id | no | A finding an external linter reported through the provider seam |

### symbol

Contract props:

- `symbol_kind`: one of `function`, `method`, `getter`, `struct`,
  `interface`, `type`, `class`, `variable`, `constant`, `enum`, or, for
  Markdown, `document` (a page) and `section` (one of its headings). A
  document or section is queryable but takes no edge in the dependency graph:
  prose about the code is not a dependency of it.

### module

- `module_role`: `production` | `test` | `tooling` | `unknown`. Absent means
  included in production analyses.
- Compilation-unit props, at most one set: `crate` (Rust), `project` (C#),
  `jvm_module` (Scala), `pub_package` (Dart) — the build unit a module compiles
  into. Several modules may share one value; that is what makes a cycle between
  them legal or not.

### Tailwind design tokens

A token a repository's Tailwind theme declares is a `symbol` with `symbol_kind`
`constant`, named `tailwind/<section>.<token>` (`tailwind/colors.ui-bg-highlight`):
`token` (the class-facing name: nested keys joined with `-`, `DEFAULT` naming its
parent), `token_section` (`colors`, `spacing`, `fontSize`, `borderRadius`, …),
`token_source` `tailwind`, `value`. Read from `tailwind.config.*`, preset and plugin
modules (`theme` and `theme.extend`, spreads and relative imports followed) and v4
`@theme` blocks. A component (or, when a file declares several or none, the file's
`file_ref`) `names` every token its class strings use (`hover:bg-ui-bg-highlight/50`);
only files of a project that styles with Tailwind are read, a config's `prefix`
(`tw-`) is part of the class, and Tailwind's built-in palette is not a token.

### route

- `source`: which pass or contract format produced the route —
  [Route sources](#route-sources). Absent on routes from framework passes that
  register no provenance (most server-side route declarations carry none), so a
  consumer must treat it as optional rather than as the route's classifier.
- `role`: `client` | `server`. Absent means server: a route declaration found
  without a call site is a served endpoint.
- `type`: sub-classifies beyond HTTP — `grpc` | `graphql` | `middleware` | `hook`.
  `hook` is a WordPress hook: a registration on it (`add_action`, `add_filter`) or,
  with `role` `client`, a hook point that fires it (`do_action`, `apply_filters`). A
  hook is in-process dispatch and is kept out of HTTP matching.
  Absent means a plain HTTP route. A gRPC route's path is the wire path
  `/pkg.Service/Method`.
- `framework`: mostly descriptive; the value `grpc` is branched on (it labels
  cross-repo edges via=grpc).
- `method`: the HTTP verb; `*` means the route handles every verb (a raw
  servlet, or a mapping declared without one).
- `matched_routes` (client routes, multi-repo mode): the server routes the
  cross-repo HTTP linker resolved this call site to, as an array of
  `{repo, name, file, method, confidence, id}`. `id` is the target route's fact
  id, computed by the writer like `target_id`; `confidence` is `verified` or
  `probable` under the rule the service edge uses. Only the chosen provider's
  routes are listed, so a call several repositories serve with nothing to choose
  between them has none. A call its own repository serves keeps its match,
  although it draws no service edge. This is a prop rather than a relation
  because a call and the route it reaches usually share a name, and a relation
  names its target by name.
- `caller`, `caller_id` (client routes): the symbol whose body makes the call,
  the innermost function containing the call site that has a symbol of its own.
  `caller_id` is that symbol's fact id; the symbol is always declared in the call
  site's own file. Absent for a call at module scope or inside a function with no
  symbol of its own. Written by the TypeScript, Kotlin, Swift,
  Java, Dart and Python extractors; a Retrofit or Feign route's caller is the interface
  method it annotates.
- `code_symbol` (GraphQL routes, TypeScript): the symbol the operation or field is
  written in. On a client operation it is the variable, function or method holding
  the document, which `calls` the operation; on a code-first field it is the
  decorated resolver method, which the field is `handled_by`.
- `operation` (GraphQL client routes): the operation name the document declares
  (`query ListUsers`), what a test intercepting by `operationName` matches.
- `unresolved_reason` (GraphQL client routes): why no loaded schema declares the
  operation's root field. `no-server-schema-in-repo`: nothing in the operation's
  package (nearest `package.json`) uses a schema of this repository. `field-not-found`:
  the package does, and no field enola read from it answers. It states what was
  read, not that the field is absent. Written after linking and cleared when a loaded
  repository serves the field. Not `unmatched_by_server`, which is the HTTP verdict.

### dependency

- `source`: where the import resolves — `internal` | `external` | `stdlib` | `framework` (a compiler/framework-provided virtual module).
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
- `resolved_via` (TypeScript imports): what resolved an aliased import —
  `vite-alias` | `nuxt-alias` | `tsconfig-paths`. Absent on a relative import.
- `unresolved_reason` (TypeScript imports): why an import matching an alias was not
  resolved — `custom-resolver` (the alias has a `customResolver`) or `dynamic-alias`
  (its replacement is computed at runtime). The import stays external, unguessed.
- `html_entry`: the import is a Vite HTML entry's `<script type="module" src>`.
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
- `messaging`: the protocol. Beside the broker protocols (`kafka`, the AsyncAPI ones),
  the TypeScript extractor reads in-process events and job queues as topics of their
  own protocol: `medusa-event` (a subscriber's `config.event`, `eventBus.emit`,
  `emitEventStep`), `directus-event` (`emitter.emitAction`/`onAction`, `defineHook`
  handlers), `node-event` (an `EventEmitter` declared in the file, or a class extending
  one; a stream's `error`/`close`/`data` and the like are not read; named
  `<emitter>:<event>`, the emitter being the class or module binding, because two
  emitters' same-named events are unrelated) and `bullmq`
  (`new Queue(name)`, `new Worker(name, …)`). Their `source` is
  `typescript-event-call`; neither the AsyncAPI contract binder nor the Kafka
  cross-repository signal reads them. The code on each side `publishes` or
  `subscribes` to the topic; `code_symbol` names it.
- `event_ref`: the constant an event name was written as (`OrderEvents.PLACED`),
  resolved through the repository's exported enums and flat object constants.
  A name that cannot be read statically creates no topic: the symbol that wrote it
  carries `unresolved_reason` `dynamic-event-name` instead.
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

- `model`, `macro` (e.g. `has_many`, or Eloquent's `hasMany`), `target`; `through` when set.
- STI chains are emitted as associations with `macro: inherits`.

### lint

- `lint_engine`, `lint_rule`, `lint_severity`, `line`, `message`

### test_ref / file_ref

Reference-only kinds. They carry only reference edges — `calls`, and
`instantiates` where the reference is a constructor call — naming the
production symbols they exercise; nothing counts them as coupling. A consumer
that builds a call graph should treat their targets as "referenced" without
adding them to coupling metrics.

A browser or API test's `test_ref` also records what the test hits, for the
post-link binder that writes its `exercises` relations: `test_http_calls`
(`METHOD /path`, `*` when the test states no verb, a computed segment as
`:param`), `test_page_visits`, `test_graphql` (root fields sent inline) and
`test_graphql_ops` (operation names intercepted by). `test_calls_unmatched` lists
those no loaded route answers; a bare `/` is never matched, since a test's root is
the base URL it was given.

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
| `handled_by` | A route/endpoint is served by target, its handler symbol. Written at extraction where the route sits on its handler (decorator and annotation frameworks, .NET, Rails routes, Laravel routes) and by post-link binders elsewhere: a Go handler that is a package function or has the `func(http.ResponseWriter, *http.Request)` signature, a routes-file handler named `<type fqn>.<method>` (Play), and gRPC implementations. For a Rails route the handler is the method that serves it: looked for inside the namespace the route's engine isolates, and on the controller's ancestors when the action is inherited. Emitted only when the target is a symbol the snapshot declares under that exact name |
| `implemented_by` | A declared contract operation is implemented by a code symbol. Added post-extraction |
| `publishes` | Source code emits, sends or enqueues the target event or job queue (a `storage` fact, `storage_kind` `topic`). Never coupling; `impact_analysis` and `traverse` follow it |
| `subscribes` | Source code handles the target event or job queue. Never coupling; `impact_analysis` and `traverse` follow it |
| `exercises` | A test file (`test_ref`) hits target: a server route it requests or intercepts, a page route it visits, a GraphQL operation it sends. Added post-link, matching the test's calls against every loaded route the way the HTTP linker matches a client call. Never coupling: fan-in, hotspots and package metrics ignore it, `impact_analysis` and `traverse` follow it |
| `names` | Source names target by symbol literal without calling it — a method name passed as data for something else to dispatch, or a PHP class written as `Foo::class`, an enum case, a class constant or a static property. A reference, not a call |

## Route sources

The `source` prop on a route fact: which extractor pass or contract format
produced it.

HTTP client call sites:

| Value | Meaning |
|---|---|
| `go-http-client` | Go net/http call site |
| `python-http-client` | Python requests / httpx / aiohttp call site |
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

One descriptive prop is prose rather than a measurement:

- `doc` (symbols, and Go, Python, Java, TypeScript and Rust modules): the first
  paragraph of the comment its author wrote on the symbol, on one line: a Python
  docstring, the comment block above a Go declaration, a JSDoc block or run of `//`
  lines above a TypeScript one, or a run of `#` lines above a Ruby `def`, `class` or
  `module`, a Javadoc, KDoc, Scaladoc or PHP docblock, the `<summary>` of a C# `///`
  XML comment, or the `///` lines or `/** */` block above a Rust item or a Swift or
  Dart declaration, read above its attributes or annotations, or the Doxygen block
  or run of `//` lines above a C or C++ declaration or the template holding it,
  where `@brief` opens the description and a definition's own doc wins over its
  prototype's. Of a block with tags, the text before its first tag; tool directives
  such as `// eslint-disable` and `# rubocop:disable` are left out. On a Go module,
  the package comment: from `doc.go` when it has one, else the first file by name
  that does, never a test file or a licence. On a Python module, the docstring of
  its `__init__.py`. On a Java module, the Javadoc of its `package-info.java`. On a
  TypeScript module, the JSDoc block opening its `index` file, when it is marked
  `@packageDocumentation`, `@module` or `@fileoverview`. On a Rust module, the `//!`
  lines or `/*! */` block opening its `lib.rs`, `main.rs` or `mod.rs`, else the
  `foo.rs` beside its `foo/`. Cut at a word and ended with `...` when it runs past
  300 bytes; the whole comment is in [docs.jsonl](docs.md). Absent when the symbol
  has no comment, and on languages whose extractor does not read one.
  `diff_snapshot` and `enola check` do not report a change to it.

`doc` adds no node and no edge. The same function, from a build before `doc` and
from one after:

```json
{"kind":"symbol","name":"internal/flows.Analyze","file":"internal/flows/flows.go","line":186,
 "props":{"symbol_kind":"function","exported":true,"cyclomatic":17}}

{"kind":"symbol","name":"internal/flows.Analyze","file":"internal/flows/flows.go","line":186,
 "props":{"symbol_kind":"function","exported":true,"cyclomatic":17,
          "doc":"Analyze finds the entries of a graph, walks each, and ranks what it found."}}
```

It is read as written. A comment that names an old name of the function, or
describes what it used to do, is passed on unchanged.

Where it is read:

| Reader | Uses `doc` |
|---|---|
| `facts.jsonl` | As a prop of the symbol or module fact |
| `docs.jsonl` | The whole comment it summarizes, joined by `id`. See [docs.md](docs.md) |
| `enola flows --show` and `--json`, and `flows.json` | In a flow's summary, and on its entry and each stage. See [flows.md](flows.md#example) |
| `diff_snapshot`, `enola check` | Ignore it, so editing a comment is not an architecture change |
| MCP tools | Only where a tool returns facts as JSON: `query_facts` in `full` mode and `architecture_blame`, as a prop. The summary and markdown outputs never print it, by design: `doc` on every result would spend an agent's context on prose it rarely needs. `show_symbol` prints the source, comment included, when an agent asks for it. No tool prints the whole comment |

Go, Python, TypeScript, Ruby, Java, C#, PHP, Rust, Kotlin, Scala, Swift, Dart and
C/C++ extractors fill it on symbols; Go, Python, Java, TypeScript and Rust on
modules.

### body_hash

`body_hash` (symbols with an `end_line`) is the version of a symbol: 16 hex
characters of SHA-256 over the lines its span covers, each line trimmed and blank
lines dropped. The id names the declaration and stays the same while its body is
rewritten, and `line` moves whenever anything above it changes; `body_hash`
changes exactly when the code inside the span does. Re-indenting a function,
padding it with blank lines or moving it leaves it unchanged; editing one token
changes it. A class's span holds its methods, so a class's version changes with
any of them.

It is computed after extraction from the span alone, so every symbol that has a
span has one, and a symbol without a span has none.

| Reader | Uses `body_hash` |
|---|---|
| `facts.jsonl` | As a prop of the symbol fact |
| `diff_snapshot`, `enola check` | Ignore it, so editing a function's body is not an architecture change |
| `enola blame`, `architecture_blame` | Pair a symbol that left and came back in one revision: `shifted` when the body is the same (only its line moved), `rewritten` when it changed, `changed` when either side has no version (a revision recorded before `body_hash`). A symbol that left one file and arrived in another with the same kind, name inside its module and body is reported as `moved`, so a reader can follow it across the new id |
