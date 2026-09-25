# enola

[![MCP Toplist](https://mcptoplist.com/badge/glama%2Fenola-labs%2Fenola.svg)](https://mcptoplist.com/server/glama%2Fenola-labs%2Fenola)
[![CI](https://github.com/enola-labs/enola/actions/workflows/ci.yml/badge.svg)](https://github.com/enola-labs/enola/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/enola-labs/enola)](https://github.com/enola-labs/enola/releases)
[![License](https://img.shields.io/github/license/enola-labs/enola)](LICENSE)

**enola builds one graph of your software system: every repository, language, framework and technology in it, and how they connect.**

**It runs locally as a single binary, with no model, external language server, or separate indexing infrastructure required.**

It reads your source code and records what is there: modules, functions, API routes, database access, message topics, infrastructure. Then it links those pieces, inside each repository and across them. A frontend's call to `/api/orders` is linked to the Go handler that serves it; one service's Kafka producer is linked to the service that consumes the topic. [What exactly is in the graph](docs/GRAPH.md).

You can ask that graph questions yourself, give it to your coding agent, or build your own tools on it. The graph comes from parsing your code; no AI model takes part in producing it. The same code always produces the same graph, and it never leaves your machine.

## Try it

```bash
curl -fsSL https://raw.githubusercontent.com/enola-labs/enola/main/install.sh | sh
```

Then point it at any repository you have checked out:

```bash
enola --explain /path/to/your/repo
```

No config file, no account, nothing written to disk. It prints what it found. If you have nothing at hand, try it on enola itself:

```bash
git clone https://github.com/enola-labs/enola
enola --explain enola
```

Part of what that prints (the numbers move as the code does):

```
Overview
  Languages:           go, typescript, c, ruby, python
  Total facts:         10814

Architecture
  cyclic dependencies         0
  layer violations            0

Impact analysis (hotspots)
  Top hotspots (by coupling):
    module                            fan-in  fan-out crit     blast radius
    internal/facts                       253        1 high     96
    pkg/command                            1       94 high     1
    pkg/bootstrap                         15       69 high     4
```

Read the first hotspot as: `internal/facts` is used from 253 places, and a change to it can reach 96 modules.

The binary is also on PyPI (`pip install enola-cli`) and RubyGems. Every install route is in [docs/CLI.md](docs/CLI.md#install).

## Across repositories

A system rarely lives in one repository, so enola doesn't stop at one. Give it a backend and the things that call it (a web app, a mobile app, another service) and it links them into one graph. Then it can answer the question that usually costs a morning and two colleagues: *if I change this endpoint, what breaks?*

The hard part is that two sides rarely spell an endpoint the same way. In [`examples/cross-repo/`](examples/cross-repo/) the web service calls `/api/v2/orders/{id}`, but the API service never writes that string anywhere:

```go
v2 := r.PathPrefix("/api/v2").Subrouter()
registerOrders(v2)                        // in main()

r.HandleFunc("/orders/{id}", getOrder)    // in registerOrders(), a different function
```

enola follows the prefix into the function it was passed to and files the route under the address it actually answers on, so the call links. It does the same for Express, FastAPI, Axum, Rails and Swift, where the prefix often sits in another file entirely.

When it *can't* link something, it says so instead of guessing:

```
$ enola coverage cluster.yaml

  service  classification  detected  resolved  unresolved
  api      isolated              0         0           0
  web      connected             3         2           1
```

The unresolved call builds its URL at runtime, so there is nothing to match. That distinction matters: a service with no connections and a service whose connections enola failed to follow should never look the same. The example runs in one command: `./run.sh`.

## Four ways to use it

### On your own

No agent and no AI needed. `enola --explain .` analyses a repository and prints a report without writing anything. To keep the graph, build it with `--generate`. It is saved under `.enola/`, and that saved graph is what the local dashboard shows:

```bash
enola --generate .
enola dashboard --open
```

The simplest way to see what changed is to check what came in with a pull. Record the current state, pull, and compare:

```bash
enola baseline pin
git pull
enola check
```

`enola check` builds the graph again and reports only what changed since the pin: new dependencies, new calls, new findings. Problems the repository already had stay out of the report. Your own edits work the same way: pin, change, check. Here a new helper in `storage` imports the delivery layer. It compiles, every test passes, and it breaks the layer order the repository declared:

```
FAIL — 1 structural regression introduced.

Regressions (fail):
  - [layers] 1.00 — Layer violation: storage -> delivery
      import of notify
```

`enola check` runs every check enola has, and [docs/EXPLAINERS.md](docs/EXPLAINERS.md) describes each one. Some of them grade against how you say your system should look, such as a layer order or which service may call which. You write that down in [docs/INTENT.md](docs/INTENT.md) and [docs/CONSTRAINTS.md](docs/CONSTRAINTS.md).

Nothing fails by default. You choose what should, for example `enola check --fail-on=layers`. [docs/GATING.md](docs/GATING.md) explains what can fail a build and why; [docs/HISTORY.md](docs/HISTORY.md) covers how the architecture changed over time.

### In CI

The same check runs on every pull request with [enola-action](https://github.com/enola-labs/enola-action). It resolves the exact base commit, grades both sides on the runner, annotates the lines that introduced a finding, and writes the architecture delta to the job summary:

```yaml
- uses: actions/checkout@v7
  with:
    fetch-depth: 0
- uses: enola-labs/enola-action@v2   # reports only; add fail-on to gate
```

Same explainers and same exit codes as `enola check` in your shell, with no baseline to publish or restore.

### With your coding agent

First tell your agents enola exists, and let it grade each session:

```bash
enola install --hooks
```

Then give the agent the graph over MCP:

| Client | Do this |
|---|---|
| **Claude Code** | `claude mcp add enola enola` |
| **Codex** | `codex mcp add enola -- enola` |
| **Copilot (VS Code)** | `code --add-mcp '{"name":"enola","command":"enola"}'` |
| **Cursor** | add the block below to `.cursor/mcp.json` (or `~/.cursor/mcp.json` for every project) |
| **opencode** | nothing, `enola install` already registered it |
| **Pi** | nothing, `enola install` wrote an extension that serves the tools (Pi has no MCP client); trust the project in Pi, or use `--global` |
| **Any other MCP client** | add the block below to its MCP config |

```json
{ "mcpServers": { "enola": { "command": "enola" } } }
```

Before an edit, the agent asks the graph what depends on the code it is about to touch, instead of piecing it together from searches. After the edit, a hook runs the same check as above, so the agent sees what it actually changed and fixes a regression before telling you it's done.

`enola install` previews every file it changes and asks first; `enola uninstall` reverses all of it. `enola doctor` tells you whether the hooks are really firing. Per-client details, including Copilot's different config key: [docs/CLI.md](docs/CLI.md#connect-it-to-your-agent).

### As a foundation for your own tools

A snapshot is a set of plain files with a [documented format](docs/schema/README.md): the facts, the relationships between them, the findings, and a receipt recording exactly how it was built. Run enola as a subprocess and load them wherever you need them.

[Cognee](https://github.com/topoteretes/cognee) builds its code-graph search this way: it pins an `enola-cli` release and loads the snapshot files, and that search needs no LLM key. How it finds the binary and what it writes: [docs/INTEGRATING.md](docs/INTEGRATING.md#cognee).

[docs/GRAPH.md](docs/GRAPH.md) explains what the graph contains and which files are a stable contract; [docs/INTEGRATING.md](docs/INTEGRATING.md) shows how to load it.

## What it reads

23 languages and formats, detected automatically. A repository with two languages is indexed as two languages without being told.

- **Application code:** Go, Java, Kotlin, Scala, JavaScript, TypeScript, Vue, Svelte, Ember, Angular, Python, Ruby, PHP, Swift, Dart/Flutter, Rust, C/C++, .NET (C#, VB.NET, F#)
- **APIs and messaging:** OpenAPI, gRPC, GraphQL, AsyncAPI
- **Infrastructure:** Terraform/HCL, Ansible

On top of the language it understands the frameworks that shape routes, storage and wiring, among them Rails, Django, FastAPI, Spring, Express, Next.js, ASP.NET Core, Laravel, Axum, SwiftUI and Jetpack Compose. The full table is in [docs/LANGUAGES.md](docs/LANGUAGES.md). A language you don't see there is a gap worth [reporting](https://github.com/enola-labs/enola/issues).

## How it works

enola parses each file, turns what it finds into typed facts ("this function calls that one", "this route is served here"), links the facts into a graph, and runs checks over it: dependency cycles, layer violations, unused routes, hotspots and more.

- **Deterministic.** 91 open-source repositories indexed three times each (once cold, twice warm) gave byte-identical results, over 8.1 million facts. Every snapshot carries a receipt of how it was built, and enola refuses to compare two snapshots that weren't built the same way.
- **Fast enough for every commit.** Re-indexing an unchanged tree took 4.8s for grafana and 41.5s for the Linux kernel.
- **Local.** One binary reading local files. No model, no embeddings, no upload.

What the graph contains, how it is built and what it writes to disk: [docs/GRAPH.md](docs/GRAPH.md). Numbers and the scripts that produce them: [docs/BENCHMARKS.md](docs/BENCHMARKS.md). Internals: [ARCHITECTURE.md](ARCHITECTURE.md).

## Limitations

- enola models structure, not runtime behaviour. It knows a service calls another; it knows nothing about timeouts, retries or whether a message can be lost.
- Calls it cannot resolve, such as URLs built at runtime, are reported as unresolved rather than guessed. Per-language limits are in [docs/extraction/](docs/extraction/) and the gaps found so far in [docs/BLIND-SPOTS.md](docs/BLIND-SPOTS.md).
- Most findings are advisory. Across the benchmark corpus, 89.2% of them could not fail a build even with every check named.
- A clean `enola check` means the change introduced nothing new, not that the repository is clean.

With a coding agent, most of these stop being dead ends. enola says exactly where its knowledge stops: which call it couldn't resolve, which finding is only advisory. The agent can open that file, read the retry settings or the URL being built, and judge whether a finding matters for this change. The graph shows the agent where to look; the agent reads what the graph can't hold. What the agent concludes is still the agent's judgement, not a measurement, and enola keeps the two apart.

## Documentation

- **[Choose a guide by task](docs/README.md)**
- **[Your first graded change](docs/FIRST-CHANGE.md)**: the loop end to end, on a module small enough to read
- **[The graph](docs/GRAPH.md)**: what is in it, how it is built, what it writes to disk
- **[CLI reference](docs/CLI.md)**: install, agent setup, commands, flags and exit codes
- **[Gating a change](docs/GATING.md)**: what a verdict contains and what can fail a build
- **[Dashboard guide](docs/DASHBOARD.md)**
- **[Building on enola](docs/INTEGRATING.md)**
- **[Architecture](ARCHITECTURE.md)**: the fact model, pipeline, graph and MCP tools
- **[Changelog](CHANGELOG.md)** and **[examples](examples/)**

## Found it useful?

If `enola --explain` told you something about your codebase you didn't already know, a star helps other people find it.

If it missed something it should have caught (an unresolved edge, a route it didn't match, a construct it walked past), please [open an issue](https://github.com/enola-labs/enola/issues). Those are the most useful bug reports this project gets.

## License

Apache License 2.0, see [`LICENSE`](LICENSE).

This repository is the full engine, not a trial edition. Nothing is gated, metered or degraded without a key, and no snapshot, fact or usage counter ever leaves your machine. The only outbound request enola makes is to GitHub's release API: a background release check you can turn off (see [Staying current](docs/CLI.md#staying-current)), and `enola upgrade` when you run it.

## Acknowledgements

**[Muhamed Isabegović](https://github.com/misabegovic)** is the author of a large part of
what this repository does. The constraints program — declared architectural law over the
fact graph — is his, along with the vocabulary it verdicts, `plan` and `constraints mine`,
the fact-provider seam and the providers that ride it, the shareable history store behind
`blame` and `diff`, declared intent compiling into the graph, Ember support, the Rails
extraction work with the `dead-methods` and `query-loops` explainers, the Ruby surface for
writing laws as sentences, and the verdict writers that put a finding where CI reads it.
He also maintains the Ruby and Rails integration gems that drive enola from Bundler.

enola bundles third-party components under their own licenses; see [`NOTICE`](NOTICE). Swift parsing uses the [tree-sitter-swift](https://github.com/alex-pinkus/tree-sitter-swift) grammar by Alex Pinkus (MIT), vendored under [`internal/extractors/swiftextractor/grammar/`](internal/extractors/swiftextractor/grammar/); Dart parsing uses [tree-sitter-dart](https://github.com/UserNobody14/tree-sitter-dart) by UserNobody14 and others (MIT), vendored under [`internal/extractors/dartextractor/grammar/`](internal/extractors/dartextractor/grammar/). Every other grammar is a normal Go module dependency and is not vendored.
