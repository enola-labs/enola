# The graph

Everything enola does starts from one thing: a graph of your software system, built by parsing source code. The checks, the dashboard, the MCP tools and `enola check` all read it. So can your own tools.

This page covers what is in the graph, how it is built, what it writes to disk, and how to consume it. It is the overview; each section links to the page that owns the detail.

## What's in the graph

The graph is made of **facts**. A fact is one thing enola found in the source: a module, a function, a route, a table. Each fact records where it was found (repository, file, line), some properties, and its **relations**, the directed edges to other facts.

### Fact kinds

| Kind | One fact per |
|---|---|
| `module` | package or directory of code |
| `symbol` | function, method, type, class, variable, constant or enum |
| `route` | API endpoint, on the side that serves it or the side that calls it (HTTP, gRPC, GraphQL) |
| `storage` | data store the code names: a table, a model, a bucket, a message topic |
| `dependency` | import, or declared package; in a multi-repo graph, also one repository depending on another |
| `service` | whole repository, in a multi-repo graph |
| `intent` | entry in the architecture you *declared* (a layer, a service, an allowed seam), rather than something measured |
| `association` | framework model relationship, such as a Rails `has_many` |
| `extraction` | extractor's own account of what it saw and could not resolve in one repository |
| `test_ref` | test file, recording which production code it exercises |
| `file_ref` | source file, recording calls made at the top level, outside any function |
| `lint` | finding an external linter reported through a [provider](PROVIDERS.md) |

Most kinds are **measured** from source. `intent` is **declared**: it is what you said the architecture should be, which is how the `intent` and `layers` checks can compare the two. `extraction`, `test_ref`, `file_ref` and `lint` are **reference-only**: they inform coverage and dead-code questions, and nothing counts them as coupling.

### Relation kinds

| Relation | Meaning |
|---|---|
| `declares` | a symbol, route or storage fact belongs to its module |
| `imports` | a module imports another module or package |
| `calls` | a symbol calls another |
| `implements` | a symbol implements an interface or extends a base class |
| `instantiates` | a symbol constructs an instance of a type |
| `injects` | a symbol receives a type through dependency injection |
| `names` | a symbol refers to a method by name, as data, without calling it |
| `depends_on` | one repository depends on another |
| `handled_by` | a route is served by a handler symbol (added after extraction) |
| `implemented_by` | a declared contract operation is implemented by a code symbol (added after extraction) |
| `has_method` | a type owns a method (added when the graph is built) |

### An example

From [`examples/cross-repo/`](../examples/cross-repo/), where a `web` service calls an `api` service. Shown as a summary, then one line as it is stored:

```
route       /api/v2/orders/{id}   api/server.go:27   GET, served     handled_by -> getOrder
route       /api/v2/orders/{id}   web/client.go:10   GET, client call
service     web                                                       depends_on -> api
dependency  web -> api            via http-client    endpoints: GET /api/v2/orders, GET /api/v2/orders/{id}
```

```json
{"kind":"service","name":"web","repo":"web","props":{"edge_coverage":[{"declared":0,"detected":3,"edge_type":"http_client","external":0,"resolved":2,"unresolved":1}],"synthetic":"crossrepo"},"relations":[{"kind":"depends_on","target":"api","target_id":"13e352a8450956d6c20ab80501574e2f"}],"id":"ee73c9905d2bd500f619cb224dafe5cb"}
```

The `service` fact carries the link and also its own coverage account: three outbound calls detected, two resolved, one not.

Every field, every contract property per kind, and the full vocabularies are in [schema/facts.md](schema/facts.md).

## How it's built

A snapshot is produced by one fixed pipeline, the same on every run:

1. **Walk the files.** Everything under the repository, minus the ignore globs (build output, vendored code, tests, generated files).
2. **Extract.** Each language extractor checks whether it applies (Go runs when there is a `go.mod`), parses the files it owns with tree-sitter or a language-specific parser, and emits facts. Frameworks are handled here: a Rails route file or a Spring controller becomes `route` facts.
3. **Link.** Resolve the edges no single file could show: a route's handler, a router prefix composed across functions, and, when two or more repositories are loaded, the connections between them.
4. **Index.** Build the graph both ways, so "what does X depend on" and "what depends on X" are equally cheap. Three kinds of synthetic edge make traversals complete: `has_method`, a bridge from a module through to what it imports, and normalised targets so a call in one repository reaches the symbol in another.
5. **Analyse.** Run the checks (enola calls them explainers) over the graph and record their findings.
6. **Write.** Save the snapshot to `.enola/` and record a receipt of how it was built.

