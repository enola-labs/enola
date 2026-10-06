<div align="center">

  <h1>enola</h1>

  <p>enola - One graph of your whole software system, built from the code, on your machine</p>

  <p align="center">
  <a href="https://enola.tech">Website</a>
  ·
  <a href="docs/README.md">Docs</a>
  ·
  <a href="docs/CLI.md">CLI reference</a>
  ·
  <a href="docs/BENCHMARKS.md">Benchmarks</a>
  ·
  <a href="https://github.com/enola-labs/enola/releases">Releases</a>
  ·
  <a href="https://github.com/enola-labs/enola/issues">Report a gap</a>
  </p>

  <p>
  <a href="https://github.com/enola-labs/enola/actions/workflows/ci.yml"><img src="https://github.com/enola-labs/enola/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/enola-labs/enola/releases"><img src="https://img.shields.io/github/v/release/enola-labs/enola" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/enola-labs/enola" alt="License"></a>
  <a href="https://github.com/enola-labs/enola/stargazers"><img src="https://img.shields.io/github/stars/enola-labs/enola.svg?style=social&amp;label=Star&amp;maxAge=2592000" alt="GitHub stars"></a>
  <a href="https://mcptoplist.com/server/glama%2Fenola-labs%2Fenola"><img src="https://mcptoplist.com/badge/glama%2Fenola-labs%2Fenola.svg" alt="MCP Toplist"></a>
  </p>

  <p>enola reads your source code and writes down what is in it: the modules, the functions, the API routes, the database tables it touches, the message topics it publishes. Then it records how each of those pieces connects to the others, inside one repository and across several. That record is a <em>graph</em>: a list of things, and a list of links between them. You can ask the graph questions, hand it to your coding agent, or build your own tools on it.</p>

  <p><strong>Runs locally as one binary. No AI model, no language server, no account, no upload.</strong><br />
  The graph comes from parsing your code. The same code always produces the same graph.</p>

<p align="center">
  <img src="docs/images/layers-gate.gif" alt="enola check on the layers-gate example: a helper added to storage imports the delivery layer, the default run reports it and exits 0, --fail-on=layers fails the same change, and after the fix the re-run passes" width="80%" />
</p>
</div>

## When to use enola

