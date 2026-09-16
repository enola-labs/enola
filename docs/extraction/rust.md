# Rust — what enola extracts

Parsed with tree-sitter. Detected by a `Cargo.toml`, at the root or up to three levels
deep, so a workspace with member crates in subdirectories is picked up.

Fixture: [`rust_sample`](../../internal/engine/testdata/repos/rust_sample/) ·
Route behaviour is covered by
[`rustextractor/axum_test.go`](../../internal/extractors/rustextractor/axum_test.go) and
[`rustextractor/utoipa_test.go`](../../internal/extractors/rustextractor/utoipa_test.go)

> **Check your `mcp-arch.yaml` before you conclude Rust is unsupported.** A config file's
> `extractors:` list *replaces* the built-in default rather than merging with it, so a
> config written before an extractor existed silently disables it. A repository indexed
> with a stale list reports zero Rust facts and says nothing about why. Either add `rust`
> to the list or delete the key to inherit the default.

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a crate or module directory | one module with its `crate` name | `module` |
| `fn`, `struct`, `enum`, `trait`, `impl` block | a symbol with `symbol_kind` | `symbol` |
| a method in an `impl` | a symbol with `receiver` and `static` | `symbol` |
| `use crate::db::get_user;` | a dependency tagged `internal` / `external` | `dependency` |
| a constructor call | an `instantiates` relation | relation |
| `.route("/x", get(h))` (Axum) | a server route **at its composed mount path** | `route` |
| `.nest("/p", other::router())` | folds `/p` onto every route the nested router declares | — |
| `#[utoipa::path(get, path = "/x")]` on a handler | a server route, one per declared verb | `route` |

## Symbols

```rust
// src/db.rs
pub struct User { … }

impl User {
    pub fn new(name: &str) -> Self { User { name: String::from(name) } }
}

pub fn get_user() -> User { User::new("x") }
```

```
symbol  src.User        src/db.rs:3    props: symbol_kind=struct, exported=true
symbol  src.User.new    src/db.rs:9    props: symbol_kind=method, receiver=User, static=true
                                       --instantiates--> User
                                       --instantiates--> String
symbol  src.get_user    src/db.rs:17   props: symbol_kind=function, exported=true
                                       --instantiates--> User
dependency src -> crate::db::get_user  src/api.rs:3   props: source=internal
```

`static=true` distinguishes an associated function (`User::new`) from a method taking
`self` — they have different call sites and different impact when changed.

## Routes — Axum `.nest()` composed across files

An Axum application assembles its router from sub-routers, and the mount prefix lives in
a different file from the paths it applies to:

```rust
// src/router_builder.rs
fn build() -> Router {
    Router::new()
        .route("/", get(root))
        .nest("/api/v1/datasets", routers::datasets::router())
        .nest("/api/v1/search",   routers::search::router())
}
```

```rust
// src/routers/datasets.rs
pub fn router() -> Router {
    Router::new()
        .route("/",                  get(list).post(create))
        .route("/status",            get(status))
        .route("/{dataset_id}/data", get(data))
}
```

The composed paths are what get stored:

```
/                                      ← the root builder's own route, still bare
/api/v1/datasets                       ← the sub-router's "/" under its mount
/api/v1/datasets/status
/api/v1/datasets/{dataset_id}/data
/api/v1/search
```

and the bare `/status` is **gone** — it is not stored alongside the composed path, because
a route that is not served at `/status` must not match a client calling `/status`.

Composition is a crate-wide fixpoint, so it accumulates through multiple levels:

```rust
// build() nests "/api/v1/activity" → activity::router()
// activity::router() nests "/spans" → spans::router()
// spans::router() declares "/{id}"
→ /api/v1/activity/agents
→ /api/v1/activity/spans/{id}
```

### Per-route middleware is transparent

```rust
.route("/plain",   get(handler))
.route("/layered", get(handler).layer(Extension(state)))   // still a GET /layered
.route("/both",    get(list).post(create).layer(mw))       // both verbs survive
```

A non-verb method in the chain — `.layer()`, `.route_layer()`, `.with_state()` — is a
decorator, not a terminator. Treating one as a terminator dropped the route entirely
rather than losing an annotation, with the verb sitting in plain sight.

### Graceful degradation, on purpose

