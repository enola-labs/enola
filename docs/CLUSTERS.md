# Two services, one graph

*How enola matches client calls to server routes across repositories and reports calls
it cannot resolve.*

Within one repository, enola resolves relationships against one source tree. Across
services, the evidence is split: `web` contains the client call, `api` contains the
registered route, and neither declares the dependency directly.

This guide uses two services small enough to read. They are
[`examples/cross-repo/`](../examples/cross-repo/), and
[`examples/cross-repo/run.sh`](../examples/cross-repo/run.sh) runs it.

---

## 1. Name the cluster

One config, listing repositories rather than describing one:

```yaml
# cluster.yaml — paths resolve relative to THIS file, so the config means the
# same thing wherever it is run from.
repos:
  - api
  - web
```

For a folder whose subfolders are the repositories, `enola cluster init <folder>` writes
this file for you.

Pass this config wherever a command accepts a repository: a directory selects one
repository, while a config file can select several. The exception is a folder whose
immediate subfolders include two or more git repositories. `--generate` and the MCP
`generate_snapshot` tool index it as a cluster without being asked: they say they
detected the repositories, write `cluster.yaml` there (or use the one already there),
and link them. Settings from the config in force still apply to that run. Pass
`--no-cluster` (`no_cluster=true` over MCP) to index the folder as one repository. A
folder that is itself a git repository is not treated this way, since nested checkouts
belong to it; it is indexed as one repository with a warning naming the command that
makes a cluster, as every other command does.

```
$ enola --generate cluster.yaml

Snapshot complete:
  Repositories: 2
    - ./api
    - ./web
  Facts:       20
  Insights:    3
  Artifacts:   1
  Duration:    14.396792ms
  Output:      ./web/.enola
```

The command prints the last repository's output directory, but every repository receives
the same complete linked graph. Reading either `api/.enola` or `web/.enola` returns the
whole cluster.

## 2. How the edge is resolved

Here is the route the `api` service registers:

```go
func main() {
	r := mux.NewRouter()
	v2 := r.PathPrefix("/api/v2").Subrouter()
	registerOrders(v2)                        // the prefix is passed, not written
	http.ListenAndServe(":8080", r)
}

func registerOrders(r *mux.Router) {
	r.HandleFunc("/orders/{id}", getOrder).Methods("GET")
}
```

Read `registerOrders` on its own and the service appears to serve `/orders/{id}`. It does
not. It serves **`/api/v2/orders/{id}`**, which is what the client calls.

The prefix and leaf path are never written together. Enola recovers the served path by
composing them across the call boundary. Using only the leaf path would miss the client;
composing it incorrectly would create a false edge.

So the routes are stored at the path they are actually served at:

```
route  /api/v2/orders       api/server.go   handler=listOrders  matched_by_clients=true
route  /api/v2/orders/{id}  api/server.go   handler=getOrder    matched_by_clients=true
```

and the edge exists:

```
dependency  web -> api   type=cross_repo  confidence=verified  via=[http-client]
                         endpoints=[GET /api/v2/orders, GET /api/v2/orders/{id}]
```

The same shape appears in Axum's `.nest()`, FastAPI's `include_router(prefix=…)`, Rails'
`scope`/`namespace`, and Swift endpoint enums whose version prefix lives in a protocol
extension several files away. [EXTENDING.md](EXTENDING.md) covers teaching enola a
connection it does not already know.

## 3. Ask what it missed

A resolved-edge count alone does not distinguish a service with no outbound calls from
one whose calls enola could not follow. `coverage` reports both cases per service:

```
$ enola coverage cluster.yaml
Cross-repo edge coverage

  service  classification  detected  resolved  unresolved
  api      isolated              0         0           0
  web      connected             3         2           1

Unresolved outbound call sites (1 across 1 service):
  web                          http_client ×1

Each is a call enola detected but could not point at a loaded service — either a
repository you have not snapshotted, a third-party endpoint, or a blind spot in
enola's extraction. Load the missing repository and re-run to tell them apart.
```

`api` is **isolated** — it detected no outbound calls, which for a service that only
serves is correct. `web` is **connected**, with one of its three call sites unresolved.

That unresolved one is deliberate:

```go
func fetchDynamic(tenant string) {
	http.Get(apiBase + "/api/v2/" + tenant + "/orders")
}
```

The path is assembled at runtime from a value enola cannot see, so there is no complete
path to match against a route. enola records the outbound call and reports it unresolved
rather than guessing. Reporting unresolved calls gives the resolved count context.
`coverage` exits `0` because it is a report, not a gate.

Three things put an entry on that list, and they need different responses:

| Cause | What to do |
|---|---|
| A repository you have not loaded | Add it to `repos:` and re-run — the count moves |
| A genuinely third-party endpoint | Nothing; it is correct |
| A blind spot in enola's extraction | [BLIND-SPOTS.md](BLIND-SPOTS.md) records how these are found, including one found in enola itself |
| Calls through your own HTTP wrapper | Declare the client under `clients:`, see [EXTENDING.md](EXTENDING.md#teaching-enola-your-http-client); worked through in [examples/custom-client](../examples/custom-client/README.md) |

If you suspect a missing service, add its repository and re-run. A resolved call confirms
that the service was outside the original graph.

## 4. Grade a change across the seam

Everything in [FIRST-CHANGE.md](FIRST-CHANGE.md) works here, with the cluster config in
place of the repository path:

```bash
enola baseline pin cluster.yaml     # every member must agree on one union
#   …change the api service's routes…
enola check cluster.yaml
```

Rename a route in `api` and the verdict can name the client in `web` that called it. This
can catch a cross-repository mismatch while each repository's tests still pass.
`enola endpoint 'GET /api/v2/orders' cluster.yaml` asks the same question directly,
without a baseline.

The `unused-routes` and `crossrepo` explainers require a cluster. `unused-routes` reports
backend routes that no loaded client calls; `crossrepo` reports the seams themselves.
Treat unused-route findings cautiously when resolution coverage is low: an unresolved
client call may belong to a route currently reported as unused.

---

## What is deliberately not linked

enola links what it can *demonstrate*. Two services that share a type name are not
thereby coupled: a shared name with no import and no call becomes a symmetric
`cross_repo_shared_code` observation with no direction, verified against the declaring
files rather than the names alone. It is reported as coupling to inspect, not as a
directional dependency.

The full rules — what links, what does not, and how to tune it — are in
[ARCHITECTURE.md → Cross-repo](../ARCHITECTURE.md#cross-repo-the-graph-of-graphs) and
[EXTENDING.md](EXTENDING.md).

---

## Where to go next

| If you want | Read |
|---|---|
| The single-repository loop first | [FIRST-CHANGE.md](FIRST-CHANGE.md) |
| Coverage, scale and precision measured on public repositories | [BENCHMARKS.md](BENCHMARKS.md) |
| To teach enola a link it does not know | [EXTENDING.md](EXTENDING.md) |
| To make enola read calls through your in-house HTTP client | [examples/custom-client](../examples/custom-client/README.md) |
| Declaring seams instead of inferring them | [INTENT.md](INTENT.md) |
| Every flag `coverage` takes | [CLI.md](CLI.md#command-line-reference) |
