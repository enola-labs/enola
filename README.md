# Enola

[![MCP Toplist](https://mcptoplist.com/badge/glama%2Fenola-labs%2Fenola.svg)](https://mcptoplist.com/server/glama%2Fenola-labs%2Fenola)
[![CI](https://github.com/enola-labs/enola/actions/workflows/ci.yml/badge.svg)](https://github.com/enola-labs/enola/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/enola-labs/enola)](https://github.com/enola-labs/enola/releases)
[![License](https://img.shields.io/github/license/enola-labs/enola)](LICENSE)

**Architectural regression testing for AI-assisted development.**

Enola detects architectural regressions in code changes. It builds a deterministic dependency
graph from source, compares the current tree with a pinned baseline, and exits non-zero when a
selected quality rule regresses. This makes it an architectural quality gate for local development
and CI.

It checks dependency cycles, layer boundaries, declared architecture constraints, cross-repository
service dependencies, change scope and structural outliers. It resolves relationships across
source files, HTTP routes, gRPC services and messaging topics.

- Local binary; no model, embeddings, account or source upload.
- [23 supported languages and formats](#supported-languages).
- **One loop everywhere.** Inspect the graph before a change and verify the result afterward. The
  same check runs through MCP, from the CLI and in CI.

**Documentation:** [Choose a guide by task](docs/README.md) · [CLI reference](docs/CLI.md) · [Architecture internals](ARCHITECTURE.md)

## What the gate checks

| Area | Checks |
|---|---|
| Dependencies | cycles, depth, layer violations |
| Declared architecture | components, constraints, allowed dependencies |
| Service boundaries | undeclared or unresolved cross-repository calls |
| Change scope | files and packages outside the declared target |
| Code structure | hotspots, public surface, complexity and dead code |

## Quickstart

Install the binary. Prebuilt releases support Linux, macOS and Windows:

```bash
curl -fsSL https://raw.githubusercontent.com/enola-labs/enola/main/install.sh | sh
```

The installer writes `~/.local/bin/enola`. Add the directory to `PATH` if needed:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

PyPI installs the same binary:

```bash
pip install enola-cli
```

Inspect a repository without writing files:

```bash
enola --explain .
```

Create a baseline and check a change:

```bash
enola baseline pin
# edit the repository
enola check                     # reports architectural changes; exits 0
enola check --fail-on=cycles    # exits 1 for new dependency cycles
```

No rule is enforced by default. Select gate rules with `--fail-on`, or declare project-specific
layers and constraints in `enola-intent.yaml`. See [What fails the build](#what-fails-the-build)
and [architecture policy](docs/INTENT.md).

### Command index

```bash
enola --explain .               # inspect without writing files
enola --generate .              # write a snapshot
enola baseline pin              # record the comparison baseline
enola check --fail-on=cycles    # run the quality gate
enola dashboard --open          # inspect the snapshot in a browser
enola doctor                    # check hooks, baseline and version status
```

`--generate` writes:

```text
.enola/
├── facts.jsonl          extracted symbols, dependencies and edges
├── insights.json        architectural findings
├── receipt.json         source and extractor provenance
├── llm_context.md       compact snapshot summary
├── extractor_cache.json incremental extraction cache
└── baseline/            pinned comparison created by `baseline pin`
```

## Worked examples

| Example | Demonstrates |
|---|---|
| [Layer gate](examples/layers-gate/) | Declared layer order, baseline and failing check |
| [Cross-repository graph](examples/cross-repo/) | Client calls linked to server routes |
| [Custom HTTP client](examples/custom-client/) | Linking an in-house client API |
| [Policy as code](examples/policy-as-code/) | Components, constraints and compliance metadata |
| [GitHub Actions](examples/ci/architecture-gate.yml) | Pull-request quality gate |

## Integrations

### Ruby and Rails

The `enola-rb` gem installs the binary and adds Rails tasks:

```bash
bundle add enola-rb
bin/rails generate enola:install
bin/rake enola:check
```

The integration is maintained by [Muhamed Isabegović](https://github.com/misabegovic) in
[`misabegovic/enola-rb`](https://github.com/misabegovic/enola-rb). See the
[Rails guide](docs/RAILS.md) for providers, generated constraints and CI setup.

### MCP clients

See [MCP integration](docs/MCP.md) for client configuration, repository instructions and hooks.

Open the read-only local dashboard with:

```bash
enola dashboard --open
```

It remains attached until Ctrl-C. See the [dashboard guide](docs/DASHBOARD.md).

## Gate output

The verdict identifies the rule, dependency, symbols and source positions involved:

<details>
<summary>Example failing and report-only verdicts</summary>

```
FAIL — 1 structural regression introduced.

Regressions (fail):
  - [layers] 1.00 — Layer violation: storage -> delivery
      import of notify

Policy: fail on new findings from [layers] at confidence >= 1.00.

What changed
  symbols      +1
  dependencies +1
  edges        +4  (imports +1, calls +2, declares +1)

Added (2):
  symbol     storage.LoadPrice                            storage/storage.go:11
  dependency storage -> layersgate/notify                 storage/storage.go:3

New coupling (4):
  storage                                      --imports--> notify
  storage.LoadPrice                            --calls--> notify.SendReceipt
  storage.LoadPrice                            --calls--> storage.ReadPrice
  storage.LoadPrice                            --declares--> storage

New coupling is reported, not failed: an added call edge is what ordinary work
looks like. Inspect the list above if it is more than you expected.
```

The report contains only changes relative to the baseline. Lists contain twelve entries by default;
use `--detail` for the full text report or `--json` for the complete structured result.

Without an enforcement policy, the finding is reported but the command exits `0`:

```
PASS — 1 new finding reported, nothing enforced: no policy set.

New findings (reported — no failure policy set):
  - [layers] 1.00 — Layer violation: storage -> delivery
      import of notify

No --fail-on policy is set, so nothing in this run could fail the build. These are
reported for you to judge. Enforce the ones you want enforced: --fail-on=layers
(`enola check --help` lists all 22).
```

The output states when no policy was enforced.

</details>

## Gate workflow

1. `enola baseline pin` records the architecture before a change.
2. `enola check` compares the working tree with that baseline.
3. New findings selected by `--fail-on` produce exit code `1`.
4. Findings already present in the baseline do not fail the change.

Run the same command locally and in CI. Use
[`enola-action`](https://github.com/enola-labs/enola-action) or the
[workflow example](examples/ci/architecture-gate.yml) to select the pull-request base.

This example reports a layer violation, fails when `layers` is selected, and passes after the fix:

![enola check on the layers-gate example: a helper added to storage imports the delivery layer, the default run reports it and exits 0, --fail-on=layers fails the same change, and after the fix the re-run passes](docs/images/layers-gate.gif)

Recorded from [`examples/layers-gate/`](examples/layers-gate/) with
[`docs/images/layers-gate.tape`](docs/images/layers-gate.tape).

## What fails the build

Enola runs all twenty-two architectural checks on every run. These checks are called
**explainers**. `--fail-on` selects which new findings affect the exit code. The default policy is
empty: findings are reported and the command exits `0`.

Examples:

- code reaching across a layer order you declared, like storage talking straight to the delivery layer (`layers`)
- two modules that ended up depending on each other (`cycles`)
- a cross-repo seam nobody declared, or a declared one the graph never measured (`intent`)
- a single function or type that a large part of the codebase depends on (`god-class`)
- a function that nearly everything calls (`hotspots`)
- an import chain ten modules deep (`dependency-depth`)
- a function far more complicated than the rest of your code (`complexity-outliers`)
- a package that exports almost everything it contains, instead of a small surface (`exported-surface`)
- API routes that nothing in the code you loaded ever calls (`unused-routes`)
- outbound calls enola could not match to any route it loaded (`coverage`)
- messaging call sites without an AsyncAPI contract, and contract operations without detected code (`messaging-coverage`)
- which repositories in a cluster ended up depending on which (`crossrepo`)
- directories that look like vendored third-party code, reported so you can decide whether to ignore them (`vendored-candidates` — informational, so it can never fail a build)
- what `import yourpackage` executes in Python, and the package `__init__.py` files responsible for most of it (`import-closure` — the summary is informational; the barrels it names are gateable)

See [all twenty-two explainers](docs/EXPLAINERS.md) for their inputs, confidence and limits.

**enola holds itself to this.** This repository declares its own layer order in [`enola-intent.yaml`](enola-intent.yaml) - six layers, entrypoint down to the fact model - and its CI runs `enola check --fail-on=layers` against it. Not `--fail-on=cycles`: enola is written in Go, where the compiler already refuses an import cycle between packages, so gating on one would enforce a rule the toolchain enforces first. The layer order is the part the compiler cannot see. Nothing but that file stops `internal/upgrade` importing `pkg/cli` today, and the build is green either way until something says otherwise.

**Why nothing is on by default.** enola used to fail on a new dependency cycle out of the box. A cycle is exactly measurable - Tarjan's SCC algorithm, no estimate anywhere - and that made it a tempting default. But *exactly measurable* is not the same as *unwanted*: Go's compiler forbids import cycles between packages outright, so a Go team's answer to the finding is usually "the compiler already has this covered"; a Rails app wires most of its graph at runtime, and two `app/` directories referencing each other is not something that community reads as a defect at all. A tool that arrives asserting otherwise spends its first impression being argued with, and the first thing those teams learn about it is which flag turns it off.

So enola states what it measured and stops there. The exception it makes for itself is the one above: an unenforced run must say it enforced nothing, because a silent green is exactly what a broken gate looks like.

**Any of the twenty-two can fail the build.** `--fail-on` takes the names above as a comma-separated list, and `--min-confidence` sets the floor within them. Two more things can fail it that are not findings at all:

- **scope spillover** - packages your change reached outside the area you declared with `--target`, gated with `--max-spillover=N`. A change can trip this with zero failing findings.
- **a gate that could not run.** A missing baseline or a bad flag exits `2`; a baseline that isn't comparable to the current code exits `3` and enola declines to grade rather than blaming your change. Neither is a judgement about the code, and neither is suppressed by `--warn-only`.

**Which of them can actually fail at the default floor: four.** `cycles`, `intent`, `constraints`, and `layers` when the order is declared in `enola-intent.yaml` are the ones enola computes with certainty, so only they reach `1.00`. Everything else is an estimate measured against your own repository - "this file has unusually many dependents *for this codebase*" - and caps below `1.00` by design ([`MaxHeuristicConfidence`](internal/explainers/common/common.go) is `0.95`). Naming an inferred explainer in `--fail-on` and nothing else therefore changes nothing at all; it needs `--min-confidence` too.

### Changing what counts

| You want | Run |
|---|---|
| The default: report everything, fail nothing | `enola check` |
| Fail on violations of a layer order you declared | `enola check --fail-on=layers` |
| Also fail on a cross-repo seam nobody declared, a declared rule breached, and new cycles | `enola check --fail-on=layers,intent,cycles,constraints` |
| Everything above, plus every explainer enola infers rather than proves | `enola check --fail-on=layers,intent,cycles,constraints,crossrepo,coverage,unused-routes,messaging-coverage,god-class,hotspots,dependency-depth,exported-surface,complexity-outliers,domain,query-loops,entry-points,dead-methods,package-metrics,dead-code,performance --min-confidence=0.8` |
| Fail if the change spread outside the area you named | `enola check --target=internal/auth --max-spillover=0` |
| Enforce a policy you set, but only warn this time | `enola check --fail-on=layers --warn-only` |

That fifth row is a different question from the others. `--target` is you saying *"this change is about `internal/auth`"*; enola works out which packages depend on it, then reports any package your change touched that isn't in that group - something you edited that your own description didn't cover. Two snapshots can tell you what changed; only you can say what you meant to change.

<details>
<summary><b>Four things that will bite you</b></summary>

- **A run with no `--fail-on` cannot fail.** That is the default, and it is deliberate - but it means a CI job that pins a baseline, runs `enola check` and reports green has enforced nothing. The output says so in a line; a job that only reads the exit code will not see it.
- **`--min-confidence` lowers the bar; it doesn't raise it.** The default floor is `1.00`, which is already the strictest setting there is. `--min-confidence=0.8` makes the gate fail on *more*, not less.
- **Confidence is per finding, not per explainer.** `layers` is the one that catches people: violations of a layer order you *declared* score `1.00`, violations of a pattern it *recognised* score `0.80`. So on a repo with no declared layer order, `--fail-on=layers` changes nothing until you also pass `--min-confidence=0.8`.
- **A misspelled name is an error.** `enola check --fail-on=cyles` exits `2` and lists the accepted, case-sensitive names instead of silently running a gate that enforces nothing.

The policy lives in flags, not in `mcp-arch.yaml`, so a pre-commit hook and a CI job can deliberately hold you to different standards.

</details>

### A failure with no failing finding

Scope is graded separately from findings, so a change can break the build having violated nothing at all. This run names no `--fail-on` whatsoever - the layer violation is reported and explicitly not enforced - and it still exits `1`, because the change touched a package its author never said it was about:

```
## Scope

**Reached beyond the declared scope.** 1 of 2 package(s) touched were predicted or declared, match ratio 0.5.

Spillover — touched but neither predicted nor declared:
  - telemetry

A package here was changed by something the declaration did not describe.
That is worth reading even when every finding is clean.

Predicted but not touched (usually fine — the change was narrower than its blast radius):
  - api
  - web
FAIL — 1 structural regression introduced.

Measurements over threshold:
  - [fail] 1 package(s) reached outside the declared scope

New findings (reported — no failure policy set):
  - [layers] 1.00 — Layer violation: storage -> delivery
      import of notify

No --fail-on policy is set, so no FINDING could fail this run — only the threshold
above grades it. These are reported for you to judge; enforce the ones you want
enforced: --fail-on=layers (`enola check --help` lists all 22).
```

The `--target` you declare is a claim about intent, and this is the gate holding you to it. Nothing here is a judgement about `telemetry` - the code may be perfectly good. It is a report that the change did something its own description didn't cover.

## Limitations

What enola does not do, and where each limit is documented in full.

- enola models structure: modules, symbols, routes, storage, dependencies and the edges between them. It has no representation for runtime behaviour - a timeout, a retry budget, whether a message can be lost - and cannot report on any of it.
- Of the twenty-two explainers, only `cycles`, `intent`, `constraints` and a declared layer order produce findings at confidence `1.00`. That is the floor `check` gates at, so naming the others in `--fail-on` has no effect unless you also lower `--min-confidence`. See [docs/EXPLAINERS.md](docs/EXPLAINERS.md).
- Most findings are advisory. Across the benchmark corpus, 96.3% of them could not fail a build under the default policy.
- The gate grades the delta against a baseline, so pre-existing findings stay silent and a repository can carry them indefinitely while every check passes. Read the findings directly to pay down existing debt.
- Outlier thresholds are computed per repository. A uniformly complex codebase produces few findings, and a clean check means the change introduced nothing new rather than that the repository is clean.
- The graph has an explicit analysis scope and visible limits. Ignore rules define which files are included; each snapshot separately reports configured exclusions, parse failures and relationships that enola detected but could not resolve. Per-language extraction limits are documented in [docs/extraction/](docs/extraction/), and explainers that under-report say so in [docs/EXPLAINERS.md](docs/EXPLAINERS.md). [docs/BLIND-SPOTS.md](docs/BLIND-SPOTS.md) records how such gaps were found.
- Confidence is comparable within an explainer but not across explainers, so enola does not rank findings or say what to fix first.

## When to use Enola

Enola is for change-level architectural enforcement. It complements line-level and behavioral
checks:

| Tool | Answers |
|---|---|
| Git diff | Which lines changed? |
| Tests | Does covered behavior still work? |
| Linter | Does each file satisfy local rules? |
| Code review | What did reviewers notice? |
| `enola check` | What structural change occurred, and does project policy allow it? |

Use Enola when a rule spans files or repositories: dependency direction, cycles, service seams,
unused routes, architectural constraints, or change scope. It does not replace tests, runtime
observability, security analysis, or review.

### Comparison with other graph tools

Repository graph tools optimize for different outputs:

| Tool | Primary use |
|---|---|
| CodeGraph | Return matching source through graph queries |
| [Graphify](https://github.com/Graphify-Labs/graphify) | Build a knowledge graph across code, documents and images |
| codebase-memory-mcp | Query code and Kubernetes manifests through an in-memory graph |
| **Enola** | Compare two deterministic architecture graphs and enforce the delta in CI |

Enola differs by acting as a gate: it compares changes against a pinned baseline, applies an
explicit policy, and returns a CI-compatible exit code. The article
[Four code graphs, four storage engines](https://menges.dev/writing/four-code-graphs-four-storage-engines/)
compares their storage models, query behavior, memory use and tradeoffs.

## How it works

enola parses your source with tree-sitter and language-specific extractors, normalizes it into a typed fact model, links it into a directed graph, and runs graph algorithms over it: Tarjan's SCC to find groups of modules that can all reach each other (a cycle), cycle-safe longest-path for the deepest import chain, and mean+2σ outlier tests to flag what sits two standard deviations above your own repository's average. No language model, no embeddings. Terms enola uses in its own output are defined in **[docs/GLOSSARY.md](docs/GLOSSARY.md)**.

**Deterministic.** The same commit yields the same answer, every time: across 81 open-source repositories indexed three times each, all 81 produced a byte-identical snapshot ID and a byte-identical fact file, over 7.0 million facts with zero parse errors ([BENCHMARKS.md](docs/BENCHMARKS.md)). Every snapshot carries a **receipt**: enola's version, the git ref and whether the tree was dirty, the extractors used, and a snapshot ID that's a `sha256` fingerprint of the facts rather than a random UUID. Before comparing two snapshots, enola checks they were built the same way - a different extractor set or changed ignore rules makes a diff meaningless, and it reports that instead of treating the mismatch as your change.

**Fast enough for every commit.** On that same corpus, a warm re-index of an unchanged tree took 7.5s for grafana (10,313 files, 167,987 facts) and 52.6s for the Linux kernel (55,408 files, 1.9M facts). Full per-repository numbers, cold and warm, are in [BENCHMARKS.md](docs/BENCHMARKS.md).

**Local.** enola runs as a local binary reading local files. Nothing leaves your machine, and there is no license check anywhere in this repository.

**[ARCHITECTURE.md](ARCHITECTURE.md)** has the fact model, the pipeline, the MCP tool reference and the analysis internals.

## Beyond one repository

Point Enola at a backend and its callers—a web app, mobile app or another service—and it joins
them into one graph. The combined graph answers:

> *If I change this endpoint, what breaks?*

It joins the two sides wherever they meet: a web client's `fetch()` to the route that serves it, a mobile app's call to that same route (an iOS endpoint enum, an Android Retrofit interface), a gRPC call to the service behind it, one service's Kafka producer to another's consumer.

**The hard part is that the two sides rarely spell the endpoint the same way.** Your frontend calls `/api/courses`. Your backend file says:

```go
r.HandleFunc("/courses", listCourses)
```

The `/api` was attached somewhere else entirely - in whatever function set this router up, quite possibly in another package. Compare the two strings literally and you find nothing, so enola follows that prefix across function and package boundaries (*interprocedurally*) and files the route under the address it actually answers on: `/api/courses`. Same story for an Express router declared in `routes/webhooks.js` and mounted in `index.js`, Axum's `.nest()`, Rails' `scope` and `namespace`, and a Swift endpoint enum whose version prefix lives three files away in a protocol extension.

Once both ends line up, `enola check` grades a change spanning two repos exactly the way it grades one that doesn't.

**It also tells you what it missed.** Some calls can't be resolved - a URL assembled at runtime, a client library enola doesn't know - and a tool that quietly drops those looks identical to one that found everything:

```bash
enola coverage cluster.yaml
```

That reports, per service, how many outbound calls it found, how many it matched to a route, and **how many it couldn't**. Which is the difference between a service that genuinely talks to nothing and a service whose edges enola just failed to follow.

[`examples/cross-repo/`](examples/cross-repo/) is a two-service demo you can run in one command. It contains one deliberately unresolvable call, so you can see what a miss looks like before you go looking for them in your own code.

## Supported languages

Enola detects all supported languages present in a repository. See
[language extraction](docs/extraction/README.md) for facts, framework support and known limits.

<details>
<summary>Languages, formats and detection markers</summary>

| Language   | Detected by |
|------------|-------------|
| Go         | `go.mod` (gorilla/mux + chi route composition / gRPC clients / Kafka topics aware) |
| Java       | `pom.xml` (Maven) or `.java` sources (Spring routes / JPA / Lombok DI / Dubbo SPI aware) |
| JavaScript | `tsconfig.json` / `package.json` with TypeScript (parsed by the TypeScript extractor) |
| TypeScript | `tsconfig.json` / `package.json` with TypeScript (Next.js, React Navigation & monorepo aware; Express sub-router mounts composed across files) |
| Vue        | `package.json` with `vue` dependency (Nuxt / Vue Router / Composition API aware) |
| Svelte     | `package.json` with `svelte` dependency (SvelteKit routing / `$lib` alias aware) |
| Ember      | `package.json` with `ember-source` dependency (`.gts`/`.gjs` template tags, `.hbs` templates, router map, ember-data) |
| Angular    | `package.json` with `@angular/core`, or an `angular.json` (component/directive/pipe/service/module roles; constructor and `inject()` DI; `.html` and inline templates in both the `*ngIf` and the `@if` dialect; router paths composed across lazy `loadChildren`; NgModule and standalone composition; `HttpClient` call sites; Nx/`angular.json` project boundaries) |
| Python     | `pyproject.toml`, `requirements.txt`, `setup.py`, … (FastAPI / Django / SQLAlchemy aware) |
| Kotlin     | `build.gradle(.kts)` with Kotlin/Android (Compose / Hilt / Room aware) |
| Swift      | `Package.swift`, `.xcodeproj`, `.xcworkspace` (SwiftUI / UIKit aware) |
| Dart / Flutter | `pubspec.yaml` (root or up to 4 levels deep), or any non-generated `.dart` source (pub packages as modules; go_router / auto_route / core `routes:` navigation; `http`, `dio`, retrofit & chopper clients; drift / isar / hive / objectbox / floor / Firestore storage; generated `.g.dart`, `.freezed.dart`, `.mocks.dart` skipped) |
| Ruby       | `Gemfile`, any loose `.rb`/`.rake`, or a Rails **engine** (`config/routes.rb` beside `lib/**/engine.rb`) — Rails routes across every engine and plugin route file, `mount` composed into the mounted engine's own routes, controller actions resolved from `resources`; **Grape** APIs found by transitive inheritance with mount prefixes composed across files; ActiveRecord / Sequel / Packwerk aware |
| Rust       | `Cargo.toml` (workspace or single crate; crate/module/`impl`/trait aware; Axum route DSL aware) |
| Scala      | an sbt/Mill/Maven/Gradle build naming Scala, or any `.scala` source (Play `conf/routes`, Pekko/Akka HTTP and http4s routes; Slick storage; sttp clients; `for … yield` read as a bind, not a loop) |
| C / C++    | `.c`/`.h` (tree-sitter-c) or `.cpp`/`.hpp`/… (tree-sitter-cpp), or `CMakeLists.txt`/`Makefile` + header (per-fact `language`, header/source method merging, namespaces, templates) |
| .NET       | `.sln`/`.slnx`/`.csproj`/`.fsproj`/`.vbproj`, or any `.cs`/`.vb`/`.fs`/`.razor`/`.cshtml`/`.xaml` source (C#, VB.NET, F#, Razor/Blazor, XAML; MSBuild `ProjectReference` as the assembly graph; ASP.NET Core attribute, minimal-API and conventional routing; EF Core/Dapper storage; `HttpClient`/Refit clients; `partial` types merged across files and languages) |
| PHP        | `composer.json`, WordPress markers, or any `.php` source (WordPress / Laravel / Symfony route + outbound HTTP-client aware) |
| Terraform / HCL | any `.tf`/`.hcl` file (blocks as Terraform addresses; prefixed and declared-set bare references; local module sources draw directory dependencies) |
| Ansible    | `ansible.cfg` or a `roles/` directory beside plays (plays → roles by name; `include_role`/`import_role`; templates counted, never rendered) |
| AsyncAPI   | any AsyncAPI 2.x/3.x YAML or JSON spec (channels and producer/consumer operations → messaging topics; local `$ref` and payload-schema identity) |
| OpenAPI    | any spec with an `openapi:` / `swagger:` key |
| gRPC       | any `.proto` file (proto services → routes; TypeScript gRPC-web client calls detected) |
| GraphQL    | graphql-ruby root types (server) + gql tags, `.graphql` operation documents and Ruby operation strings (clients); operation documents activate detection without a TypeScript root |

Framework- and platform-specific detection for each language is described in **[ARCHITECTURE.md → Supported languages](ARCHITECTURE.md#supported-languages)**.

> Python, Ruby, PHP, Rust and Dart are parsed with tree-sitter and contribute call and dependency edges to the graph, so `traverse`, `find_path`, and `impact_analysis` reach into them - not just modules and routes.

</details>

## Staying current

enola releases often. It checks for a new release at most once every 12 hours, in the background, and caches the answer in `~/.enola/update.json` - no command ever waits on the network, and a machine that is offline behaves exactly like one that is up to date. When there is a newer release, `enola check`, `enola --generate` and `enola doctor` say so in one line, and `enola upgrade` installs it.

The notice also states whether the **extractors** changed. If they did, snapshots from the installed
version omit facts available in the current release. MCP sessions receive the same notice once.

It is silent for builds from source, never runs when `CI` is set, and turns off entirely with `export ENOLA_NO_UPDATE_CHECK=1`.

## Learn more

- **[Documentation](docs/README.md)** — choose a guide by task.
- **[Your first graded change](docs/FIRST-CHANGE.md)** — the loop end to end, on a module small enough to read.
- **[Dashboard guide](docs/DASHBOARD.md)** — review changes visually, trace dependencies and verify snapshot provenance.
- **[Architecture](ARCHITECTURE.md)** — the fact model, pipeline, graph, MCP tools and value model.
- **[Changelog](CHANGELOG.md)** — every released version, newest first.
- **[Examples](examples/)** — runnable gates, cross-repository analysis, configuration and CI workflows.
- **[GitHub Action](https://github.com/enola-labs/enola-action)** — grade every pull request against its exact base.
- **[Issue tracker](https://github.com/enola-labs/enola/issues)** — report extraction gaps, unresolved edges and incorrect findings.

## License

Apache License 2.0 - see [`LICENSE`](LICENSE).

The repository contains the complete engine. The binary makes no license checks and sends no
snapshot, fact or usage data off the machine. `enola upgrade` accesses the GitHub release API.

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