No step calls a language model. Re-runs only re-parse files whose content changed, so refreshing a snapshot is fast. Stage-by-stage internals: [ARCHITECTURE.md → The pipeline](../ARCHITECTURE.md#the-pipeline).

## Across repositories

Give enola several repositories (a `cluster.yaml`, or a folder holding them) and it builds one graph over all of them. Every fact carries the label of the repository it came from, and each repository becomes a `service` fact.

The linker then connects them from evidence the extractors already found:

- **HTTP:** a route one repository calls is matched to the route another serves, by normalised path and method.
- **Messaging:** a Kafka topic one repository consumes is matched to the repository that owns it.
- **gRPC and GraphQL:** calls are matched to the services and operations that serve them.
- **Imports:** a shared library one repository imports from another.
- **Shared code:** two repositories declaring the same distinctive types are recorded as coupled, without a direction and without an edge.

Each link becomes a `dependency` fact whose `via` property says how it was established. A call enola detected but could not match is **counted as unresolved**, never guessed at, and `enola coverage` reports those counts per service. Because links are ordinary edges, every traversal and impact question reaches across repositories with no extra step.

What each signal matches, what is deliberately left unlinked, and how to tune it: [ARCHITECTURE.md → Cross-repo](../ARCHITECTURE.md#cross-repo-the-graph-of-graphs), [CLUSTERS.md](CLUSTERS.md), [EXTENDING.md](EXTENDING.md).

## Identity and determinism

The same source, configuration and enola build always produce the same graph, byte for byte. Three identifiers make that checkable:

- **Fact ID.** A fact's `id` is a hash of its repository, kind, name and file. It excludes line numbers, so it stays stable when code moves within a file.
- **Relation targets.** An edge names its target (`target`), and carries the target's ID (`target_id`) whenever that name resolves to exactly one fact. An unresolved or ambiguous target keeps only the name, and a consumer should keep it that way rather than pick one.
- **Snapshot ID.** A SHA-256 fingerprint over all the facts, the enola version and the effective configuration. Two runs over the same inputs produce the same ID.

Before comparing two snapshots, enola checks their receipts. If they were built by a different enola version, with different extractors or different ignore rules, their difference would describe how they were made rather than what changed, so enola declines to compare them instead of reporting that as your change.

Why the graph is a value computed on demand rather than a store kept up to date: [SNAPSHOTS.md](SNAPSHOTS.md). Identity rules in full: [schema/facts.md → Identity and IDs](schema/facts.md#identity-and-ids).

## On disk

`enola --generate` writes the snapshot to `.enola/` in the repository. In a multi-repo run, every repository's `.enola/` receives the same complete linked graph.

| File | What it holds | Stable contract? |
|---|---|---|
| `facts.jsonl` | The graph: every fact and its relations, one JSON object per line. There is no separate graph file. | yes, [schema/facts.md](schema/facts.md) |
| `insights.json` | The findings the checks produced, with confidence and evidence | yes, [schema/insights.md](schema/insights.md) |
| `receipt.json` | How the snapshot was built: format version, snapshot ID, enola version, git commit and dirty state, extractors, config hashes, extraction quality | yes, [schema/receipt.md](schema/receipt.md) |
| `snapshot.meta.json` | The receipt fields plus per-file content hashes, for incremental runs | no |
| `llm_context.md` | A compact written summary for an agent to read directly | no |
| `extractor_cache.json` | Parsed results per file, so unchanged files are not re-parsed | no; keep it between runs |
| `previous/` | The snapshot before this one, rotated on every write | same files as above |
| `baseline/` | The snapshot you pinned with `enola baseline pin`; survives re-snapshots | same files as above |

The contract files carry `format_version` (currently `1`). Additive changes, such as a new kind or property, keep the version; renaming, removing or changing the meaning of a documented field bumps it. Consumers must accept unknown kinds and fields. Rules in full: [schema/README.md](schema/README.md).

Outside the repository, `~/.enola/graphs/` holds the **architecture history**: one revision per snapshot, each stored as a patch against the previous one. It is the only thing enola keeps that the working tree cannot reproduce, so nothing that grades a change reads it; deleting it changes no verdict and no snapshot ID. See [HISTORY.md](HISTORY.md).

## Consuming the graph

The graph is infrastructure: the same snapshot serves every consumer, and none of them needs an AI model.

- **The CLI.** `enola --explain`, `enola check`, `enola coverage`, `enola dashboard` and the history commands all read it. See [CLI.md](CLI.md).
- **MCP.** The 22 tools give an agent queries over the graph: look up facts, traverse edges, find the path between two symbols, measure the impact of changing one. See [ARCHITECTURE.md → The tools](../ARCHITECTURE.md#the-tools).
- **The files.** Run enola as a subprocess and load `facts.jsonl`, `insights.json` and `receipt.json` into your own store. [Cognee](https://github.com/topoteretes/cognee) does this for its code-graph search. See [INTEGRATING.md](INTEGRATING.md).

To add facts from your own tooling, see [PROVIDERS.md](PROVIDERS.md); to teach the linker a connection it does not know, see [EXTENDING.md](EXTENDING.md).

## What's not in the graph

- **Runtime behaviour.** Timeouts, retries, delivery guarantees and performance under load are not structure, and the graph has no representation for them.
- **What could not be resolved.** A URL built at runtime or a client library enola does not know is counted, not linked. The `extraction` and `service` facts carry those counts, and `receipt.json` rolls them up.
- **What was excluded.** Files the ignore rules skipped and files that failed to parse are counted in the receipt, with a sample of each naming the glob or the error, rather than silently missing.

Per-language limits: [extraction/](extraction/README.md). Gaps found so far: [BLIND-SPOTS.md](BLIND-SPOTS.md).