- **Find out what a change will break.** Before you edit a function, a route, or a module, ask the graph what depends on it. The answer is a list of real places in the code, not a guess. [Quickstart](#quickstart).
- **Keep your architecture the way you declared it.** Write down the layer order your code should follow, or which service may call which. Every change is then graded against that, and only what the change broke is reported. [Grade a change](#grade-a-change).
- **Give your coding agent the structure of the system.** Over MCP, the agent asks the graph what depends on the code it is about to touch, and a hook grades its edit when it is done. [Connect your agent](#connect-your-agent).
- **See several repositories as one system.** A frontend's call to `/api/orders` is linked to the Go handler that serves it. One service's Kafka producer is linked to the service that consumes the topic. [Across repositories](#across-repositories).
- **Build your own tools on the graph.** A snapshot is a set of plain files with a documented format. Load them wherever you need them. [Build on the graph](#build-on-the-graph).

## Choose your starting point

| I want to… | Start here |
| --- | --- |
| See what enola finds in a repository I have | [Quickstart](#quickstart) |
| Check whether a change broke the architecture | [Grade a change](#grade-a-change) |
| Run that check on every pull request | [Use it in CI](#use-it-in-ci) |
| Give my coding agent the graph | [Connect your agent](#connect-your-agent) |
| Link a backend and the apps that call it | [Across repositories](#across-repositories) |
| Load the graph into my own tool | [Build on the graph](#build-on-the-graph) |
| Know which languages it understands | [What it reads](#what-it-reads) |
| Follow one worked example end to end | [Your first graded change](docs/FIRST-CHANGE.md) |

## Quickstart

**1. Install enola.** One prebuilt binary; no Go toolchain or C compiler needed.

```bash
curl -fsSL https://raw.githubusercontent.com/enola-labs/enola/main/install.sh | sh
```

This puts `enola` in `~/.local/bin`. The same binary is on PyPI (`pip install enola-cli`) and RubyGems. Every install route, and how to upgrade, is in [docs/CLI.md](docs/CLI.md#install).

**2. Point it at a repository you have checked out.**

```bash
enola --explain /path/to/your/repo
```

No config file, no account, nothing written to disk. It reads the code and prints what it found. If you have nothing at hand, run it on enola itself:

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

How to read it:

- A **fact** is one thing enola recorded: "this function exists", "this function calls that one", "this route is served here". `Total facts` is how many it found.
- A **cyclic dependency** is two or more modules that each need the other, so neither can be changed or tested alone.
- A **layer violation** is a module reaching into a layer it is not allowed to use, under a layer order the repository declared. enola declares its own in [`enola-intent.yaml`](enola-intent.yaml); 0 means nothing in the code crosses it.
- A **hotspot** is a module many other modules depend on. `fan-in` is how many modules use it; `fan-out` is how many it uses; `blast radius` is how many modules a change to it can reach. Read the first row as: `internal/facts` is used from 253 places, and a change to it can reach 96 modules.

**3. Keep the graph and look at it.** `--explain` only prints. To save the graph, build it with `--generate`. It is written under `.enola/` in the repository, and that saved graph is what the local dashboard shows:

```bash
enola --generate .
enola dashboard --open
```

The dashboard is a local web page; nothing is sent anywhere. [Dashboard guide](docs/DASHBOARD.md).

## How enola works

enola parses each source file, turns what it finds into typed facts, links the facts into a graph, and runs checks over the graph: dependency cycles, layer violations, unused routes, hotspots and more. Each check is called an **explainer**, and [docs/EXPLAINERS.md](docs/EXPLAINERS.md) describes every one of them.

| Command | What it does | Learn more |
| --- | --- | --- |
| `enola --explain <repo>` | Read a repository and print a report. Writes nothing. | [CLI reference](docs/CLI.md#explain-a-repository-at-a-glance) |
| `enola --generate <repo>` | Build the graph and save it under `.enola/` as a snapshot. | [The graph](docs/GRAPH.md) |
| `enola baseline pin` | Record the current state of the graph as the point to compare against. | [Gating a change](docs/GATING.md) |
| `enola check` | Build the graph again and report only what changed since the pin. | [Gating a change](docs/GATING.md) |
| `enola coverage <cluster>` | For several repositories, say which cross-repository calls were linked and which could not be. | [Clusters](docs/CLUSTERS.md) |
| `enola dashboard --open` | Show the saved graph in a local web page. | [Dashboard guide](docs/DASHBOARD.md) |
| `enola install --hooks` | Tell your coding agents enola exists and grade each of their sessions. | [Connect your agent](#connect-your-agent) |

Three properties hold for all of it:

- **Deterministic.** The same code always gives the same graph. 91 open-source repositories indexed three times each (once cold, twice warm) gave byte-identical results, over 8.1 million facts. Every snapshot carries a **receipt**: a record of exactly how it was built. enola refuses to compare two snapshots that were not built the same way.
- **Fast enough for every commit.** Re-indexing an unchanged tree took 4.8s for grafana and 41.5s for the Linux kernel.
- **Local.** One binary reading local files. No model, no embeddings, no upload.

What the graph contains and what it writes to disk: [docs/GRAPH.md](docs/GRAPH.md). The numbers above and the scripts that produce them: [docs/BENCHMARKS.md](docs/BENCHMARKS.md). Internals: [ARCHITECTURE.md](ARCHITECTURE.md).

## Grade a change

The simplest way to see what changed is to check what came in with a pull. Record the current state, pull, and compare:

```bash
enola baseline pin
git pull
enola check
```

`enola check` builds the graph again and reports only what changed since the pin: new dependencies, new calls, new findings. Problems the repository already had stay out of the report. Your own edits work the same way: pin, change, check.

Here a new helper in `storage` imports the delivery layer. It compiles, every test passes, and it breaks the layer order the repository declared:

```
FAIL — 1 structural regression introduced.

Regressions (fail):
  - [layers] 1.00 — Layer violation: storage -> delivery
      import of notify
```

A **regression** is something the change made worse compared with the pin. Some checks grade against how you say your system should look, such as a layer order or which service may call which. You write that down in [docs/INTENT.md](docs/INTENT.md) and [docs/CONSTRAINTS.md](docs/CONSTRAINTS.md).

Nothing fails by default. You choose what should, for example `enola check --fail-on=layers`. [docs/GATING.md](docs/GATING.md) explains what can fail a build and why; [docs/HISTORY.md](docs/HISTORY.md) covers how the architecture changed over time. The whole loop on a module small enough to read in a minute: [docs/FIRST-CHANGE.md](docs/FIRST-CHANGE.md).

## Use it in CI

The same check runs on every pull request with [enola-action](https://github.com/enola-labs/enola-action). It resolves the exact base commit, grades both sides on the runner, annotates the lines that introduced a finding, and writes the architecture delta to the job summary:

```yaml
- uses: actions/checkout@v7
  with:
    fetch-depth: 0
- uses: enola-labs/enola-action@v2   # reports only; add fail-on to gate
```

Same explainers and same exit codes as `enola check` in your shell, with no baseline to publish or restore. A ready workflow file is in [`examples/ci/`](examples/ci/).

## Connect your agent

First tell your agents enola exists, and let it grade each session:

```bash
enola install --hooks
```

Then give the agent the graph over **MCP** (Model Context Protocol: the standard way a coding agent calls an outside tool):

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

What changes for the agent: before an edit, it asks the graph what depends on the code it is about to touch, instead of piecing that together from text searches. After the edit, a **hook** (a command the agent runs automatically at the end of its turn) runs the same `enola check` as above. The agent sees what it actually changed and fixes a regression before telling you it is done.

`enola install` previews every file it changes and asks first; `enola uninstall` reverses all of it. `enola doctor` tells you whether the hooks are really firing. Per-client details, including Copilot's different config key: [docs/CLI.md](docs/CLI.md#connect-it-to-your-agent).

## Across repositories

A system rarely lives in one repository, so enola does not stop at one. Give it a backend and the things that call it (a web app, a mobile app, another service) and it links them into one graph. Then it can answer: *if I change this endpoint, what breaks?*

The hard part is that two sides rarely spell an endpoint the same way. In [`examples/cross-repo/`](examples/cross-repo/) the web service calls `/api/v2/orders/{id}`, but the API service never writes that string anywhere:

```go
v2 := r.PathPrefix("/api/v2").Subrouter()
registerOrders(v2)                        // in main()

r.HandleFunc("/orders/{id}", getOrder)    // in registerOrders(), a different function
```

enola follows the prefix into the function it was passed to and files the route under the address it actually answers on, so the call links. It does the same for Express, FastAPI, Axum, Rails and Swift, where the prefix often sits in another file entirely.

When it *cannot* link something, it says so instead of guessing:

```
$ enola coverage cluster.yaml

  service  classification  detected  resolved  unresolved
  api      isolated              0         0           0
  web      connected             3         2           1
```

The unresolved call builds its URL at runtime, so there is nothing to match. That distinction matters: a service with no connections and a service whose connections enola failed to follow should never look the same. The example runs in one command: `./run.sh`. How several repositories are described and matched: [docs/CLUSTERS.md](docs/CLUSTERS.md).

## Build on the graph

A **snapshot** is a set of plain files with a [documented format](docs/schema/README.md): the facts, the relationships between them, the findings, and a receipt recording exactly how it was built. Run enola as a subprocess and load those files wherever you need them.

[Cognee](https://github.com/topoteretes/cognee) builds its code-graph search this way: it pins an `enola-cli` release and loads the snapshot files, and that search needs no LLM key. How it finds the binary and what it writes: [docs/INTEGRATING.md](docs/INTEGRATING.md#cognee). If you got enola as part of Cognee, [docs/COGNEE.md](docs/COGNEE.md) shows where the binary is and how to use it directly.

[docs/GRAPH.md](docs/GRAPH.md) explains what the graph contains and which files are a stable contract; [docs/INTEGRATING.md](docs/INTEGRATING.md) shows how to load it step by step.

## What it reads

More than twenty languages and formats, detected automatically. A repository with two languages is indexed as two languages without being told.

- **Application code:** Go, Java, Kotlin, Scala, JavaScript, TypeScript, Vue, Svelte, Ember, Angular, Python, Ruby, PHP, Swift, Dart/Flutter, Rust, C/C++, .NET (C#, VB.NET, F#)
- **APIs and messaging:** OpenAPI, gRPC, GraphQL, AsyncAPI
- **Infrastructure:** Terraform/HCL, Ansible

On top of the language it understands the frameworks that shape routes, storage and wiring, among them Rails, Django, FastAPI, Spring, Express, Next.js, ASP.NET Core, Laravel, Axum, SwiftUI and Jetpack Compose. The full table is in [docs/LANGUAGES.md](docs/LANGUAGES.md). A language you do not see there is a gap worth [reporting](https://github.com/enola-labs/enola/issues).

## Explore examples

Each example is a small repository plus a `run.sh` that reproduces the output shown in its README.

- [The gate on five packages](examples/layers-gate/): a module built to a layer order, and one change that breaks it. This is the fixture the outputs in this README come from.
- [Two repositories, one graph](examples/cross-repo/): a web service and the API it calls, with one call enola can link and one it reports as unresolved.
- [Policy as code](examples/policy-as-code/): PCI DSS and GDPR-inspired constraints on a small Go module, with three changes that violate them.
- [A custom HTTP client](examples/custom-client/): teaching enola an in-house client so its calls link to server routes.
- [A CI workflow](examples/ci/) and [a pre-commit hook](examples/hooks/) ready to copy.
- [Per-language configuration files](examples/) for Go, TypeScript, Python, Ruby, PHP, Kotlin, Swift and C++.

## Benchmarks

Every number below is measured on public repositories with scripts you can rerun. Method, corpus and limits: [docs/BENCHMARKS.md](docs/BENCHMARKS.md).

| Question | Result | What was measured |
| --- | --- | --- |
| Does the same commit produce the same graph? | **91 / 91** | public repositories produced byte-identical graphs across a cold run and two warm ones |
| When something regresses, is exactly that reported? | **20 / 20** | injected dependency cycles caught, with none of 1,859 pre-existing findings repeated as new |
| Of the cross-repository calls that exist, how many are linked? | **19 / 24** | fixture calls resolved, with the 3 unresolved and 2 external ones reported, not hidden |
| Does it finish on a real repository? | **0** | parse errors across the whole corpus, the Linux kernel included |
| Does an agent, given the check, avoid shipping the regression? | **3 / 3 → 0 / 3** | agents shipped the injected cycle without enola, and none did with the hooks installed |

## Limitations

- enola models structure, not runtime behaviour. It knows a service calls another; it knows nothing about timeouts, retries or whether a message can be lost.
- Calls it cannot resolve, such as URLs built at runtime, are reported as unresolved rather than guessed. Per-language limits are in [docs/extraction/](docs/extraction/) and the gaps found so far in [docs/BLIND-SPOTS.md](docs/BLIND-SPOTS.md).
- Most findings are advisory. Across the benchmark corpus, 89.2% of them could not fail a build even with every check named.
- A clean `enola check` means the change introduced nothing new, not that the repository is clean.

With a coding agent, most of these stop being dead ends. enola says exactly where its knowledge stops: which call it could not resolve, which finding is only advisory. The agent can open that file, read the retry settings or the URL being built, and judge whether a finding matters for this change. The graph shows the agent where to look; the agent reads what the graph cannot hold. What the agent concludes is still the agent's judgement, not a measurement, and enola keeps the two apart.

## Documentation

- **[Choose a guide by task](docs/README.md)**
- **[Your first graded change](docs/FIRST-CHANGE.md)**: the loop end to end, on a module small enough to read
- **[The graph](docs/GRAPH.md)**: what is in it, how it is built, what it writes to disk
- **[CLI reference](docs/CLI.md)**: install, agent setup, commands, flags and exit codes
- **[Gating a change](docs/GATING.md)**: what a verdict contains and what can fail a build
- **[Dashboard guide](docs/DASHBOARD.md)**
- **[Building on enola](docs/INTEGRATING.md)**
- **[Glossary](docs/GLOSSARY.md)**: the terms used in enola's output
- **[Architecture](ARCHITECTURE.md)**: the fact model, pipeline, graph and MCP tools
- **[Changelog](CHANGELOG.md)** and **[examples](examples/)**

## Community & Support

### Found it useful?

If `enola --explain` told you something about your codebase you did not already know, a star helps other people find it.

If it missed something it should have caught (an unresolved edge, a route it did not match, a construct it walked past), please [open an issue](https://github.com/enola-labs/enola/issues). Those are the most useful bug reports this project gets.

### Contributing

Code, documentation, bug reports and feature ideas are all welcome. [`CONTRIBUTING.md`](CONTRIBUTING.md) covers the development workflow, the architecture gate that runs on every pull request, and how to add a language, a connection or an analysis.

### Code of Conduct

Read the [Code of Conduct](CODE_OF_CONDUCT.md) for the guidelines this community follows.

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

enola bundles third-party components under their own licenses; see [`NOTICE`](NOTICE). Swift parsing uses the [tree-sitter-swift](https://github.com/alex-pinkus/tree-sitter-swift) grammar by Alex Pinkus (MIT), vendored under [`internal/extractors/swiftextractor/grammar/`](internal/extractors/swiftextractor/grammar/); Dart parsing uses [tree-sitter-dart](https://github.com/UserNobody14/tree-sitter-dart) by UserNobody14 and others (MIT), vendored under [`internal/extractors/dartextractor/grammar/`](internal/extractors/dartextractor/grammar/); Scala parsing uses [tree-sitter-scala](https://github.com/tree-sitter/tree-sitter-scala) (MIT), vendored under [`internal/extractors/scalaextractor/grammar/`](internal/extractors/scalaextractor/grammar/). Every other grammar is a normal Go module dependency and is not vendored.
