# Serving enola over HTTP

`enola serve` exposes the MCP tools over Streamable HTTP, for a service that calls
enola as a tool: an internal developer portal, a company knowledge tool, a review bot.
The stdio server an agent starts (`enola` with no command) is unchanged, and nothing in
a repository or a config file can turn serving on. It is a separate command, run on
purpose by whoever operates it.

```sh
openssl rand -hex 32 > /etc/enola/tokens && chmod 600 /etc/enola/tokens
enola serve --root /srv/repos --token-file /etc/enola/tokens
```

```
enola serve v1.x: MCP over Streamable HTTP at http://127.0.0.1:7341/mcp
  Roots:       /srv/repos
  Tokens:      1 accepted
  Limits:      16 concurrent calls, 15m0s per call, sessions idle 30m0s
```

The consumer connects with any MCP client that speaks Streamable HTTP, sending
`Authorization: Bearer <token>`, and uses the tools as an agent would:
`generate_snapshot(repo_paths=["/srv/repos/api", "/srv/repos/web"])` to index a backend
and its frontend as one linked set, then `endpoint_impact`, `impact_analysis`,
`traverse` and the rest against it.

## What enola does not do

It fetches nothing. The repositories are on the disk where enola runs, and keeping them
current (clone, fetch, checkout) is the operator's job. Which repositories to index, and
when, is the consumer's: it names them in its calls, as an agent does.

## Who can call it

- **A token is always required.** `--token-file` holds the accepted tokens, one per
  line; blank lines and lines starting with `#` are skipped. A token needs at least 32
  characters. Give each consumer its own, so one can be revoked by deleting its line and
  restarting. The file must be readable by its owner only (`chmod 600`), or serve
  refuses to start, the way ssh refuses a private key. There is no flag that turns
  authentication off.
- **Loopback by default.** `--http` takes the address to listen on, `127.0.0.1:7341`
  unless told otherwise. Anything else is reachable from the network, so serve says so
  at start; put TLS in front of it (a reverse proxy, a service mesh). serve does not
  terminate TLS itself.
- **Browsers are refused.** A cross-origin request from a web page is answered 403,
  and on loopback the MCP SDK's DNS rebinding protection rejects a request whose `Host`
  is not a loopback name. A service calling server to server sends neither and is
  unaffected.

## What a call can reach

Every filesystem path a call names must lie inside a `--root`, after symlinks are
resolved; a link inside a root that points out of it is refused, and so is a path that
does not exist. That covers every argument whose name ends in `_path` or `_paths`, and
`baseline` when it names a directory rather than `pinned` or `previous`. It also covers
what an argument only leads to: every repository a snapshot reads, including the
members a `cluster.yaml` inside a root lists, is checked again before it is read.

There is no default repository. A call that names none is refused, rather than given the
directory the service manager started enola in.

```
repo_path: /etc is outside the directories this server serves
```

## The tools

A served instance lists only the tools in its allowlist, `server.ServedTools`. Today
that is every tool the stdio server has. A tool added to enola later stays off HTTP
until it is added to that list on purpose, and a test fails until someone decides.

The instructions a served instance sends at initialization are written for a service,
not for a coding agent: no deferred tool loading, no "if asked whether a project needs
enola". The notice that a newer enola exists is not sent either; a consumer cannot
upgrade the binary.

## Serving one graph

Given a repository or a cluster config as its last argument, serve serves that graph
and nothing else. The operator chooses what is indexed; consumers only query.

```sh
enola serve --root /srv/repos --token-file /etc/enola/tokens /srv/repos/cluster.yaml
```

- **Every session queries it at once.** All sessions are served by one server holding
  the graph, so a consumer that opens a new session for every call (identity resolved
  per call, several replicas behind a load balancer) needs no `generate_snapshot` and
  leaves nothing behind on the server, whether or not it closes its sessions.
- **Nothing indexes.** `generate_snapshot` and `set_baseline` are not listed, and
  `plan_check` refuses a `patch`, which would index a scratch copy. The instructions
  tell the consumer the graph is already there.
- **The operator refreshes it.** Run `enola --generate` over the same config on
  whatever schedule suits you. serve checks the graph's `snapshot.meta.json` every 5
  seconds, waits until a change has held still for one more check, so a run still
  writing is never read half done, and swaps the new graph in. Calls already running
  finish on the graph they started with. If the new artifacts cannot be read, the old
  graph stays and the log says why.
- **Before the first index**, calls answer that the graph is not available yet and
  that its operator indexes it, not that the consumer should.

Put `output.root` in the config file both use, rather than `--output-root` on serve
alone, so that `enola --generate` writes where serve reads:

```yaml
repos:
  - api
  - web
output:
  root: /var/lib/enola
```

`--memory-budget` does not apply: there is one graph, and it is always served.

## Sessions and graphs

Without a repository or config, consumers index what they ask about.

Each MCP session has its own loaded graph: a consumer's `generate_snapshot` changes
what that session queries and nobody else's. What sessions share is the graphs
themselves. When a session asks for repositories another session already indexed,
and nothing in them changed on disk since, it adopts that graph instead of indexing
again or loading a second copy:

```
Reused the graph this server already holds for these repositories: nothing in them changed since it was built.
```

The same set of repositories is one graph whatever order they are listed in. A
change to any file in any of them, or an `append`, builds a new graph as usual. A
folder indexed as a cluster is always built, since what it stands for depends on its
contents.

`--memory-budget` (`16GiB`, `512MiB`) bounds the graphs held for sessions. Past it,
the least recently used graph that no call is reading is unloaded and the server logs
it. The session that held it reloads it from its artifacts on disk at its next call,
which is told so in the result:

