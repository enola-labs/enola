# Cross-repo resolution, and its limits

Two services. The `web` service calls the `api` service. Nothing in either repository
says so — the link only exists if a tool can work it out.

Run it:

```bash
./run.sh
```

## What is hard about this

`api/server.go` registers its routes like this:

```go
func main() {
    v2 := r.PathPrefix("/api/v2").Subrouter()
    registerOrders(v2)                       // the prefix is passed, not written
}

func registerOrders(r *mux.Router) {
    r.HandleFunc("/orders/{id}", getOrder)   // the leaf path, alone
}
```

Read `registerOrders` on its own and the service appears to serve `/orders/{id}`. It
doesn't. It serves **`/api/v2/orders/{id}`**, and the client calls that path.

The prefix and the leaf path are never written together. Neither function contains the
answer, so recovering it means composing the prefix **across the call boundary** —
following the subrouter into the function it was handed to. Match on the bare path and
the client's call resolves to nothing; match on the wrong composed path and it resolves
to something wrong, which is worse.

The same shape appears in Axum's `.nest()`, FastAPI's `include_router(prefix=…)`, Rails'
`scope`/`namespace`, and Swift endpoint enums whose version prefix lives in a protocol
extension several files away.

## What you should see

```
  service  classification  detected  resolved  unresolved
  api      isolated              0         0           0
  web      connected             3         2           1
```

Three outbound calls detected in `web`. Two resolved to `api` — at the composed path.
And **one did not**, on purpose.

## The one that doesn't resolve, and why that matters

`web/client.go` also contains this:

```go
func fetchDynamic(tenant string) {
    http.Get(apiBase + "/api/v2/" + tenant + "/orders")
}
```

The tenant is a value enola cannot see, so the call is recorded as
`/api/v2/{}/orders`. No route is served at that shape, so enola reports the call as
**unresolved** rather than guessing at one.

That is deliberate, and it is the more important half of this example. A missing edge
is visible — it shows up in the unresolved count, and you can go look. A *wrong* edge is
invisible and gets acted on: an impact analysis that quietly includes a dependency that
does not exist is worse than one that admits it doesn't know.

## What enola does not resolve

Stated here rather than discovered later:

- **Dynamic or interpolated paths** — as above. Detected, reported unresolved, never guessed.
- **Cross-file mount resolution** — a subrouter mounted in one file and populated in
  another *package* is not followed. Composition works across function and package
  boundaries within a module-wide pass; a mount whose prefix cannot be traced to a
  literal is left at its bare path rather than composed to a plausible one.
- **Bare catch-alls** — `app.get('*')` and equivalents are skipped. A SPA fallback is not
  an endpoint, and treating it as one would match any client path in any repository.
- **Absolute URLs to hosts that are not loaded repositories** — reported as external
  rather than unresolved, because a third-party API is not a blind spot.

In every case the choice is the same: **report the gap instead of inventing an edge.**
`enola coverage` exists so those gaps are countable rather than hidden — see
[docs/CLI.md](../../docs/CLI.md).

---

The same walkthrough in prose, with the output of every step explained: **[docs/CLUSTERS.md](../../docs/CLUSTERS.md)**.
