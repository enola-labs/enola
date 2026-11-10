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

## Limits

| Flag | Default | What it bounds |
|---|---|---|
| `--max-concurrent` | CPU count | Tool calls running at once. More wait for a slot. |
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
an agent on this machine worked in last, and a server must not overwrite it. Snapshots
are written where they always are, into each repository's `.enola` directory, so the
directories under `--root` must be writable by the user serve runs as.

A repository or cluster config given as the last argument has its graph restored from
disk at start, so the first call does not wait for an index:

```sh
enola serve --root /srv/repos --token-file /etc/enola/tokens /srv/repos/cluster.yaml
```