```
[pool] unloaded /srv/repos/roslyn (596 MiB) to stay inside the 1024 MiB budget
[pool] heap after unloading: 411 MiB live, 1187 MiB held from the OS
```

The budget is an estimate, twice the size of a graph's `facts.jsonl`. Measured as the
heap a restored graph holds:

| Repository | facts.jsonl | Heap held | Ratio |
|---|---:|---:|---:|
| gin | 0.5 MB | 1.3 MB | 2.5 |
| lobsters | 2.0 MB | 4.7 MB | 2.3 |
| vite | 6.8 MB | 12.3 MB | 1.8 |
| roslyn | 296 MB | 332 MB | 1.1 |
| vscode | 380 MB | 533 MB | 1.4 |
| dotnet/runtime | 441 MB | 673 MB | 1.5 |
| gitlab | 438 MB | 705 MB | 1.6 |
| linux | 1162 MB | 1422 MB | 1.2 |

It bounds the graphs held between calls, not the peak while one is built: indexing
dotnet/runtime from scratch peaked at about 7 GB of RSS. On macOS, RSS stays high after
an unload until the system needs the memory; the `heap after unloading` line is the
number to read.

Measured on one machine (Apple Silicon, 16 cores), serving `~/development` with
`--output-root`:

| What | Time |
|---|---:|
| A new session, connect to initialized | 15 to 40 ms |
| Roslyn (376k facts), first build in a session | 19 s |
| The same, 15 more sessions at once, first time the server proves it unchanged | 4.3 s slowest |
| The same, once the server has hashed its files | 0.6 s one session, 1.3 s slowest of 15 |
| Roslyn reloaded from disk after an unload | 3.1 s |
| A query on a loaded graph, over HTTP | about 1 ms |

Sessions asking at the same moment whether the same graph is still current share one
walk of its trees, rather than each walking it.

Under load: 32 to 128 sessions over five sets of repositories (vite, khoj, lobsters,
gin, and vite with khoj linked), each session indexing its set once and then making
40 queries, with every graph already built:

| Sessions | `--max-concurrent` | Calls per second | Query p50 | Query p95 | Query p99 | RSS |
|---:|---:|---:|---:|---:|---:|---:|
| 32 | 16 | 920 | 9 ms | 99 ms | 444 ms | 408 MB |
| 64 | 16 | 888 | 19 ms | 217 ms | 851 ms | 409 MB |
| 128 | 16 | 804 | 33 ms | 664 ms | 1.9 s | 447 MB |
| 64 | 4 | 871 | 20 ms | 93 ms | 1.1 s | 353 MB |

Throughput is bound by CPU, not by the cap: a lower `--max-concurrent` gives the same
rate with a shorter p95, because fewer tree walks compete. A session costs a fraction
of a megabyte; the graphs are the memory.

## Baselines between consumers

A pinned baseline and the previous run live in a repository's output directory, so
consumers working on the same repository share them, as two agents on one machine do.
Each pin and each run records which session wrote it: the token's id and the MCP
session id. When a consumer diffs against a baseline another session pinned, and that
session is still connected, the diff says so and names the token:

```
the baseline was pinned at 2026-11-10T09:27:37Z by another session of this server (token 4612ea736396) that is still running on this repository, so it may include that session's changes, or have replaced a baseline this session pinned
```

`set_baseline` says the same when it replaces such a pin. A pin by a session that has
ended is just the baseline, and nothing is said.

## Limits

| Flag | Default | What it bounds |
|---|---|---|
| `--max-concurrent` | CPU count | Tool calls running at once, across every session. More wait for a slot. |
| `--memory-budget` | none | Estimated memory the graphs held for sessions may take. See above. |
| `--call-timeout` | `15m` | How long one call may run before it is cancelled. `0` for no limit. |
| `--session-timeout` | `30m` | How long an idle session is kept. |

A request body is capped at 16 MiB, enough for a `plan_check` patch. Responses stream
for as long as their call runs, so there is no write timeout; the call timeout bounds
that instead.

## The audit log

One line per tool call goes to stderr after the call ran:

```
[audit] token=4612ea736396 session=ICZIDHVZ... tool=generate_snapshot outcome=ok duration=33ms args_bytes=127 response_bytes=977
```

`token` is the first 12 hex digits of the token's SHA-256: enough to tell consumers
apart, useless for authenticating. The arguments are left out; a patch or a search
pattern is the consumer's business. `outcome` is `ok`, `tool_error` (the tool ran and
said no, a refused path included) or `error` (the call never reached a tool, such as a
tool outside the allowlist).

## What it leaves alone

A served instance writes no `~/.enola/receipt.json`: that receipt tracks the workspace
an agent on this machine worked in last, and a server must not overwrite it.

The checkouts under `--root` belong to whoever keeps them current, and a `git clean`
or a fresh clone there would delete anything enola wrote, the pinned baseline included.
`--output-root` moves every repository's artifacts out of it:

```sh
enola serve --root /srv/repos --output-root /var/lib/enola --token-file /etc/enola/tokens
```

The artifacts of `/srv/repos/api` then live in `/var/lib/enola/srv/repos/api/.enola`:
the snapshot, the extractor cache, the pinned baseline and the previous run. With it,
a served instance writes nothing into the repositories at all, so they can be
read-only to the user serve runs as. A folder of repositories is still indexed as a
cluster, from the `cluster.yaml` already there or from the repositories under it, but
a served instance never writes that file. The same setting is `output.root` in a
config file, for the CLI and the stdio server.

enola reads each repository's git state (commit, remote, whether the tree is dirty)
without taking git's index lock, so the job keeping the checkouts current never fails
on `.git/index.lock` because a snapshot was running.

Without `--output-root`, snapshots are written where they always are, into each
repository's `.enola` directory, and the directories under `--root` must be writable.


