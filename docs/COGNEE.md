# Using Enola alongside Cognee

*You installed Cognee, so you already have enola. This page shows where it is and how
to use it directly.*

[Cognee](https://github.com/topoteretes/cognee) uses enola for one job: it runs
`enola --generate` on a repository and loads the result into its code graph. The
binary it runs is a complete enola install. The same binary gives your coding agent
the architecture over MCP, shows you a repository's structure, and grades what a
change did to it. There is nothing more to install.

Everything enola does is local. It parses source files and uses no model, no
embeddings, no upload and no account.

## Find the binary

Cognee depends on the PyPI package `enola-cli`, which places the binary in the
scripts directory of the Python environment Cognee is installed in.

| Environment | Path |
|---|---|
| A virtual environment on Linux or macOS | `.venv/bin/enola` |
| A virtual environment on Windows | `.venv\Scripts\enola.exe` |
| Anything else | Ask the interpreter, as below |

```bash
python -c "import sysconfig; print(sysconfig.get_path('scripts'))"
```

Run that with the interpreter that has Cognee installed. With the environment active,
`enola` is on your `PATH`:

```bash
enola --version
```

With uv, `uv run enola --version` works without activating anything.

Keep the full path. You need it when you register enola with an agent, because the
agent starts enola outside your activated environment.

## Look at a repository

Start with the read-only report. It prints the architecture enola measured (patterns,
cycles, layer violations, hotspots, blast radius) and writes nothing to `.enola/`:

```bash
enola --explain /path/to/repo
```

Then open the dashboard on the repository's latest snapshot:

```bash
enola dashboard --open /path/to/repo
```

A repository you ingested with Cognee already has a snapshot. For any other
repository, create one first with `enola --generate /path/to/repo`.
[DASHBOARD.md](DASHBOARD.md) walks through every tab.

## Connect it to your coding agent

This is the main reason to use enola directly. Over MCP, your agent queries the graph
instead of reading files to reconstruct it: what depends on a symbol, how two modules
are connected, what a change would reach.

Register the binary by its full path.

**Claude Code**

```bash
claude mcp add enola /path/to/project/.venv/bin/enola
```

**Codex**

```bash
codex mcp add enola -- /path/to/project/.venv/bin/enola
```

**Cursor**, in `mcp.json`:

```json
{
  "mcpServers": {
    "enola": {
      "command": "/path/to/project/.venv/bin/enola"
    }
  }
}
```

[CLI.md](CLI.md#connect-it-to-your-agent) covers GitHub Copilot, opencode and Pi.

Then tell your agents the graph is there. Run this in the repository you work on:

```bash
enola install --dry-run   # show what would change
enola install             # write it
```

It adds a short instruction to the files your agents already read. After that, ask in
plain language:

> "Generate an architectural snapshot of /path/to/repo"

> "What would break if I refactor the billing module? Show me the impact analysis."

> "How does the HTTP handler layer reach the database layer? Show me the shortest path."

[CLI.md](CLI.md#use-it) has the full set of prompts and the tools behind them.

## Grade a change

Enola pins the architecture before a change and reports what the change did to it:
a new dependency cycle, a layer crossed the wrong way, coupling nobody asked for.

With an agent:

> "Pin the current architecture as a baseline before we start."

> "Re-snapshot and show me the architecture diff against the baseline."

From the shell, in a git hook or in CI:

```bash
enola baseline pin              # before editing
enola check                     # after: reports the delta, always exits 0
enola check --fail-on=layers    # or exits 1 on what you named
```

[FIRST-CHANGE.md](FIRST-CHANGE.md) runs this loop once on a small module, and
[GATING.md](GATING.md) defines what can fail a build.

## More than one repository

Enola links repositories into one graph, so a client call in one resolves to the route
that serves it in another. [CLUSTERS.md](CLUSTERS.md) shows how.

## Sharing a repository with Cognee

Your runs and Cognee's runs work on the same repository. Five things keep them out of
each other's way.

**They share `.enola/`.** Both write the snapshot to `.enola/` at the repository root.
The same binary produces the same snapshot for the same source tree, so neither
disturbs the other. Your own use adds `baseline/` and `previous/` inside it, which
Cognee does not read. Add `.enola/` to `.gitignore`.
[GRAPH.md](GRAPH.md#on-disk) lists every file.

**They share the configuration.** Enola needs no configuration file. If you add an
`mcp-arch.yaml` at the repository root, for example to ignore third-party code copied
into your source tree, it applies to your runs and to Cognee's.
[CLI.md](CLI.md#configuration-optional) explains the file.

**Leave the version alone.** Cognee pins the enola release it has validated.
`enola upgrade` refuses to replace a pip-installed binary, and
`pip install -U enola-cli` conflicts with Cognee's pin. A newer enola arrives with a
newer Cognee.

**The release notice does not apply to you.** When you run enola yourself, it checks
for a newer release and may suggest an upgrade. Cognee turns that check off for its
own runs. To turn it off for yours:

```bash
export ENOLA_NO_UPDATE_CHECK=1
```

**A second, newer enola is possible, with a cost.** If you want the latest release
for your own use, install a separate copy with the
[install script](CLI.md#install) and register that path with your agent. Cognee keeps
running the copy in its own environment. The cost: the enola version is part of
`snapshot_id`, so two releases writing to one `.enola/` change it back and forth, and
Cognee loads the graph again each time it sees a change.

Enola also keeps an architecture history under `~/.enola/graphs/`, one revision per
snapshot. It is what `enola log` and `enola diff` read. Cognee's runs add to it too.
[HISTORY.md](HISTORY.md#where-it-lives-and-what-it-costs) covers its size and how to
prune it with `enola gc`.
