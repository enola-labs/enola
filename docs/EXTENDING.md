# Teaching enola a new connection

Most of what enola knows about a codebase comes from parsing one file at a time. This page
is about the other part: the edges that only exist once everything is in one store — a
route in a `.proto` and the Go method serving it, a call site in one repository and the
endpoint answering it in another.

There are three ways to add one, and picking the wrong one is the main way this goes badly.
So start here:

| You want to… | Use | Lives in |
|---|---|---|
| Parse a language or framework enola cannot read | **Extractor** | `internal/extractors/<lang>/` |
| Connect facts *within* one repository that no single extractor could see | **Binder** | `internal/linkers/binders/<name>/` |
| Establish that one repository depends on another | **Cross-repo signal** | `internal/linkers/crossrepo/signals/<name>/` |
| Make enola read calls through your own HTTP wrapper | **Nothing, it's config** | `clients:` in `enola.yaml` |
| Stop a wrong edge, or teach enola a framework's boilerplate | **Nothing — it's config** | `linking:` in `enola.yaml` |

That last row is not a consolation prize. Most accuracy problems are vocabulary problems,
and vocabulary is data — see [Tuning without code](#tuning-without-code).

Extractors have their own page: [what each one extracts](extraction/README.md). This page
covers the other two, and the config.

---

## The rule that governs all of it

> **A missing edge beats a wrong one.**

This is not a style preference, it is the reason the design holds together. A missing edge
is *visible*: it shows up in `enola coverage` as an unresolved count, and a human can go
look at it. A wrong edge is *invisible* and gets acted on — an `impact_analysis` that
silently includes a dependency that does not exist is worse than one that admits it does
not know.

Every gate in the existing signals is there because someone found a real false positive.
When you are unsure whether to draw an edge, don't.

---

## Adding a binder

A binder resolves references **within** an assembled fact set. Use one when the two ends of
an edge are found by different extractors that never see each other's output.

The worked example is gRPC. The `.proto` says a service has a `GetUser` RPC; a Go file says
a struct embeds `UnimplementedUserServiceServer` and has a `GetUser` method. Neither
extractor can see the other, and the thing tying them together — a code generator's naming
convention — is only visible once both are in the store.

```go
type Binder interface {
	Name() string
	Stage() BindStage
	Bind(ctx context.Context, store *facts.Store) error
}
```

### The contract

- **Add only.** You may add or update relations and props on facts that already exist. You
  may not add or remove facts: that would make the graph depend on which binders were
  enabled, and two snapshots taken with different sets would stop being comparable.
- **Idempotent.** Every binder re-runs on every snapshot and every append. Check for the
  relation before appending it — `fact.HasRelation(kind, target)` exists for this.
- **Deterministic, and independent of registration order.** Binders in the same stage run
  in an unspecified order and must not observe each other. `facts.jsonl` is hashed into the
  snapshot ID, so a binder whose output depended on ordering would make an unchanged tree
  produce a different snapshot from run to run.

That last one is enforced, not merely requested:
`TestBinders_OutputIsIndependentOfRegistrationOrder` serializes the whole store after
running every binder forward and reversed, and requires the bytes to match.

### Choosing a stage

```go
StagePreLink   // before cross-repo linking
StagePostLink  // after it, before the graph index is built
```

**`StagePostLink` unless you have a reason.** It is the more constrained choice, and it is
right for anything that resolves references inside one repository — which is most binders.

Use `StagePreLink` only when your binder changes *what the cross-repo linker will match
on*. Exactly one does today: the Python gRPC extractor can only see a short service name
(`SttService`), while the wire path is `/vosk.stt.v1.SttService/StreamingRecognize`. The
fully qualified name lives in the `.proto`, which a different extractor read. If that
rewrite happened after linking, the linker would match the un-rewritten name and the whole
gRPC dependency would silently vanish.

### Prefer a table to a branch

`grpcimpl` recognizes generated base types through an exported table:

```go
var DefaultServerBases = []ServerBase{
	{Generator: "protoc-gen-go-grpc", Pattern: regexp.MustCompile(`^(?:.*\.)?Unimplemented(.+)Server$`)},
	{Generator: "grpc_python_out",    Pattern: regexp.MustCompile(`^(?:.*\.)?(.+)Servicer$`)},
}
```

Supporting another gRPC code generator is a row, not a code change —
`TestServerBases_AreExtensible` proves it by binding a fabricated convention through
`NewWith` without touching the binder. If your binder encodes per-technology knowledge,
make it a table for the same reason.

### Key on structure, not on language

`httphandler` binds a route to the symbol serving it. It used to test `language == "go"`,
which made Go support a property of the *engine* rather than of the Go extractor: adding
TypeScript meant editing a binder no TypeScript code knows about.

It now keys on a prop the extractor sets — `http_handler`, which goextractor puts on any
func whose signature is exactly `func(http.ResponseWriter, *http.Request)`. A language with
no handler-shaped signature emits the prop nowhere, indexes nothing, and binds nothing:
the same outcome the language gate produced, except that a new extractor can opt in
without this file changing.

If you find yourself writing a language name into a binder, look for the structural fact
underneath it.

### Registering a binder

```go
// pkg/bootstrap/bootstrap.go
eng.RegisterBinder(mybinder.New())
```

---

## Adding a cross-repo signal

A signal derives evidence that one repository depends on another. Four ship today: HTTP
route matching, imports, Kafka topic ownership, and shared code.

A signal never builds facts. It reads the multi-repo fact set through `SignalInput` and
reports what it found to an `EvidenceSink`; the core turns accumulated evidence into
`service` nodes and dependency facts. That split is the whole point — it is what lets you
add a signal without touching the code that materializes the graph.

```go
type CrossRepoSignal interface {
	Name() string
	Phase() SignalPhase
	Contribute(in SignalInput, out EvidenceSink)
}
```

### Directional or symmetric

```go
PhaseDirectional  // one repo calls, imports, or consumes from another
PhaseSymmetric    // a relationship with no inherent direction
```

Directional signals run first and cannot observe each other, which is what makes their
order irrelevant. Symmetric signals run afterwards and *may* read the accumulated
evidence — because the only honest way to orient a symmetric finding is to defer to a
direction something else established.

Shared code is the symmetric one. Two repositories declaring the same types says nothing
about which depends on which. So it annotates an edge a directional signal already drew;
and when none exists, it records a **coupling** — queryable evidence that carries no
relation and never enters the traversable graph. That distinction matters more than it
looks: `depends_on` composes across hops, and shared code does not. A repo calling one side
of a copy-paste pair does not thereby reach the other.

```go
pairs, directional := out.DirectedPairs(a, b)
if !directional {
	c := out.Coupling(a, b)   // no relation, stays out of the graph
	c.Via("shared_symbols")
	c.Sample(plugin.BucketSymbols, id)
	return
}
for _, p := range pairs {
	out.Edge(p[0], p[1]).Via("shared_symbols")   // annotate the established direction
}
```

### Reading: `SignalInput`

```go
Facts() []facts.Fact                          // the whole multi-repo fact set
Repos() []string                              // every loaded repo label, sorted
ResolveRepo(candidate string) (string, bool)  // an @scope or topic prefix → a repo label
PrimaryLanguage(repo string) string
TopDirs(repo string) map[string]bool          // the repo's own top-level source dirs
OwnScopes(repo string) map[string]bool        // npm @scopes the repo publishes
ModuleNames(repo string) []string             // longest first
HasSource() bool
ReadSource(f facts.Fact) (string, bool)
```

Every derived accessor is computed lazily and at most once per snapshot, so needing the
same index as another signal costs nothing. Do not re-walk `Facts()` to rebuild one of
these; add an accessor if what you need is missing.

**`HasSource()` and `ReadSource()` are not the same question.** `HasSource()` false means
no source is available at all, so verification cannot run and must be skipped. A failed
`ReadSource` means *this one file* could not be read, so *this one candidate* is
unverifiable. Collapsing them turns every name-matched candidate into a rejected one the
moment source is unavailable — which is how the shared-code signal briefly stopped
producing anything at all.

### Writing: `EvidenceSink`

```go
Edge(consumer, provider string) EdgeEvidence
Coupling(a, b string) CouplingEvidence
Coverage(repo, edgeType string) *Coverage
DirectedPairs(a, b string) (pairs [][2]string, ok bool)
```

```go
e := out.Edge("web", "api")
e.Via("http-client")                                  // HOW it was observed
e.Confidence("verified")                              // strongest value wins
e.Sample(plugin.BucketEndpoints, "GET /v1/items")     // duplicates collapse
```

### Buckets: declaring your own evidence

A `Bucket` names one class of evidence and the props it materializes into:

```go
type Bucket struct {
	Name           string  // identity, and the sort key that makes output deterministic
	CountProp      string  // receives the number of distinct samples
	SamplesProp    string  // receives the samples, sorted and capped
	UnverifiedProp string  // optional: the PRE-verification tally, written only when
	                       // it exceeds the number that survived
}
```

The four built-ins are `BucketEndpoints`, `BucketImports`, `BucketTopics`, `BucketSymbols`.
A new signal declares its own and reports into it — materialization is a loop and knows
nothing about any particular signal.

The prop names are a **public contract**: they are what `query_facts(kind=dependency)`
returns. `TestMaterialize_PropSurfaceIsStable` pins the exact set, so a typo in a `Bucket`
field fails the build rather than silently renaming a field somebody queries.

`UnverifiedProp` is how a signal reports the gap between *matched by name* and *confirmed*.
Shared code uses it: on a real repository pair, 39 type names matched but only 23 survived
comparing the files behind them. Reporting only the 23 hides that the filter did work;
reporting both always would be noise, so it appears only when the two differ.

### Coverage: making your misses visible

```go
out.Coverage(repo, "http_client").Detected++
// …later, if it resolved:
out.Coverage(repo, "http_client").Resolved++
```

The difference is the blind spot — call sites enola saw but could not attribute. A
repository with no outbound edges but a non-zero unresolved count is a *coverage gap*, not
an isolate, and `coverage_report` says so. If your signal can fail to resolve something it
detected, count both. This is the mechanism that makes "a missing edge is visible" true.

### Registering a cross-repo signal

```go
// pkg/bootstrap/bootstrap.go
eng.RegisterCrossRepoSignal(mysignal.New(linkVocab))
```

Take the vocabulary at construction if your signal consults one — never read it from a
global, or two snapshots taken under different vocabularies would interfere.

---

## Tuning without code

Before writing a signal to fix a wrong edge, check whether it is a vocabulary problem.
Nearly every accuracy fix in this area has been one more string in one more list: Rails
base classes, UI-component words, generated-code path markers.

Those lists live in [`internal/linkers/vocab`](../internal/linkers/vocab/) and are
overlaid from `enola.yaml`:

```yaml
linking:
  framework_conventions:
    add: [BaseViewController, AppDelegate]   # boilerplate every app of this framework declares
  generic_path_segments:
    remove: [status]                         # we really do serve a meaningful /status
  non_contract_paths:
    add: ["/generated/"]
  thresholds:
    min_shared_symbols: 5
```

Three things to know:

- **Lists are add/remove, never replace.** A replacing list would let one addition silently
  discard every default — and nothing would fail, because a thinner vocabulary just draws
  *more* edges.
- **A bad threshold is an error, not a clamp.** Zero or out-of-range is rejected with every
  problem reported at once. Silently correcting it would leave you believing a setting took
  effect that did not.
- **Changing this changes the snapshot ID.** The vocabulary decides which edges get drawn,
  so it is folded into the config hash. Two snapshots taken under different vocabularies
  are not comparable, and the receipt says so rather than pretending otherwise.

What you cannot express here is a general matching *rule*. That is deliberate: a config
language able to describe how to match would let you manufacture an edge, and every fact in the
graph is supposed to be derived rather than asserted. Widening what counts as "too generic to
link on" can only ever *remove* edges, the safe direction.

Two settings are the exception, and both are bounded so that they choose among evidence rather
than create it. `linking.match_literal_against_params`, off by default, lets a server path
parameter stand in for a literal client segment under strict conditions. `service_aliases`, a
top-level key, names the repository behind a service name a declared client passes, and only
ever picks among repositories that already serve the called path. Both change the snapshot ID
like everything else here. See [Teaching enola your HTTP client](#teaching-enola-your-http-client).

## Teaching enola your HTTP client

enola recognises the HTTP clients it ships knowledge of: `fetch`, axios, Angular's
`HttpClient`, Spring's `RestTemplate` and the rest listed per language in
[extraction/](extraction/README.md). A company's own wrapper is none of them. When every call
goes through something like `httpRequestService.sendRequest(serviceName, path, options)`,
nothing in the name or signature says HTTP, the calls are invisible, and a service that plainly
depends on another reads as isolated. That needs no extractor. Declare the client:

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
        verb_option: method
        default_verb: GET

service_aliases:
  resource-api: gateway
```

| Key | Meaning |
|---|---|
| `name` | Stamped on every route the client produces (`client_spec`) and on its account. |
| `language` | The extractor that reads it. `typescript` today (`ts`, `javascript` and `js` mean the same). A spec for a language enola indexes but has no client reader for yet, such as `python` or `go`, is skipped with a warning at load, in `enola coverage` and in the MCP snapshot summary; the rest of the config still runs. Anything else is a config error. |
| `receiver_types` | The declared type of the member a call is made on, by constructor parameter or `inject()` field. The type decides: the same method on any other type is not read. |
| `methods[].name` | A method that makes a request. |
| `path_arg` | Required. The argument holding the path. |
| `service_arg` | The argument naming the target service. Its value becomes the route's `target_hint`. |
| `options_arg`, `verb_option` | The object literal the verb is read from, and its property (`method` by default). A quoted verb, or an identifier or enum member named for one (`GET`, `HttpMethod.POST`), is read. |
| `default_verb` | The verb when the call states none. Without one the route carries no verb and matches whichever verb serves the path. |
| `service_aliases` | Service name to repository label, for when the two differ. |

**What this is, and is not.** A client declaration teaches an extractor to *read* a call site.
It never declares an edge: the call becomes a client route like any other, and the linker still
draws an edge only when a loaded repository serves that path. A wrong declaration can at worst
produce routes that match nothing, and those are visible (below). An alias is the one part that
chooses. It applies only to routes a declared client read, it picks only among repositories that
already serve the path, and it is a constraint: if the aliased repository serves none of them, no
edge is drawn at all, and the call reports `unmatched_reason: alias_not_serving`.

**How a path is read.** `this.<field>` resolves against the enclosing class's own
fields, a local bound once folds to its value, and template literals and `+`
concatenations are joined. An operand that resolves to nothing becomes the `{}` path
parameter, unless it leads the path: then the prefix is unknown and the call produces no route.
The details are in [extraction/typescript.md](extraction/typescript.md#declared-in-house-clients).

**When a path parameter must absorb a literal.** A call to `/v1/resources/catalog/items`
against a server serving `/v1/resources/:type/items` does not match exactly. Turn on:

```yaml
linking:
  match_literal_against_params: true
```

It is tried only when no exact match exists. The whole server path must be matched, at least two
literal segments must agree, a parameter may not lead the server path, a client placeholder never
matches a server literal, and when more than one server path pattern fits, nothing matches.
Endpoints matched this way are listed in `param_segment_endpoints` on the dependency.
An edge's `confidence` is the strongest among its endpoints, so that list is how to
weigh the looser ones.

**Checking that a declaration did anything.** Every declared client gets an account per
repository, an `extraction` fact named `typescript:client:<name>`: its
receivers, the calls made through it, the routes they became, and the calls skipped by cause
(`dynamic_path`, `missing_path_argument`, `non_route_path`).
`enola coverage` prints them as a *Declared clients* table. A client that matched no
call site in any loaded repository is reported as a finding that names the likely cause: no
receivers means `receiver_types` is wrong, receivers without call sites means the
method names are.

A worked example you can run end to end: [examples/custom-client](../examples/custom-client/README.md).

---

## The prop vocabulary is a contract

An extractor writes a prop value; a linker in another package reads it back and branches on
it. Those values are a contract between two packages that never reference each other, and
they live in [`internal/facts/contract.go`](../internal/facts/contract.go).

If your extractor emits a route, register its `source` there and decide one thing: is it a
**hand-written call site** (someone wrote this request) or **contract-derived** (read from
a spec or IDL)? Hand-written sources go in `HandWrittenClientSources` and link as
`via: "http-client"`; the rest link as `via: "http"`.

This is enforced. `TestContract_NoHandWrittenContractLiterals` fails the build if any file
in `extractors/`, `linkers/` or `engine/` spells one of these values out instead of using
the constant — readers included, since a linker comparing against a literal is the same bug
from the other side.

It is enforced because it happened. The linker kept a private copy of the hand-written set
that had never included the Java extractor's two values, so every `RestTemplate` and
`@FeignClient` call site linked as a generic `via: "http"` for as long as that extractor had
existed. Nothing failed, because nothing tied the reading side to the writing side.

> **Watch the `Kind`.** The `source` prop carries two unrelated vocabularies: route
> provenance (`ts-http-client`, `grpc-proto`, …) on `KindRoute`, and dependency origin
> (`internal` / `external` / `stdlib`) on `KindDependency`. Reading it without checking
> `Kind` first gets you a value from the wrong one.

### Props an explainer reads

Routes are not the only contract. Three module/symbol props are read outside the
package that writes them, and they are **tables rather than branches**, so teaching
another language is a row:

- **`CompilationUnitProps`** (`crate`, `project`, `jvm_module`, `pub_package`) names
  the build unit a module compiles into. The cycles explainer uses it to tell a
  build-order defect from a cycle that is merely internal to one unit — MSBuild and
  Cargo both forbid cycles *between* units, so a cycle found in C# or Rust is
  necessarily within one. A prop belongs here only when several module facts can
  share a value: Swift's `spm_target` is deliberately absent, because swiftextractor
  names each module fact *by* its target and two never share one.

  The languages differ in how much the prop buys, and Dart is the extreme. Scala
  imposes no package-level acyclicity within an sbt module, so a cycle there is legal;
  **Dart does not even forbid circular imports between libraries** — they are legal,
  they compile, and they are ordinary — so a Dart cycle is never a build-order defect
  at all. Registering the prop is what stops the explainer telling a Flutter author
  their mutual import "can cause initialization issues".
- **`data_holder`** marks a type that declares state and no behaviour. The
  package-metrics explainer spares such packages its "extract
  interfaces" advice, and recognises a dedicated construct (`data_class`, `record`)
  where one exists. Emit `data_holder` when your language has none in common use —
  C# writes its DTOs as plain classes with auto-properties. Scala emits it for a
  `case class` even though that *is* a dedicated construct: the reading side knows
  only the three key strings, so a fourth would need a change on both sides to say
  something the generic marker already says.
- **`abstract`** decides whether a type counts toward package-metrics abstractness,
  and it is **authoritative** — it can demote as well as promote. Set it when your
  language's interface-kind construct is not reliably an abstraction: a Ruby module
  is a mixin *or* a namespace, and a Scala trait routinely carries its whole
  implementation, so both languages compute it (from the members present) rather
  than letting the keyword decide. Leave it unset where an interface always is one,
  as in Go — absence means "abstract", so emitting it only when true is the same as
  not emitting it at all.

An explainer in another module cannot import `internal/facts`, so it mirrors these key
strings locally. That is the same arrangement the route `source` values have, and it
carries the same hazard: a prop renamed on one side goes silently unread on the
other.

---

## The shape of a fact is a contract too

The prop *values* above are not the only thing a reader in another package depends on.
Two structural conventions are load-bearing, undocumented until an extractor broke both,
and they fail in the worst possible way: **silently, by producing degenerate output
rather than wrong output**, so every test stays green and the tool merely reports
nothing interesting.

**A dependency fact is named `"<importer> -> <imported>"`.**

```go
facts.Fact{
    Kind:      facts.KindDependency,
    Name:      pkgDir + " -> " + target,   // NOT just target
    Relations: []facts.Relation{{Kind: facts.RelImports, Target: target}},
}
```

The relation carries the imported side; the *name* is the only place the **importing**
side survives, and the package-metrics explainer recovers it by splitting on
`" -> "`. Name a dependency by its target alone and every one of your language's edges
is invisible there — efferent coupling comes out 0 for every package, average
instability 0.00, and the most depended-upon package of a real application is whatever
generated directory happened to have three importers.

**A symbol carries a `declares` relation to its module.**

```go
Relations: []facts.Relation{
    {Kind: facts.RelDeclares, Target: pkgDir},
    // …anything else this symbol declares, in any order
}
```

That same explainer attributes a symbol to a package through this edge. Emit it and
your language gets package metrics; omit it and every symbol falls out of the
population.

**Order does not matter, and that is a repair rather than a courtesy.** The explainer
used to take the first `declares` target and stop, which quietly assumed a symbol
declares nothing but its own module — true of Go, false of any language whose *types*
declare their own members, which is natural and which Dart does. A method name was then
read as a package name, minting one phantom package per class: 1,746 of them on one
repository against 199 real modules, the same failure the exported-surface explainer
documents for .NET arrived at from a different direction. It now resolves the target
against the known module set, so a member edge arriving first cannot be mistaken for a
package. Prefer emitting the module edge first anyway — it reads better and matches Go —
but nothing breaks if you do not.

Neither convention is checked by the golden tests, because both produce perfectly
well-formed facts. The way to catch this class of mistake is to run the analyzers
over a real repository in your language and ask whether the numbers are *plausible*, not
merely present: a metric that is uniformly `0.00`, or a "most depended-upon package"
naming a generated directory, is the shape of a contract that was never met.

---

## Before you open the PR

- **Goldens unchanged**, or changed deliberately with the diff reviewed:
  `go test ./internal/engine -run TestGolden`. Adding a *new* fixture is encouraged;
  changing an existing golden needs a reason.
- **Make your guard fail.** Break the thing your test protects and watch it report the
  failure. A test that has never failed is not a guard — two of the tests in this area were
  written, passed, and turned out to assert nothing.
- **Determinism.** Same input, same bytes. If you added a signal or binder, the
  order-independence tests already cover you; if you added a map iteration anywhere near
  output, sort it.
- **`cacheVersion`.** An *extractor* change needs a bump in `internal/engine/cache.go` plus
  an entry in `internal/cachecov`. Binders, signals and linkers sit outside the extractor
  cache and need neither.
