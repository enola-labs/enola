# Python — what enola extracts

Parsed with tree-sitter. Detected by `pyproject.toml`, `setup.py`, `requirements.txt`,
`Pipfile`, `pytest.ini`, `mypy.ini`, `tox.ini` or `setup.cfg`, at the root or up to three
levels deep.

Fixtures: [`python_sample`](../../internal/engine/testdata/repos/python_sample/) ·
[`python_flask_sample`](../../internal/engine/testdata/repos/python_flask_sample/) ·
[`py_fastapi_multirepo`](../../internal/engine/testdata/repos/py_fastapi_multirepo/) ·
[`py_grpc_multirepo`](../../internal/engine/testdata/repos/py_grpc_multirepo/)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a package directory | one module per directory | `module` |
| `def`, `class`, `async def` | a symbol with `symbol_kind`, `exported`, `cyclomatic` | `symbol` |
| `import` / `from … import` | a dependency tagged `internal` / `external` / `stdlib` | `dependency` |
| a call inside a `for` / `while` | the callee in `calls_in_loop` / `calls_in_scaling_loop` | props |
| `@app.get("/x")`, `@router.post("/y")` | a server route **at its composed runtime path** | `route` |
| `app.include_router(r, prefix="/p")` | folds `/p` onto every route that router declares | — |
| `@app.route` / `Blueprint` (Flask) | one route per HTTP method the decorator lists | `route` |
| SQLAlchemy models | an entity with its table name | `storage` |
| a `.proto` service + generated stub | both sides of a gRPC edge | `route` |
| module-level statements | a `file_ref` carrying the call edges | `file_ref` |
| `tests/`, `test_*.py`, `conftest.py` | a reference-only `test_ref` | `test_ref` |

## Modules, symbols and calls

Modules are directories, so `app/handlers/sqlalchemy` is its own module rather than part
of `app`:

```
module  app
module  app/handlers
module  app/handlers/sqlalchemy
module  app/handlers/tools

symbol  app/api.handler        app/api.py:6
        props: symbol_kind=function, exported=true, cyclomatic=1
        --calls--> app/db.get_user
        --declares--> app
```

Loop props work as they do in [Go](go.md#loops-for-n1-hunting) — `app/db.get_path` carries
`loop_count=1, loop_depth=1, calls_in_scaling_loop=[app/db.get_user]`, which is the N+1
candidate set.

Python also recognises a loop fixed by name (`for p in RESERVED_PREFIXES`, `for s in
DagRunState`, `range(MAX_RETRIES)`), an inner loop reached through the outer element
(`for dag in dags: for task in dag.tasks`), and a batch loop (`for chunk in chunked(ids,
500)`, a three-argument `range`, a `while` that fetches a page and drains it). The rules
are in [the index](README.md#loops-what-counts-as-nesting).

### Module-level code is not lost

Python does real work at import time, and those calls belong to no function. They are
recorded on a `file_ref` — an edge carrier that is not itself architecture:

```
file_ref  app/mcp_server.py   --calls--> app/mcp_server.health_check
                              --calls--> app/mcp_server.list_users_tool
                              --calls--> app/mcp_server.quickstart_prompt
```

Without this, an MCP tool registered by a decorator at module scope has no caller and
reads as dead code.

### Test references, and the one gap

`tests/**`, `test_*.py` and `conftest.py` are ignored for indexing and recovered as
`test_ref`:

```
test_ref  tests/test_app.py   --calls--> app/api.handler
                              --calls--> app/tested_only.verify_checksum
```

> **Stated limit.** Python has no `TestRefExtractor` implementation yet — the globs are
> configured and the recovery path exists, but until it is implemented a Python symbol
> called *only* from a test reads as unreferenced. Expect dead-code false positives on
> Python repositories to be higher than on Go, Ruby or TypeScript. This is written down
> in [`internal/config/config.go`](../../internal/config/config.go) next to the globs
> themselves rather than discovered later.

## Routes — FastAPI router factories and `include_router`

The dominant FastAPI idiom builds the router *inside* a function and nests the handlers
as inner `def`s. Walking module-level statements never reaches those decorators, so a
module-level walk finds no routes in this file at all:

```python
# api/routers/search.py — the paths here are leaves
def get_search_router() -> APIRouter:
    router = APIRouter()

    @router.get("/results")
    async def results():
        return []

    @router.post("/")
    async def search(payload: dict):
        return {}

    return router
```

```python
# api/client.py — the mount is in a different file
app = FastAPI()
app.include_router(get_search_router(), prefix="/api/v1/search", tags=["search"])
```

The composed paths are what get stored:

```
route  /api/v1/search/results   api/routers/search.py:12   props: framework=fastapi, method=GET
route  /api/v1/search           api/routers/search.py:16   props: framework=fastapi, method=POST
```

Note the second one: a router's `"/"` route under a `/api/v1/search` mount is
`/api/v1/search`, not `/api/v1/search/`. Prefix folding runs repo-wide, so the factory
and the mount can live anywhere.

A TypeScript client in the same cluster then resolves against those paths:

```
route  /api/v1/search/results   web/src/api.ts:5    props: role=client, framework=fetch
route  /api/v1/search           web/src/api.ts:17   props: role=client, framework=axios

service acme  props: edge_coverage=[{edge_type: http_client, detected: 2, resolved: 2, unresolved: 0}]
```

## Routes — Flask

```python
@app.route("/users", methods=["GET", "POST"])
def users(): ...

@app.route("/health")
def health(): ...
```

```
route  /users    app.py:16   props: framework=flask, method=GET,  handler=app.users
route  /users    app.py:16   props: framework=flask, method=POST, handler=app.users
route  /health   app.py:22   props: framework=flask, method=GET,  handler=app.health
```

One fact per method, not one per decorator — because `GET /users` and `POST /users` are
different endpoints with different clients, and an unused-route finding on one must not
be masked by traffic on the other. Blueprints, `MethodView` classes and their registered
prefixes are handled the same way (`views.HealthView.version`, `views.ping`).

## gRPC

```
route  /users.v1.UserService/GetUser   proto/users/v1/users.proto   props: role=server, source=grpc-proto
route  /users.v1.UserService/GetUser   client.py                    props: role=client, source=py-grpc-client
dependency client -> server            props: type=cross_repo, via=[grpc], confidence=verified
```

`confidence=verified`, because both sides are matched through a declared contract rather
than a string comparison.

## What is deliberately not extracted

- **Routes whose path is not a literal.** `@app.get(PREFIX + path_var)` is skipped rather
  than guessed.
- **Dynamic mounts.** `include_router` with a computed prefix leaves the sub-router's
  routes at their bare paths rather than dropping them, so they stay visible and show up
  as unresolved client edges.
- **Runtime registration.** Routes added by a plugin loader or a metaclass at import time
  are not traced.
- **Duck typing and `getattr` dispatch.** A call through `getattr(obj, name)` resolves to
  nothing rather than to every candidate.

---

Measured on real Python repositories: [BENCHMARKS.md](../BENCHMARKS.md).