```rust
fn build(mount: &str, r: Router) -> Router {
    Router::new().nest(mount, r)      // the mount is not a literal
}
```

The nested router's routes keep their **bare** paths (`/thing`) rather than being dropped.
A visible route at the wrong path is still findable and still shows up as an unresolved
client edge; a dropped route is invisible. The same rule covers `.route(path, svc)` with
a non-literal path — it emits nothing rather than guessing.

## Routes — the `#[utoipa::path]` attribute

`utoipa_axum` registers a handler with `routes!(find_team)`, which repeats the handler
but **not** its path. The path is written once, in the attribute on the handler itself:

```rust
#[utoipa::path(
    get,
    path = "/api/v1/teams/{team}",
    params(("team" = String, Path, description = "…")),
)]
pub async fn find_team(state: AppState) -> AppResult<Json<TeamResponse>> { … }
```

```
route  /api/v1/teams/{team}  src/controllers/team.rs:18  props: method=GET, framework=utoipa, handler=find_team
```

An API registered this way has no `.route("/path", …)` call to read, so before this was
extracted its endpoints were absent from the graph entirely — crates.io stored 8 routes
where it serves 59, and every call its Svelte frontend made to them resolved to nothing.

- `method(get, head)` emits **one fact per verb**.
- `context_path = "/api"` is folded onto the path.
- A non-literal path — a `const`, a `concat!` — emits **nothing** rather than a guess,
  per the degradation rule above.
- The attribute is read wherever it appears, including on a method in an `impl`; a
  handler inside a `#[cfg(test)]` module is not a served route and is skipped.

`utoipa_axum::OpenApiRouter::nest()` would make the attribute path relative to a mount
prefix. It is not composed yet: the attribute path is stored as written. The model for
closing that is the Axum `.nest()` fixpoint above, and it needs a repository that
actually uses it to be worth building.

## Complexity and loop metrics

Function and method symbols carry the standard props the performance analyzer reads:
`cyclomatic`, `loop_count`, `loop_depth`, `scaling_loop_depth`, `calls_in_loop`,
`calls_in_scaling_loop`, `recursive_self` and `performs_io`.

`for`, `while` and `loop` are the loops. A **bounded** one — a constant-trip `for`
over a literal range or array, or an infinite `loop {}` / `while true` — raises
`loop_depth` but not `scaling_loop_depth`: it adds no data-dependent factor, so a
call inside it is not an N+1. That distinction is what keeps an event loop from
reading as O(n) in a data size it has nothing to do with.

`benches/` counts as test code, alongside `tests/`. Cargo builds it as bench
targets, and a benchmark iterates by definition, so a loop there is the point rather
than a finding.

**What the analyzer does with these is Rust-specific**, and worth knowing before
reading a finding. An async runtime spells its in-memory primitives with the same
verbs other languages reserve for I/O: `send` and `recv` on a channel, `read` and
`write` on a buffer, `poll` on a future, `new` for construction. The generic
keyword gate treats those as per-iteration I/O and is wrong to; Rust therefore has
its own gate (`isExpensiveRustCall`) that asks for I/O SHAPE instead — a storage
fact, a callee the extractor flagged `performs_io`, a database or HTTP receiver
token, or a distinctive round-trip method such as `fetch_one` or `read_to_string`.
Measured on tokio, that is the difference between 42 high-severity call-in-loop
findings and 3.

## What is deliberately not extracted

- **Macro-generated routes and items.** Anything produced by a proc macro is not
  expanded. The one router macro this no longer costs you is `utoipa_axum`'s
  `routes!()`, because the path it registers is readable from the handler's attribute
  without expanding anything — see above. Any other macro that declares an API still
  exposes almost none of it, and the resulting route count looks like a bug rather than
  a boundary. Check how your routes are declared before reading a low number as one.
- **`.route(path, handler_value)`** — a handler passed as a value rather than through a
  verb builder. There is no method to infer, and a guessed one could false-match
  another repository's endpoint, so the route is skipped rather than invented.
- **Non-literal paths and mounts**, per the degradation rule above.
- **Trait dispatch.** A call through `dyn Trait` resolves to the trait method, not to
  every `impl`.
- **`cfg`-gated code** is parsed as written; enola does not evaluate feature flags, so all
  branches contribute facts.

---

Measured on real Rust repositories: [BENCHMARKS.md](../BENCHMARKS.md).
