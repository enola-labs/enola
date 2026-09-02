# enola — command-line reference

Every command, flag and setup step. For what enola is and why you would run it, see
[the README](../README.md); for how the engine works, see [ARCHITECTURE.md](../ARCHITECTURE.md).

---

## Quick start

### Install

Grab a prebuilt binary - no Go toolchain or C compiler required:

```bash
curl -fsSL https://raw.githubusercontent.com/enola-labs/enola/main/install.sh | sh
```

This installs `enola` to `~/.local/bin`. If that's not on your `PATH`, add it:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

Binaries are published for Linux, macOS (amd64/arm64), and Windows (amd64). You can also download a specific build from the [Releases page](https://github.com/enola-labs/enola/releases), or [build from source](#build-from-source).

### Upgrade

Once installed, update to the latest release in place:

```bash
enola upgrade
```

This downloads the newest build for your platform, verifies its checksum, and replaces the running binary. If enola is installed somewhere your user can't write, re-run with elevated permissions or re-run the install script above.

Because your agent launches enola as a long-lived MCP server process, an upgrade only takes effect once that process restarts - reconnect the MCP server so it picks up the new binary:

- **Claude Code** - restart the session, or re-register with `claude mcp remove enola && claude mcp add enola enola`.
- **Cursor** - toggle the enola server off and back on in **Settings → MCP** (or reload the window).
- **GitHub Copilot (VS Code)** - restart the server from the `.vscode/mcp.json` editor (the **Restart** CodeLens above the server entry), or reload the window.
- **opencode** - quit and restart it; it loads its configuration once at startup and never reloads it.

### Configuration (optional)

**enola needs no config file.** Every setting has a built-in default, so out of the box it indexes the current repo with all extractors enabled and writes to `.enola/`. A config file (`mcp-arch.yaml`) only *overrides* those defaults - it never adds capability you'd otherwise lack.

Every command prints the config it resolved, on stderr, before it does anything:

```
enola: using config /Users/you/src/api/mcp-arch.yaml
enola: no mcp-arch.yaml in /Users/you/src/api, using built-in defaults
```

It is worth reading. A config decides which extractors run and which paths are ignored, so the wrong one does not fail - it analyses something other than what you asked for. enola looks in the working directory, then (only for a binary that is *not* on your `PATH`, i.e. an unpacked bundle rather than an installed one) beside the executable; the second case says so explicitly.

Note that a list-valued setting **replaces** its default rather than extending it. That is why the bundled `mcp-arch.yaml` declares no `extractors:`, `explainers:` or `renderers:` - a copied list silently falls behind as new ones ship. Set `extractors:` only to deliberately narrow a run; enola warns when an extractor you excluded would have detected the repository.

The install script installs **only the binary**, by design - it does not place a config file. Grab the bundled one from the repo whenever you want to customize (tune the `ignore` globs, pick a subset of extractors, change the output dir, …):

```bash
curl -fsSL https://raw.githubusercontent.com/enola-labs/enola/main/mcp-arch.yaml -o mcp-arch.yaml
```

Third-party code copied into your own source tree is worth adding to those globs: the bundled list covers where dependencies conventionally live — `vendor/`, `node_modules/`, `Pods/` — but not a library vendored into `src/`, which is then part of everything the snapshot says about size, coupling and layering. The `vendored-candidates` explainer reports the ones it can recognise, each with the glob to paste.

The [`examples/`](../examples/) directory has ready-made per-language and multi-repo starting points, and [`examples/full.yaml`](../examples/full.yaml) documents every option. For the full field reference and defaults, see **[ARCHITECTURE.md → Configuration](../ARCHITECTURE.md#configuration)**.

### Connect it to your agent

**Claude Code** - register enola as an MCP server with one command. This assumes the `enola` binary is on your `PATH` (the install script above puts it in `~/.local/bin`):

```bash
claude mcp add enola enola
```

The shape is `claude mcp add <name> <command> [args…]`: the first `enola` names the server, the second is the binary. The trailing config path is **optional** - omit it (as above) to run on built-in defaults, or pass one to override them:

```bash
claude mcp add enola enola /path/to/enola/mcp-arch.yaml
```

When you do pass a config, its `repo:` is only the *default* repository - you can still snapshot any repo by passing `repo_path` to `generate_snapshot`. Verify it registered with `claude mcp list`, then start Claude Code and ask it to generate a snapshot.

**Cursor / other MCP clients** - add enola to your client's MCP configuration. For example, in Cursor's `mcp.json` (the config path in `args` is optional - drop it to use defaults):

```json
{
  "mcpServers": {
    "enola": {
      "command": "enola",
      "args": ["/path/to/enola/mcp-arch.yaml"]
    }
  }
}
```

**opencode** - `enola install --targets opencode` writes the registration itself, because it is already editing the same file to register enola's instructions. It uses an existing `opencode.json` if there is one and otherwise creates `.opencode/opencode.json`, and it leaves a server entry you wrote yourself exactly as it is, in both directions: not overwritten on install, not deleted on uninstall. A `.jsonc` config is skipped rather than rewritten, since the comments in it would not survive. opencode reads its configuration once at startup, so restart it afterwards.

With `--hooks` the same target installs `.opencode/plugin/enola.js`. opencode has no hook configuration in the shape Claude Code and Codex accept, so the plugin does a narrower job: it names the enola tool that answers a structural question in the descriptions of `grep`, `glob` and `list`, repeats that in the system prompt where it also reaches subagents, and refuses the first searches of a session outright with the tool to call instead. That last part blocks, so it is bounded twice: it gives up after two refusals, and it gives up the moment any enola tool is called, including one that failed. `ENOLA_OPENCODE_GATE=off` disables the refusals and leaves the rest.

**GitHub Copilot (VS Code)** - add enola to `.vscode/mcp.json` in your workspace (or your user-level MCP config via **MCP: Open User Configuration**). Note the top-level key is `servers` (not `mcpServers`), and the config path in `args` is optional - drop it to use defaults:

```json
{
  "servers": {
    "enola": {
      "command": "enola",
      "args": ["/path/to/enola/mcp-arch.yaml"]
    }
  }
}
```

Or add it from the command line: `code --add-mcp "{\"name\":\"enola\",\"command\":\"enola\"}"`. Then open a project and ask Copilot to generate a snapshot.

### Use it

Everything below is a prompt you type at your agent in plain English. enola picks the tool.

#### 1. Map it

> "Generate an architectural snapshot of /path/to/my/project"

The snapshot gives your agent all 19 tools (`enola --list`) plus a summary at
`.enola/llm_context.md`. Measured cold and warm times across the public corpus are in
[BENCHMARKS.md](BENCHMARKS.md#4-scale).

#### 2. Understand it

> "I just joined this project - based on the snapshot, give me a tour: the main modules, how they relate, and where to start reading."

> "Draw me a mermaid diagram of the module dependencies from the snapshot."

> "Where are the architectural risks - dependency cycles, layer violations, god classes with high fan-in, call-graph hotspots, complexity outliers, or modules buried deep in the dependency chain?"

> "Which modules have the largest public surface? We're trying to tighten up what's exported."

#### 3. Plan the change

> "I need to add an API endpoint for user preferences. Which packages should I touch, and in what order?"

> "What would break if I refactor `internal/server`? Show me the impact analysis."

> "How does the HTTP handler layer reach the database layer? Show me the shortest path."

#### 4. Make the change - and verify it

This closes the verification loop around the agent's change:

> "Pin the current architecture as a baseline before we start."

> *…now let the agent do the work…*

> "Re-snapshot and show me the architecture diff against the baseline. Did we introduce any coupling or cycles we didn't intend?"

If the diff shows a regression, hand it straight back:

> "You introduced a cycle between `internal/auth` and `internal/session`. Refactor to remove it, then diff again."

Two prompts, no file re-reading, no review meeting. Repeat until the diff is boring.

**The same loop without an agent.** Everything above is also a shell command, so the check can run in a git hook or CI instead of depending on the agent remembering to ask:

```bash
enola baseline pin              # before editing
enola check                     # after - reports the delta, always exits 0
enola check --fail-on=layers    # …or exits 1 on what you named
```

See [The gate - `enola check`](#the-gate---enola-check).

#### 5. Go multi-repo

Generate the first repo, then add the rest with append mode - enola links them into one cross-repo graph:

> "Generate a snapshot of /path/to/go-service with append mode"

> "If I change the auth service, which other services are impacted?"

> "Trace the login flow from the web client through to the backend route that serves it."

> "Which of my backend's endpoints aren't called by any of the client apps?" *(cleanup candidates - check for callers outside these repos first: cron, webhooks, third-party clients)*

> "Which cross-repo calls did enola fail to resolve? I want to know where the map has blind spots."

When you snapshot a *different* repo without `append`, enola assumes you're extending the set and auto-appends it - handy when you forgot `append` on repo #2. If you've actually **moved to another project** and want a clean single-repo snapshot instead, ask for a fresh one (`fresh=true`) so the old repos are discarded rather than merged in.

#### 6. Look at it yourself - without spending a token

Some questions don't need an agent at all. The MCP server also serves a **read-only dashboard** on localhost (URL printed at startup, or run `enola --status`), and it already answers, on one page:

- *What is in this graph right now?* - the repos loaded, services, cross-repo edges (with a node-link diagram), fact and insight counts.
- *What did the analysis find?* - every insight grouped by explainer and filterable by confidence, so you can see the cycles and hotspots without asking a model to list them.
- *How was this snapshot produced?* - the receipt: snapshot ID, enola version, git ref and dirty flag, extractors used.
- *Why does this snapshot look thin?* - extraction quality: files seen vs. parsed vs. skipped, parse errors with samples, unresolved cross-repo edges, coverage gaps.
- *What has this actually saved me?* - the same value estimate `--status` prints, per tool and lifetime ([how it's calculated](../ARCHITECTURE.md#the-value-model)).

Reading it costs nothing and burns no context. It is also the fastest way to inspect a snapshot before relying on an answer built from it.

#### 7. Useful with local and smaller models

Local and smaller models often have less context available for exploring a large repository.
Enola can move structural questions into deterministic graph queries and leave the model
to interpret the result:

- **Less source in context.** A graph query returns the dependents measured within the
  snapshot's scope, so the model can open only the evidence it needs.
- **Graph traversal outside the model.** Questions such as "what depends on this across
  three repositories?" are answered over resolved edges rather than reconstructed from
  a sequence of file reads.
- **Fewer exploration turns.** Avoiding grep-open-read loops can reduce both inference
  time and context use on local hardware.
- **Nothing leaves your machine.** enola is a local binary, the graph is a local file, the dashboard binds loopback only. A fully offline architecture-intelligence stack.

#### Keeping it current

**Regenerate after major changes** so the snapshot stays current. Refreshes are fast: enola caches each language's facts and re-parses a language only when one of its files (or a shared config like `package.json`) actually changed, reusing the rest. If a snapshot does go stale, enola tells your agent so on every tool call - a warning, never a block.

> **Very large repositories (e.g. the Linux kernel).** The first, cold index of a huge repo can take a minute or more and may exceed your MCP client's per-tool-call timeout, surfacing as `MCP error -32001: Request timed out`. The snapshot usually still finishes and is cached server-side - but to avoid the error, either:
> - **Raise your MCP client's tool-call timeout.** In Claude Code, set the `MCP_TOOL_TIMEOUT` environment variable (milliseconds) before launching, e.g. `MCP_TOOL_TIMEOUT=600000`.
> - **Pre-generate from the shell once**, then start the server: run `enola --generate <config-pointing-at-the-repo>` (writes `.enola/`), after which the MCP server auto-loads the cached snapshot on startup and later `generate_snapshot` calls reuse the extractor cache (only changed files are re-parsed), so they return quickly.

---

## The nineteen tools

`enola --list` prints this catalogue from the running binary; it is reproduced here so
you can read it without one installed. You never call these by name — you ask in plain
English and the agent picks — but knowing what exists is what lets you ask for the right
thing.

| Tool | The question it answers |
|---|---|
| `generate_snapshot` | Index a repository and extract its architecture as queryable facts. |
| `explore` | What is in here, and what touches it? The one to reach for first. |
| `query_facts` | Precision filter over the extracted facts. |
| `show_symbol` | Show me the actual source of this named symbol. |
| `traverse` | Walk the dependency or call graph outward from here. |
| `find_path` | How does A reach B? |
| `impact_analysis` | If I change this, what breaks? |
| `endpoint_impact` | Who calls this HTTP endpoint, and which screens sit behind them? |
| `governing_intent` | Which knowledge pages govern this code — and which code does a page govern? |
| `constraints_for` | What am I about to break by editing this file? |
| `plan_check` | What would this change do, before it exists? |
| `coverage_report` | Which cross-repo edges were resolved, and which were missed? |
| `query_insights` | What did the explainers find? |
| `set_baseline` | Remember the architecture as it is now. |
| `diff_snapshot` | What did my change actually do? |
| `snapshot_receipt` | What was this graph generated over, what was excluded, and where are its known limits? |
| `compare_receipts` | Are these two snapshots comparable enough to trust a diff between them? |
| `architecture_history` | How has the architecture changed over time? |
| `architecture_blame` | When did this enter the architecture, and when did it leave? |

The last two are the only ones that answer about the **past**. Everything above them
describes the tree as it is now — `diff_snapshot` included, which compares two nows.

Full parameter reference for each: **[ARCHITECTURE.md → The tools](../ARCHITECTURE.md#the-tools)**.

## Explain a repository at a glance

**Generate a one-screen architecture report with no AI, API key or account.**

`enola --explain [repo_path]` is a one-shot mode that generates a snapshot, computes statistics over the fact graph, and prints a human-readable report to stdout - no MCP server started, no artifacts written to `.enola/`, nothing sent anywhere. Use it for a direct view of the structure enola measured.

**When to use it:**
- **Onboarding onto an unfamiliar codebase** - module count, architecture pattern, hottest packages and where the complexity lives.
- **Evaluating code you didn't write** - a dependency, an acquisition, an open-source project, a contractor's delivery. Cycles, coupling and complexity are hard to hide from a graph.
- **Pre-refactor sanity check** - cycles, layer violations, blast radius of the top modules, before you commit to a plan.
- **CI and audits** - plain text, no color codes, safe to pipe or capture.

```bash
# Use the config in the current directory (mcp-arch.yaml)
enola --explain

# Analyze a specific repository path
enola --explain /path/to/repo

# Report over a whole cluster, from a config that names it with `repos:`
enola --explain ci/cluster.yaml
```

The argument is a **repository** when it is a directory and a **config file** when it is a file, so both forms work without a flag to tell them apart.

**The report covers nine sections:**
- **Overview** - path, analysis time, active languages, total fact count
- **Architectural kinds** - counts of modules, symbols, routes, storage, dependencies, services
- **Relations** - the edge census: declares, imports, calls, implements, instantiates, injects, has_method
- **Symbol breakdown** - functions, methods, structs, interfaces, and other kinds
- **API & data surface** - route count broken down by HTTP method, plus storage count
- **Dependencies** - external, internal, and stdlib import counts
- **Architecture** - detected pattern with confidence, cyclic dependencies, layer violations, cross-repo edges
- **Impact analysis (hotspots)** - top modules ranked by fan-in + fan-out coupling, with criticality tier and blast radius
- **Code health** - per-explainer findings with their top offenders: god classes (high fan-in symbols), call-graph hotspots, deep dependency chains, large public surfaces, and complexity outliers
- **Vendored candidates** - directories that look like in-tree copies of another project, so you can decide whether to exclude them. Nothing is excluded on your behalf; the section is absent when there is nothing to report

Every finding carries a confidence score. Proof-class findings reach `1.0`: dependency
cycles, `intent` set differences, violations of a declared layer order, and breaches of
declared constraints. Heuristics such as god classes and complexity outliers stay below
`1.0`. The analyses use graph algorithms and repository-relative statistics, including
Tarjan's SCC, longest-path and mean+2σ outlier tests. The same source, enola version and
configuration produce the same report. The vocabulary is defined in
**[docs/GLOSSARY.md](GLOSSARY.md)**.

**Vendored candidates.** Airflow vendors nothing, so its report has no such section. Here is one that does — [gmsh](https://gitlab.onelab.info/gmsh/gmsh), a mesh generator that keeps its dependencies in `contrib/`:

```
Vendored candidates (nothing excluded)
  6 directories carrying their own licence under a dependency-named parent (564 files)
    contrib/Netgen                               305 files, cpp, LICENSE, no inbound refs
    contrib/voro++                               78 files, cpp, LICENSE, 1 inbound ref
    contrib/metis                                74 files, c, LICENSE.txt, 1 inbound ref
    contrib/hxt                                  68 files, c/cpp, LICENSE.txt, 44 inbound refs
    contrib/ANN                                  28 files, c/cpp, License.txt, 9 inbound refs
    … and 1 more                                 see .enola/insights.json
    Add any you agree are vendored to `ignore:` in your enola config.
```

Each line is a directory carrying its own licence file under a parent conventionally used for dependencies. That is a hint and not a verdict — the same names are used for first-party code, and gmsh's own `contrib/mobile/` and `contrib/MeshOptimizer/` are correctly absent from the list. **Nothing here has been excluded from the snapshot**: every one of those 564 files is in the graph. The inbound reference count is the part worth reading before you act — `contrib/hxt` has 44 references from gmsh's own code and `contrib/Netgen` has none, which are different decisions. To exclude any of them from future snapshots, add its glob to `ignore:` in your config; the full list, with the globs, is in `.enola/insights.json` and via `query_insights(explainer="vendored-candidates")`.

Here's the actual report for [Apache Airflow](https://github.com/apache/airflow) - a large polyglot codebase (Python, TypeScript, Java and gRPC in one tree) analyzed in a single pass: **68,161 facts, 175,000+ resolved edges, in under 8 seconds** on a laptop (extraction parses files in parallel across cores). Nothing here was written by a model.

```
════════════════════════════════════════════════════════════
 Repository explanation: /path/to/airflow
════════════════════════════════════════════════════════════

Overview
  Generated:           2026-08-04T19:34:16Z
  Analysis time:       7.73310575s
  Languages:           python, typescript, java, grpc
  Total facts:         68161

Architectural kinds
  module                   1261
  symbol                  31083
  route                     463
  storage                    63
  dependency              30771

Relations
  declares                31352
  imports                 30771
  calls                   79630
  implements               4353
  instantiates            29078
  injects                     1
  has_method                  2

Symbol breakdown
  method                  15730
  function                 7870
  class                    4556
  type                     1927
  variable                  808
  interface                 172
  struct                     10
  enum                        6
  constant                    4

API & data surface
  routes                    463
    GET                     265
    POST                     76
    PATCH                    55
    DELETE                   46
    PUT                      20
    HEAD                      1
  storage                    63

Dependencies
  internal                11619
  external                10948
  stdlib                   8203
  unclassified                1

Architecture
  Pattern:             (none detected)
  cyclic dependencies        23
  layer violations            0

Impact analysis (hotspots)
  coupled modules           477
    high criticality        319
    medium criticality      158
  Top hotspots (by coupling):
    module                            fan-in  fan-out crit     blast radius
    providers/common/compat/src/air…    1147       26 high     626
    airflow-core/src/airflow/models      780      344 high     756
    airflow-core/src/airflow             803      104 high     756
    airflow-core/src/airflow/utils       752      145 high     756
    task-sdk/src/airflow/sdk             740       73 high     755
    task-sdk/src/airflow/sdk/defini…     167      194 high     650
    providers/google/src/airflow/pr…      18      337 high     44
    task-sdk/src/airflow/sdk/execut…     186      166 high     755

Code health
  god classes (high fan-in)     25
    dev/breeze/src/airflow_breeze/utils/console… 409 dependents
    providers/google/src/airflow/providers/goog… 339 dependents
    providers/amazon/src/airflow/providers/amaz… 224 dependents
    dev/breeze/src/airflow_breeze/utils/run_uti… 199 dependents
    airflow-core/src/airflow/utils/helpers.prun… 175 dependents
  call-graph hotspots       173
    providers/google/src/airflow/providers/goog… fan-in 339 / out 5
    task-sdk/src/airflow/sdk/bases/operator.Bas… fan-in 14 / out 101
    dev/breeze/src/airflow_breeze/utils/run_uti… fan-in 199 / out 7
    providers/google/src/airflow/providers/goog… fan-in 116 / out 12
    airflow-core/src/airflow/serialization/seri… fan-in 16 / out 65
  deep dependency chains     10
    providers/common/ai/src/airflow/providers/c… depth 8
    providers/common/ai/src/airflow/providers/c… depth 8
    airflow-core/src/airflow/ui/src/components/… depth 7
    providers/airbyte/docs                       depth 7
    providers/akeyless/docs                      depth 7
  large public surfaces      20
    task-sdk/src/airflow/sdk/execution_time/com… 120/133 (90%)
    task-sdk/src/airflow/sdk/definitions/mapped… 100/111 (90%)
    airflow-ctl/src/airflowctl/api/operations    94/101 (93%)
    providers/google/src/airflow/providers/goog… 67/72 (93%)
    dev/breeze/src/airflow_breeze/utils/packages 62/68 (91%)
  complexity outliers        15
    airflow-core/src/airflow/jobs/scheduler_job… complexity 66
    task-sdk/src/airflow/sdk/execution_time/sup… complexity 63
    airflow-core/src/airflow/ui/src/pages/DagsL… complexity 55
    airflow-core/src/airflow/ui/src/hooks.useDa… complexity 53
    airflow-core/src/airflow/api_fastapi/core_a… complexity 52
```

No artifacts are written; `.enola/` is not touched. For a persistent snapshot with agent-readable output, use `--generate` or the MCP server.

For interactive per-module blast-radius queries with configurable depth, see the `impact_analysis` tool reference in **[ARCHITECTURE.md → The tools](../ARCHITECTURE.md#the-tools)**.

---

---

## Command-line reference

Run `enola --help` for the full text. With no flags, enola starts the MCP server on stdio.

Every path argument follows the same rule: **a directory is a repository, a file is a config.** Anything that is neither is rejected rather than silently ignored.

| Command | What it does |
|------|--------------|
| `install [--hooks] [--global]` | **Tell your coding agents enola is here.** Writes its instructions into the files they actually read - `.claude/rules/enola.md`, `.cursor/rules/enola.mdc`, and a marked block in `AGENTS.md` if you have one. Previews every change and asks before writing. See [Wiring it into your agents](#wiring-it-into-your-agents---enola-install). |
| `coverage [--repo=<svc>] [--unresolved] [--json]` | **Which cross-repo edges enola resolved, and which it did not** — per service, so you can tell a genuinely isolated service from one whose outbound edges enola could not follow. The unresolved list is always shown: it is what makes the resolved count worth believing, and each entry is either a repository you have not loaded, a third-party endpoint, or a blind spot in enola. Needs two or more repositories in one graph. A report, not a gate — it always exits `0`. |
| `doctor [repo]` | **Are the session hooks actually firing?** `install --hooks` writes a hook configuration and reports success — but whether your agent honours that configuration is a contract owned by the agent, not by enola, and a config it ignores looks identical to one it runs. So the hooks record every time they fire, *including* the runs where they deliberately say nothing, and this reports when each last ran, what it concluded, and whether the pinned baseline can still be graded against at all. `NEVER FIRED` after a real session means the wiring is not working. A report, not a gate — it always exits `0`. |
| `dashboard [--open] [repo\|config]` | **Explore the latest snapshot visually without starting MCP.** Starts the read-only localhost dashboard attached to the terminal until Ctrl-C; `--open` also launches the browser. |
| `providers list \| fetch <name>` | **The fact providers this binary carries itself.** `list` names each built-in and whether it is ready; `fetch rubydex` downloads the pinned Rubydex engine library from rubygems.org, verifies its published sha256, and caches it under the user cache directory, after which a `providers:` entry named `rubydex` with no command runs in-process. The only network access a provider ever makes, and never at snapshot time. |
| `uninstall [--global]` | Remove everything `install` wrote, leaving the rest of each file byte-for-byte as it was. |
| `baseline pin\|show\|clear [repo\|config]` | Manage the diff baseline - the "before" a change is graded against. `pin` freezes the snapshot on disk when every repository's own receipt shows it matches the working tree under this build and config (members of a cluster must also agree on one union), and otherwise snapshots first, linking once on the cluster's last turn, and says which repository made it regenerate; `show` reports what the current baseline describes; `clear` removes it. Stored per repository, in that repo's `.enola/baseline`, so several repos each keep their own. |
| `check [flags] [repo\|config]` | **Grade what a change did to the architecture**, and exit with a code CI can act on. Read-only: writes nothing and leaves the baseline in place, so it can be run repeatedly. See [The gate](#the-gate---enola-check). |
| `constraints <lint\|mine\|init\|explain\|ledger> [repo\|config]` | **The authoring loop.** `lint` validates the declared vocabulary and resolves each component against the current snapshot; `mine` proposes candidate rules out of the snapshot's own regularities; `init` writes a first declaration binding every shipped recipe whose required roles resolve to directories the repository has, refusing to overwrite and guessing nothing; `explain <path>` names the components a file's facts belong to, the selector that admitted each, and the edges the file makes; `ledger` reports how much of the declared law is being EXCUSED rather than obeyed — each rule's breaches beside the suppressions and exemptions that signed them away, with every excuse's owner, reason and age, and the ones that now match nothing. `lint` exits `0` when every declaration is valid, `1` when it reported validation problems; `mine`, `init`, `explain` and `ledger` exit `0` whenever they produced a report, and `2` when they could not run — no snapshot to read, or a declaration `init` would have had to overwrite. See [CONSTRAINTS.md](CONSTRAINTS.md). |
| `endpoint [flags] <endpoint> [repo\|config]` | **What changing an HTTP endpoint reaches.** The controller serving it, the models that controller touches, the models associated with those, the tables behind them, and the callers - including the frontend screen a calling route module implements. The endpoint is matched as a substring of the path, optionally prefixed with a verb (`GET /v1/candidates`, or just `/v1/candidates`). Client call sites and mock-server routes are excluded: this answers about what the application serves. `--json` emits the report; `--max-routes` bounds how many matched endpoints are followed. The `endpoint_impact` MCP tool answers the same question in a session. |
| `plan [flags] [path...] [repo\|config]` | **The pre-edit contract.** Which declared constraints govern an intended change (`--paths`, `--symbols`), its blast radius over the current snapshot, and — for a `--patch` — the constraint verdicts that WOULD appear, evaluated over a scratch copy before any edit lands in the tree. Nothing is written; a report, never a gate. Exits `0` on any produced report, `2` when it could not run. See [CONSTRAINTS.md](CONSTRAINTS.md). |
| `log [flags] [repo\|config]` | **What has this architecture done over time?** One line per snapshot enola recorded, oldest first, with what changed since the one before it - `--graph` draws the branch topology, `--stat` breaks each delta down by fact kind, `-n` and `--since` bound the window. Read-only: it reports what was observed and never snapshots to fill a gap. `--backfill` instead BUILDS the timeline from the repository's own commit history, so a repository enola has never seen still has a past to read. **Experimental.** See [HISTORY.md](HISTORY.md). |
| `show [rev] [repo\|config]` | **What did THIS revision do?** `log` says a revision added twelve facts; this says which twelve. Reconstructs the revision and its predecessor out of the stored history and compares them, so a past change is described in the words it was described in at the time. A revision is a snapshot id or prefix, a git commit, `HEAD~3`, `@7`, a ref name, or `latest` (the default). **Experimental.** |
| `diff <a>..<b> [repo\|config]` | **What happened between these two points?** The architecture delta across a range - the question a week of work produces, where `show` answers for a single revision. Either side of the range may be empty, meaning the oldest or the newest recorded revision. **Experimental.** |
| `blame [flags] <pattern> [repo\|config]` | **When did this enter the architecture, and when did it leave?** A question a snapshot cannot answer however good it is, because it is about the past. The pattern matches a module or symbol name, a file path, or both endpoints of an edge; `--findings` searches recorded findings instead ("which snapshot introduced this cycle?"), and `--first` stops at the introduction. Revisions whose stored contents have aged out are reported as `unsearched`, never as absent. **Experimental.** |
| `gc [flags] [repo\|config]` | **What is stored, and what can go?** Reports how many revisions the history holds, how many can still be replayed, and how much disk they cost. With no flags it removes only garbage - segment directories no revision refers to. `--thin-older-than=90d` drops old contents while keeping the timeline complete, and `--prune-working` discards uncommitted-tree snapshots; each has to be asked for, because both lose something a reader could still reach. **Experimental.** |
| `history <push\|pull\|verify\|gc> [store] [repo\|config]` | **Share a history between machines** through a directory store - a git repository, a shared mount, an S3-synced folder. Plain files, content-addressed, tamper-evident. `push` copies local revisions in, `pull` imports what other machines pushed, `verify` walks every chain and names gaps and tampering (exits `1` when it finds a problem), and `gc` applies retention - printed first, deleted only with `--apply`, and recorded in the chain. Point it with `history.shared_dir` or the first argument. **Experimental.** |
| `upgrade` | Download and install the latest release over the running binary. |

| Flag | What it does |
|------|--------------|
| `--generate [repo_path\|config_path]` | Generate a snapshot and exit - no MCP server. Artifacts go to `output.dir` (default `.enola/`). With `repos:` in the config, indexes the whole cluster in one run. |
| `--explain [repo_path\|config_path]` | Print the statistics report above and exit. Read-only: nothing is written to `.enola/`. A directory is a repository; a file is a config, so a `repos:` config reports over the whole cluster. |
| `--list` | List the MCP tools this build serves, with one-line summaries. |
| `--status` | List every enola server running right now - PID, repos, uptime, calls, dashboard URL - plus per-tool call counts and an estimate of the reconstruction those calls saved, in time and tokens. |
| `--status --all` | The same usage, broken down per repository. |
| `--no-dashboard` | Start the MCP server without the localhost dashboard. |
| `--version` | Print the build version. |
| `--version --json` | Print the build version and the extractor version as JSON, on stdout. This is the release manifest — see [Staying current](#staying-current). |
| `--help`, `-h` | Show usage. |

### Staying current

enola releases often, so it tells you when you are behind — without ever making you wait on the network.

A background check runs at most once every 12 hours and caches its answer in `~/.enola/update.json`. Every notice you see is a read of that file, so no command ever blocks on a request, and a machine with no network behaves exactly like one that is up to date. The check itself runs only where nothing is waiting on it: a detached child process any enola command starts when the cache is due, the session-start hook, and the MCP server's startup. The child is what makes the notice reach a shell-only install - one that has no agent hooks and never starts a server still gets told.

When a newer release exists, `check`, `--generate` and `doctor` print one line on **stderr** (never stdout, so `--json` output stays a clean document), and `enola upgrade` installs it. A command that fails outright prints it too, after the error: when the extractors have moved, an old build detecting no language is not a fact about your repository, and `snapshot produced no facts` deserves the line that explains it.

```
enola v0.3.12 is available (you have v0.3.2) — extractors changed since your build,
so this graph is missing facts a current enola would extract. Run `enola upgrade`.
```

The second clause is the only detail the notice carries, and it is the one worth acting on. **Extractor version** is `internal/engine.cacheVersion`, the constant bumped whenever an extractor starts reading something differently — the same value every snapshot records as `extractor_version`. When it has moved, your build is not merely older: the graphs it produces are missing facts a current enola would extract. When it has not, the clause is absent.

Deliberately, the notice does **not** summarise what changed. Release titles are narrow, so any one headline undersells a ten-version gap and oversells a one-version gap — and the changes that matter most are the least visible, since a release adding one language while fixing another language's routing reads as neither to the person who needed the fix.

Your agent gets the same information once per session, appended to its first MCP tool result. That wording is different on purpose: it states the fact, assigns the decision to you, and names no command. An agent told to run `enola upgrade` would run it mid-task on a machine it was not asked to modify — and would achieve nothing visible, because replacing the binary leaves the already-running server on the old one.

The check never runs for a build from source (it is ahead of the last release, not behind it), never when `CI` is set, and not at all with:

```bash
export ENOLA_NO_UPDATE_CHECK=1
```

`enola doctor` reports the standing answer either way, including when it is "up to date".

### Wiring it into your agents - `enola install`

An MCP server your agent forgets to use is a tool you don't have. `enola install` writes a short instruction into the files your agents already read, so they know the graph is there and what it's for:

```bash
enola install                 # this repository (shared with the team via source control)
enola install --global        # this user, every project
enola install --dry-run       # show what would change, write nothing
enola install --yes           # skip the confirmation prompt (for scripts)
enola install --targets=claude,cursor   # a subset instead of every detected agent
enola uninstall               # remove it all again
```

| Target | Local (this repo) | Global (`--global`) |
|---|---|---|
| Claude Code | `.claude/rules/enola.md` *(owned)* | `~/.claude/rules/enola.md` *(owned)* |
| Cursor | `.cursor/rules/enola.mdc` *(owned)* | — no user-level rules directory |
| GitHub Copilot | `.github/instructions/enola.instructions.md` *(owned)* | — lives in IDE/account settings |
| Codex · Copilot · Pi | `AGENTS.md` *(marked block, only if it already exists)* | — |
| Codex | `.codex/hooks.json` *(managed entries, `--hooks` only)*, covered by `AGENTS.md` otherwise | `~/.codex/AGENTS.md` *(marked block)*, `~/.codex/hooks.json` *(managed entries, `--hooks` only)* |
| Pi | *covered by `AGENTS.md`* | `~/.pi/agent/AGENTS.md` *(marked block)* |
| opencode | `.opencode/enola.md` *(owned)* + its entry in `opencode.json`, or covered by `AGENTS.md` | `~/.config/opencode/enola.md` *(owned)* + its entry in `~/.config/opencode/opencode.json` |
| opencode | `mcp.enola` in the same config *(the one target that registers the server itself)* | same |
| opencode | `.opencode/plugin/enola.js` *(owned, `--hooks` only)* | `~/.config/opencode/plugin/enola.js` *(owned, `--hooks` only)* |

**Codex, Copilot, Pi and opencode all read the repository's `AGENTS.md`**, so locally one block serves all four - enola won't write a second repo-local file for them, which would only put the same instruction into the same context window twice. Their `--global` entries add what `AGENTS.md` can't: guidance in projects where nobody has run `enola install`. Those are written only when the tool's config directory already exists, so enola never creates `~/.codex` for someone who doesn't use Codex.

**`--targets` is for narrowing, not for choosing.** The default is every target, and that
is almost always what you want: each one writes only into files its own agent reads, and
a target whose agent is not installed skips itself and says so, so installing "everywhere"
costs nothing but a few small files in your repository. Reach for `--targets` when you
have a specific reason - you are trying one agent out, a colleague objects to a directory
appearing in the repo, or you are re-running after fixing one target's configuration:

```bash
enola install --targets=claude,copilot   # only these two
enola install --targets opencode         # only opencode
```

One consequence worth knowing, because it is not obvious: enola skips a target's own
instruction file when the repository's `AGENTS.md` already carries the block, since
writing both would put the same paragraphs into the same context window twice. With
`--targets opencode` the `agents` target is not part of the run, so that check asks
whether the block is actually in `AGENTS.md` rather than whether the file exists - a
narrowed run never leaves an agent with a registration and no instructions.

**It never surprises you.** Every run previews what it will touch and asks before writing. It never creates an `AGENTS.md` that wasn't already there. Re-running reports `unchanged` rather than churning files. And `uninstall` restores shared files byte-for-byte - the block is delimited by explicit `<!-- enola:begin -->` / `<!-- enola:end -->` markers, and if those markers have been hand-edited into an unbalanced state, enola refuses to write rather than guess where its section ends.

#### Closing the loop automatically - `--hooks`

```bash
enola install --hooks
```

Without it you get instructions and nothing else: a paragraph telling the agent the graph
exists, which it is free to read and then ignore. That is the honest description of the
default, and on a small local model it is often what happens. `--hooks` is what makes the
loop run whether or not the agent remembers to.

In Claude Code and Codex that means two session hooks:

- **`SessionStart`** freezes the architecture as a baseline when a session begins - the "before".
- **`Stop`** grades what the session changed when your agent finishes a turn, and hands the verdict back **only if** there is something to say: a structural regression under the policy you set, or - since the default policy is empty - a finding enola measured exactly and did not enforce.

**In opencode it means something different**, because opencode has no hook configuration
of that shape. There `--hooks` installs `.opencode/plugin/enola.js`, which works on the
other end of the session: rather than grading the change afterwards, it pushes the first
move towards the index. See the opencode section above for what it does and how to turn
the blocking half off. Neither the baseline nor the grading is installed there, so
`enola doctor` is not part of that setup and will not report on it.

The agent gets a chance to fix the layer it crossed before telling you it's done, rather than you finding it in review.

**When nothing was enforced, the decision stays yours.** The verdict handed to the agent says the run exited `0`, that this is a report rather than a failed build, and that whether the change is acceptable is not the agent's call: it is told to show you the findings and ask - accept it, change it, or set a policy that would fail on it next time - and explicitly not to revert or refactor on its own initiative, nor to describe the session as clean without mentioning them.

It is deliberately opt-in and deliberately quiet:

- **Session start is never delayed.** The baseline snapshot runs detached, so the hook returns in milliseconds whether your repo takes 0.2 seconds or two minutes to index. A timeout would only *cap* that cost; detaching removes it.
- **Your own baseline is never replaced.** A baseline you pinned yourself - or that your agent pinned with `set_baseline` - is left alone. Only one enola pinned automatically is refreshed, and only when the tree has actually moved.
- **Several open terminals do one snapshot, not six.** The pin is single-flight across processes; sessions that arrive while one is running do nothing.
- **Silent unless it matters.** No baseline, nothing changed, an architecture that moved without producing a single finding, an incomparable baseline - all produce no output at all. The gate speaks when the change regressed the architecture under your policy, or when it introduced a finding enola computes exactly (a declared-layer violation, an intent mismatch, a cycle) that no policy enforced. Estimates below the confidence floor never trigger it, or a re-ranked hotspot list would become a session report.
- **It never blocks.** The verdict is context the agent can act on, not a wall it has to get past.
- **It never breaks your session.** Every failure path - no snapshot, unreadable input, a directory that isn't a repository - exits cleanly and says nothing. A broken enola must never look like a broken session.
- **It merges into your config.** Your existing hooks, permissions and settings are preserved; `uninstall` removes exactly enola's entries and nothing else.

The hooks shell out to `enola hook session-start` and `enola hook stop`, which is what you'll see in `.claude/settings.json` (or Codex's `hooks.json`), pinned to the absolute path of the binary you installed with. You never run them yourself.

Codex additionally requires you to approve a new hook once before it will run it - inside Codex, run `/hooks`. That's a Codex requirement, not something `enola install` can do for you.

### The gate - `enola check`

`diff_snapshot` answers "what did my change actually do?" for an agent. `enola check` asks the same question from a shell, and turns the answer into an **exit code** - so the same delta can gate a commit or a CI job with no agent in the loop.

```bash
enola baseline pin /path/to/repo    # 1. freeze how it looks now, BEFORE editing
#   …make your changes…
enola check /path/to/repo           # 2. grade what they did
```

The baseline is a pinned artifact rather than "whatever state the tool last held" - it survives re-snapshots, publishes atomically, and travels to another machine. Why the graph works that way at all: **[docs/SNAPSHOTS.md](SNAPSHOTS.md)**.

**`pin` never pins a stale snapshot.** If the repository holds no snapshot, or its tree has moved since the one it holds, `pin` regenerates first and says which repository made it do so, and why:

```
enola baseline: regenerating, shopfront holds no snapshot
enola baseline: regenerating, shopfront moved since its snapshot: 1 modified (e.g. storage/storage.go)
```

That is what makes step 1 safe to run without thinking about it: the "before" is always the tree you were looking at when you typed it, not whenever enola last happened to index. For a cluster, every member must agree on one union before the pin stands, so a config with `repos:` snapshots what it must and links once on the cluster's last turn.

A baseline is stored **per repository**, in that repo's own `.enola/baseline`, so several repositories each keep their own and pinning one never disturbs another. `baseline show` reports what the current one describes - when it was generated, over which repo, how many facts, and its snapshot id - and `baseline clear` removes it.

**Pinned, or one step back?** `check` grades against the pinned baseline. `--baseline=previous` grades against the preceding snapshot instead - the `previous/` set that rotates every time a snapshot is written. The two answer different questions, and diverge as soon as more than one change has landed:

```
$ enola check                        # against the pin, three edits ago
  symbols      +3
  Added (3):
    symbol     api.ChangeA                                  api/api.go:14
    symbol     web.ChangeB                                  web/web.go:11
    symbol     notify.ChangeC                               notify/notify.go:12

$ enola check --baseline=previous    # against the last snapshot written
  symbols      +2
  Added (2):
    symbol     web.ChangeB                                  web/web.go:11
    symbol     notify.ChangeC                               notify/notify.go:12
```

Pin when you start a piece of work and the answer stays "what has this branch done since I began", however many times you re-snapshot in between - which is why it is the default and why a multi-day refactor only warns rather than declining. Reach for `previous` when the question is narrower: what did the run that just finished change?

| Exit | Meaning |
|------|---------|
| `0` | **clean** - nothing the policy enforces (which, with no `--fail-on`, is everything) |
| `1` | **regression** - the policy was violated |
| `2` | **error** - the gate could not run (no baseline pinned, bad argument, inverted snapshot pair) |
| `3` | **declined** - the baseline is not comparable, so it refused to grade |

`3` is deliberately not `1`. When the two snapshots were built over different inputs - a different enola version, a different extractor set, changed ignore globs, or the same repository indexed under two different labels - the delta describes *how they were produced*, not what you edited. Reporting that as a failing change would be a lie, so the gate says it declined and why.

**Repository labels are part of that.** Every fact carries the label of the repository it came from, and a diff matches facts on it, so two snapshots that label one repository differently share no facts at all. The label is the repository's name from its git remote when the indexed directory is that repository's root - so a worktree, a second clone, and a CI checkout all agree - and the checkout directory name otherwise. When the two sides disagree anyway, the gate declines with `repo_label` rather than reporting your whole repository as rewritten.

**When the only mismatch is who produced the facts, the gate grades the intersection instead of declining.** A baseline taken before the repo's first `enola_intent:` page lacks the `mdintent` extractor; a provider whose tool is missing on one machine ran on one side only. Both used to skip the whole verdict - exactly on the PRs that introduce the measurement machinery. Now the gate grades only facts from producers present in BOTH snapshots and says so, unmistakably: the headline reads `PASS (partial verdict)` or `FAIL (partial verdict)` (JSON status `partial_clean` / `partial_regression`, with an `intersection_grading` object), every excluded producer is named with the side that lacks it and how many of its facts and findings went ungraded, and the verdict states outright that a regression among an excluded producer's facts is NOT reported. Exit codes stay `0`/`1`, so CI needs no change. A provider's facts are attributed by their stamped `provider` prop; an extractor's by its declared file ownership - a disputed extractor that declares none keeps the exit-`3` decline, with the reason printed. Everything that corrupts fact identity itself - a different enola version or build, a different repository, changed ignore globs - still declines: there is no sound intersection to grade.

**A stale baseline warns; it never blocks.** Past three days it tells you exactly how stale and what that means (the delta now also contains whatever the repo itself changed in between) - then grades anyway, because a long-lived baseline is a legitimate way to measure a multi-day refactor and only you know which you meant.

**Nothing fails by default.** A bare `enola check` runs all nineteen explainers, reports every finding the change introduced, and exits `0` - saying in its own output that no policy was in effect, because a gate that enforces nothing must never be mistaken for a gate that found nothing. What breaks the build is what you name:

```bash
enola check --fail-on=layers                               # fail on a declared layer order
enola check --fail-on=constraints                          # fail on a declared rule breach
enola check --fail-on=layers,cycles,intent                 # …and cycles, and undeclared seams
enola check --fail-on=god-class --min-confidence=0.8       # an inferred one needs a lower floor
enola check --fail-on=layers --warn-only                   # enforce, but only warn this time
enola check --json                                         # machine-readable verdict
enola check --format=sarif > enola.sarif                   # SARIF 2.1.0, one result per finding in every bucket
enola check --format=annotations --host=buildkite          # one entry per positioned finding, for buildkite-agent annotate
enola check --format=annotations --host=github             # the same as workflow commands, for Actions
enola check --detail                                       # full delta under the verdict
enola check --baseline=previous                            # compare against the preceding snapshot
enola check --focus=internal/auth                          # narrow the delta to what you touched
enola check --write                                        # also persist the snapshot (default: read-only)
enola check --reviewers                                    # who owns what you touched, and who should review it
```

### Who should review this — `--reviewers`

`--reviewers` adds one section to the verdict: for each module the change touched, who owns
it, and — where you barely know the code you just edited but own something that imports it —
who to ask.

```
Reviewers for this change (1) — authorship over the last 500 commits;
steering, never graded:
  pkg/auth — owner: Bob (100%), 0 minor contributor(s) of 1
      you are a minor contributor here (0%)
      you own pkg/tokens (100%), which imports pkg/auth  [major-minor-dependency]
      suggested reviewer: Bob
```

> The `major-minor-dependency` — owning a component while being a stranger to one it
> depends on — is from Christian Bird, Nachiappan Nagappan, Brendan Murphy, Harald Gall and
> Premkumar Devanbu, [*Don't Touch My Code! Examining the Effects of Ownership on Software
> Quality*](https://www.microsoft.com/en-us/research/publication/dont-touch-my-code-examining-the-effects-of-ownership-on-software-quality/)
> (ESEC/FSE 2011). They found it in 52% of Windows Vista binaries, well above a randomised
> null model, and recommended telling the owner what you need instead of making the change
> yourself. The 5% line between major and minor contributors is theirs.

Spotting it needs the import graph as well as the commit log, which is why it lives here
rather than in a tool that reads only git.

**Off by default.** Without the flag no author name is read or printed. The measurement is
one `git log` over the last `--reviewer-window` commits (500), so it costs the same at any
repository age and nothing is cached. Names come from `%aN`, so `.mailmap` applies.
`--author` overrides `git config user.name`; an actor with no commits in the window is
named once and only owners are reported.

**Never graded.** It cannot change the verdict, the exit code, or what `--fail-on` fails on.
It moves with your git history rather than your code, and the 5% line is a correlation
measured on Vista binaries, not a rule you declared. A module with fewer than five commits
in the window names no owner and carries no `major-minor-dependency`; at one commit somebody
holds 100% of it. Expect the clearest signal where components have their own teams — in a
single library maintained by one core team, most people are minor to most modules.

Text and JSON only. SARIF and the annotation hosts place entries on diff lines as problems,
and this is neither.

**The verdict has four writers, and all four read the same verdict.** `--format` picks `text` (the default), `json` (what `--json` means), `sarif` or `annotations`; nothing is recomputed for a writer, so a fifth is a table row. SARIF carries one rule per declared rule id with the team's `because` as its description, one result per finding in every bucket (failures as errors, advisories and declared findings as warnings, resolved findings at no level and with no region, suppressed and exempted findings with the ledger entry or exemption that excused them), the evidence span as the region, and the finding's stable identity under `partialFingerprints.enola/v1`. Annotations place every finding that has a measured position on its file and line, as Buildkite markdown grouped by file (`--link` takes the pull request's files view so each line links into the diff) or as GitHub workflow commands; findings without a position are counted at the end and never placed. The host is a flag and never read from the environment, so a run on a laptop renders exactly what the run in CI rendered.

The nineteen names `--fail-on` accepts are `cycles`, `layers`, `intent`, `constraints`,
`crossrepo`, `coverage`, `unused-routes`, `messaging-coverage`, `god-class`, `hotspots`,
`dependency-depth`, `exported-surface`, `complexity-outliers`, `domain`, `query-loops`,
`entry-points`, `dead-methods`, `vendored-candidates` and `import-closure`.

**A name it does not recognise is refused, not ignored.** `--fail-on=cyles` exits `2`
and names what it could not match, rather than exiting `0` while enforcing nothing —
a misspelled gate would otherwise be indistinguishable from a passing one in CI.
Matching is exact, so `CYCLES` is refused too: case-insensitivity here would be a
guess about which explainer you meant, and this flag exists to remove guesses about
what fails. A spec mixing valid and invalid names is refused whole, because enforcing
the valid half is the same defect wearing a smaller number.

**Declaring what you meant to change.** The flags above grade the delta. `--target` grades
it against your *intent*: reverse-dependency impact analysis runs on the pre-change graph,
and any package the change reached outside that predicted radius is reported as
**spillover** — a package altered by something your description did not cover.

```bash
enola check --target=internal/auth                    # what should this change have reached?
enola check --target=internal/auth --expected=cmd/api # …plus a package you know you touched
enola check --target=internal/auth --max-spillover=0  # and fail if it reached anywhere else
```

```
## Scope

**Reached beyond the declared scope.** 1 of 2 package(s) touched were predicted or
declared, match ratio 0.5.

Spillover — touched but neither predicted nor declared:
  - unrelated

Predicted but not touched (usually fine — the change was narrower than its blast radius):
  - api
```

Spillover is **reported, never failed, until you ask for it**: `--max-spillover=N` allows
up to N and fails above that, so `--max-spillover=0` means "fail on any". A scope check
that broke the build the first time someone passed `--target` would only teach people not
to pass it.

This is the one question a delta cannot answer on its own. A diff is a function of two
snapshots, so it can say what changed and nothing about what you *meant* to change —
spillover needs that third input.

The output names what moved rather than counting it - the added symbols with their `file:line`, the new coupling with its relation kinds, and any finding whose content shifted:

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
```

That is `enola check --fail-on=layers` on [`examples/layers-gate/`](../examples/layers-gate/), verbatim. Without the flag the same run prints the same finding under `New findings (reported — no failure policy set)` and exits `0`.

Lists cap at 12 entries with a `--detail` pointer, and `declares` edges - the mechanical one-per-new-symbol link to their module - always sort last, since they say nothing about what got coupled.

**Baselines are portable.** A baseline is identified by the repository's normalized git remote (falling back to the checkout directory name), not by the absolute path it was pinned at - so one pinned on a CI runner grades against a checkout anywhere else. That's what makes the CI shape cheap: the default branch publishes `.enola/baseline/` once, every PR restores it and diffs against it, and no job ever indexes the base a second time.

Ready-made wiring: [`examples/hooks/pre-commit`](../examples/hooks/pre-commit) (blocks only on exit `1`; a missing or incomparable baseline skips the gate rather than blocking someone over setup they haven't done) and [`examples/ci/architecture-gate.yml`](../examples/ci/architecture-gate.yml) (publish-on-main, restore-on-PR).

### What it saved you - `--status`

enola isn't just a token saver, but it is one, and it keeps score. `--status` shows every enola server running right now, what your agents have actually called, and an estimate of what those calls replaced:

```
=== enola MCP Status ===
Servers running: 2

      pid  repos                        uptime   calls  dashboard
    59903  api, web, mobile            57m 12s      42  http://127.0.0.1:56730 (shared)
    60122  auth-service                12m 04s       8  http://127.0.0.1:56744
Tracking since: 2026-07-21 11:54:19
Repos tracked: 21

Tool Usage:
  tool                running     total
  explore                   1         1
  generate_snapshot         1         1
  query_facts               1         1
  query_insights            1         1

Value Estimate (approximate):
  tool                calls   ~time saved   ~tokens saved
  explore                 1            6s           11.2K
  generate_snapshot       1  21h 48m 52s           130.9M
  query_facts             1            3s            6.3K
  query_insights          1           14s           23.4K
  TOTAL                   4  21h 49m 16s           130.9M†
  running now             4  21h 49m 16s           130.9M

  † corpus exceeds a single context window — not reproducible by re-reading files.
```

That's a single session over the **Linux kernel** - 218M tokens of C and Rust across 55,399 parsed files, indexed into 1.9M facts in 2m20s. The 130.9M is exactly `218M × 0.6`: priced from the corpus, not from the fact that one call happened. And the dagger is doing more work than the number is - at 218× a context window, that graph isn't expensive to rebuild by reading files, it's *impossible* to.

Run the same session again over an unchanged repo and it collapses to a few thousand tokens: the snapshot ids match, so each call earns confirmation credit instead of a rebuild. Building an understanding and confirming one still holds are different things, and the estimate says so.

`--status --all` gives the same figures broken down **per repository**, sorted by tokens saved - useful for seeing which part of your estate the tooling is actually earning its keep on.

Be clear about what these numbers are. They answer one question: **what would an agent have had to ingest to reach the same answer with ordinary tools - grep, glob, open a file, read it, infer?** So a snapshot is priced from the *corpus it indexed*, measured, not from the fact that a call happened; a 17.9K-token service and the 218M-token Linux kernel are not the same act of work, and no flat per-call price is right for both. Reading time converts to your time waiting on the agent, including the rework a non-deterministic reconstruction implies. Failed calls are counted but earn nothing, and the tokens you spend reading enola's own response are subtracted - so `output_mode='summary'` genuinely scores better than `'full'`.

They're an estimate, labelled as one - but the inputs are real: corpus sizes measured at snapshot time, call counts recorded per repository under `~/.enola/usage/`. They survive server restarts and deleting a repo's `.enola/` directory, and `--status` works from any directory, not just a snapshotted one. The full model, its constants and what it deliberately leaves out are in [ARCHITECTURE.md](../ARCHITECTURE.md#the-value-model).

**The dagger matters more than the number.** When the corpus exceeds what an agent can hold at once - as an 8-repo ecosystem or a large monorepo does - the counterfactual isn't expensive, it's *impossible*: cross-repo edges can't be derived by re-reading files when both sides can't be in context together. Those rows are flagged, because "not reproducible by re-reading" is a stronger claim than any figure.

And the estimate stays conservative where it can't measure. It prices the ingestion an agent avoided and a slice of the rework; it doesn't try to price the missed caller found in code review, or the afternoon spent reconstructing how two services talk. That's the saving `impact_analysis` and `diff_snapshot` are really for, and it's the one you'll feel first.

### The dashboard

For a task-oriented walkthrough of every tab, with screenshots, see
**[DASHBOARD.md](DASHBOARD.md)**.

Open the latest snapshot directly:

```bash
enola dashboard --open
```

This serves only the dashboard and stays attached to the terminal until Ctrl-C. An MCP server is not required. Startup prints the selected snapshot directory and complete `http://` URL. Use `enola --status` in another terminal to list running sessions and their dashboard URLs. Starting the MCP server also starts the same **read-only dashboard** on a free loopback port (`127.0.0.1`), printed to stderr at startup. A standalone dashboard checks for newer snapshots written to its selected directory and reloads the page when one appears.

The dashboard is organized into six tabs:

- **Overview** — architectural change cards, current facts, findings, repositories, services, and cross-repository edges.
- **Architecture** — a searchable module graph, focused consumer/dependency neighborhoods, edge evidence, findings, and a synchronized module table.
- **Snapshots** — freshness, Git capture, extractor schema, repositories in the graph, and the complete technical receipt.
- **Lifetime usage** — retained tool totals and estimated value across repositories and completed sessions, kept separate from current-snapshot data.
- **Diagnostics** — active processes, dashboard URLs, paths, ports, and this page's runtime identity.
- **Quality** — complete file accounting, expected exclusions, potential blind spots, inactive extractors, parse failures, and unresolved cross-repository connections. Genuine parse failures include a prefilled GitHub issue action.

It is strictly a viewer: it never extracts source or writes snapshot data. It binds loopback only and serves nothing but the dashboard. **Refresh** checks the selected snapshot directory immediately. Pass `--no-dashboard` to skip the dashboard attached to an MCP server.

`enola --generate` is different: it generates the snapshot files and exits, without starting an MCP server or dashboard and without registering a running instance. A running standalone dashboard notices that publication and reloads it. A normal repository dashboard restores only that repository's snapshot; pass an explicit cluster config to display a multi-repository graph.

#### Several servers at once

Agent tooling starts one enola server per session, so opening four terminals means four servers - each with its own graph, its own dashboard, and its own ephemeral port. Two things keep that legible.

**One bookmarkable URL.** Besides its own port, every server competes for a fixed **shared URL**, `http://127.0.0.1:7171` by default. The first to start wins it; when that one exits another takes over within a few seconds, so the address keeps working for as long as any server is up. Whichever server answers there lists all the others. Set `ENOLA_DASHBOARD_PORT` (or `dashboard.port` in the config) to move it, or `ENOLA_DASHBOARD_PORT=off` to keep only the ephemeral ports.

`enola dashboard` is a standalone dashboard process. MCP-hosted dashboards exit with their agent sessions; a standalone dashboard remains until Ctrl-C. Diagnostics refreshes its process list on each page request.

**Every page describes its own server.** The PID, uptime, repos and per-server call counts on a page belong to the process serving it - never to whichever server happened to start last. If a page shows a graph you did not expect, the switcher tells you which server holds the one you want.

Running servers register themselves under `~/.enola/instances/`; a record is removed on exit, and one left behind by a hard-killed process is cleaned up by the next reader. Each workspace also keeps its own graph receipt under `~/.enola/graphs/`, so restarting a server in one repo restores *that* repo's graph rather than whatever another terminal snapshotted last.

---

---

## Build from source

Prerequisites: **Go 1.25+** and a **C compiler** (for the tree-sitter bindings).

```bash
go build -o enola ./cmd/enola   # or: go install ./cmd/enola
```

To run a one-shot snapshot without starting the MCP server:

```bash
enola --generate [config_path]   # config_path is optional; defaults to mcp-arch.yaml, falling back to built-in defaults if absent
```

Artifacts are written to the configured `output.dir` (default `.enola/`). The config file is optional - see **[ARCHITECTURE.md → Configuration](../ARCHITECTURE.md#configuration)** for the full field reference and defaults.

**Indexing a whole cluster in one command.** Cross-repo linking needs several repositories in one graph. Name them with `repos:` and a single run indexes them all - the first fresh, the rest appended - producing the service nodes, cross-repo edges, `coverage_report` and unused-route findings that a single-repo snapshot cannot have:

```yaml
# ci/cluster.yaml
repos:
  - ../api
  - ../web
  - ../sdk
```

```bash
enola --generate ci/cluster.yaml
```

Entries resolve **relative to the config file**, not to your working directory, so a cluster config can be checked in and means the same thing on a laptop and in CI. (`repo:` is unchanged: a single repository, relative to the working directory.) Order matters - the first entry resets the graph and the rest are added to it. Linking and the explainers run once, over the whole union, after the last entry; every repository's output dir then receives the complete linked graph (the same bytes, so a consumer reading any one of them reads the whole cluster). Configured fact providers run concurrently within each repository and merge in name order.

---
