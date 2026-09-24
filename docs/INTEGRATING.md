# Building on enola

Use this guide when another tool runs enola, reads its snapshot artifacts, and
loads the graph into its own store. For interactive CLI use, see
[CLI.md](CLI.md). For what the graph contains and how it is built, see
[GRAPH.md](GRAPH.md). For field definitions, see [schema/](schema/README.md).

One implementation of the core workflow is already live: [Cognee](https://github.com/topoteretes/cognee).
The [Cognee section below](#cognee) documents its precise behavior and validation boundaries.

## Artifacts

```sh
enola --generate /path/to/repo
```

The command writes these contract artifacts to `<repo>/.enola/` by default:

| File | Contents |
|---|---|
| `receipt.json` | Format version, snapshot identity, provenance, counts, and extraction quality |
| `facts.jsonl` | Graph nodes and relations, one fact per line |
| `insights.json` | Findings and their supporting evidence |

The directory also contains internal artifacts such as `extractor_cache.json`,
`snapshot.meta.json`, and renderer output. Keep `extractor_cache.json` between
runs to preserve incremental extraction performance. Do not consume the other
internal artifacts as contracts.

## 1. Install a pinned release

Release assets follow this naming scheme:

```text
https://github.com/enola-labs/enola/releases/download/v{version}/enola-{version}-{os}-{arch}.tar.gz
https://github.com/enola-labs/enola/releases/download/v{version}/enola-{version}-{os}-{arch}.sha256
```

Each release also carries `enola-{version}-{os}-{arch}.upgrade.sha256`, the same
digest under a name only `enola upgrade` fetches, so upgrades can be counted
apart from installs. Integrations should verify against `.sha256`.

The release workflow currently publishes:

| OS | Architectures |
|---|---|
| `linux` | `amd64`, `arm64` |
| `darwin` | `amd64`, `arm64` |
| `windows` | `amd64` |

Pin a version, verify the archive checksum, and cache the binary. Do not resolve
"latest" during each run; adopting a new writer should be an explicit upgrade.

## 2. Run generation

Run enola from a working directory that does not contain an unrelated
`mcp-arch.yaml`. Enola resolves that file from the working directory, and
list-valued settings such as `extractors` replace the defaults.

Every run reports the resolved configuration on stderr:

```text
enola: no mcp-arch.yaml in /tmp/work, using built-in defaults
```

Record that line with the subprocess logs. A present but invalid configuration
is an error.

Exit status `0` means generation and artifact writing succeeded. On any non-zero
status, do not ingest files already present in the output directory; they may be
from an earlier run.

The default output directory is `.enola`. A configured `output.dir` must be a
subdirectory of the repository and is excluded from extraction automatically.

### Side effects

Generation writes more than the three contract artifacts:

- Snapshot artifacts and the extraction cache go to the configured repository
  output directory.
- The graph-wide receipt is updated under `~/.enola/`.
- Architecture history is stored under `~/.enola/graphs/<key>/history` by
  default. `history.dir` can override this location.

Enola does not run a daemon for this integration. Do not assume that generation
is network-isolated: the CLI may perform an update check, and configured
external provider commands control their own network behavior. Set
`ENOLA_NO_UPDATE_CHECK=1` in the subprocess environment to keep a pinned run
network-quiet, and `ENOLA_NO_PROMPTS=1` when nothing reads its output. That is
the pair a pinned consumer such as Cognee uses.

## 3. Validate the receipt

After a successful subprocess exit, read `receipt.json` before loading the other
artifacts.

```json
{
  "snapshot_id": "sha256:...",
  "format_version": 1,
  "quality": {}
}
```

- Reject an unsupported `format_version`. Treat `0` as unknown.
- Use `snapshot_id` to skip ingestion when the graph has not changed.
- Check `fact_count`, `insight_count`, and `output_hashes` when validating the
  files you read.
- Surface `quality.parse_errors`, the relationship between `files_parsed` and
  `files_seen`, and `quality.census.excluded_kinds`. These are extraction-quality
  signals, not a universal pass/fail threshold.

`snapshot_id` hashes the serialized facts, enola version, and effective
configuration hash. It does not hash `insights.json` or the complete receipt.
`receipt.json` is not byte-stable because it contains values such as generation
time, duration, and the absolute repository path.

## 4. Load facts

`facts.jsonl` contains one JSON object per line. Current writers add a 32-character
fact `id` derived from `(repo, kind, name, file)`.

Use `id` as the materialized node identity, but do not require each JSONL record
to have a unique ID. Multiple records can share an ID. When materializing one
node per ID:

- Retain `name` for display and lookup.
- Union relations from all records with that ID.
- Retain every source location, or define an explicit location-selection rule.
- Preserve conflicting properties rather than allowing input order to choose a
  value silently.

If raw-record fidelity matters, store the JSONL records separately from the
materialized nodes.

Fact IDs are stable only while all four identity inputs remain stable. In
particular, `repo` normally comes from the Git remote's repository name but
falls back to the checkout directory name when no usable remote exists. Two
remote-less checkouts under different directory names therefore produce
different IDs.

Accept unknown fact kinds and properties for a supported format version. New
vocabulary is additive and does not bump `format_version`; either retain unknown
data or report that it was intentionally dropped.

## 5. Load relations

Each relation contains a readable target name and may contain a resolved ID:

```json
{
  "kind": "declares",
  "target": "api",
  "target_id": "b40cc8199deadc4199623e0a6a8c64b1"
}
```

Use `target_id` when present. Current writers resolve it as follows:

1. Prefer facts named `target` in the source fact's repository.
2. Emit an ID when all matching local facts have the same identity.
3. If there is no local match, emit an ID only when all matches across the
   snapshot have the same identity.

When `target_id` is absent, the target may be external, ambiguous, or written by
an older writer. Preserve `target` as an unresolved reference. Do not select an
arbitrary matching fact.

`declares` points from the declared fact to its containing module. For example,
a symbol carries `declares -> module`; there is no inverse module-to-symbol
relation. See [Relation kinds](schema/facts.md#relation-kinds) for all directions.

## 6. Load insights

`insights.json` is always a JSON array. An empty result is `[]`, not `null`.
Evidence may contain `file`, `symbol`, `fact`, and a resolved `fact_id`.

Use `fact_id` when present. Current evidence producers may set both `symbol` and
`fact`; ID resolution uses `symbol` when non-empty and otherwise uses `fact`.
When `file` is present, it is used to narrow candidates before an ID is emitted.

A missing `fact_id` is valid. It can mean that the cited fact is ambiguous or
outside the snapshot. It can also be intentional: a finding about a missing
handler cites a name for which no fact exists. Preserve unresolved evidence
rather than rejecting the insight.

## 7. Upgrade compatibility

`format_version` changes for breaking contract changes such as renamed fields or
changed identity semantics. New optional fields, kinds, and prop values are
additive and do not change it.

The schema describes artifacts written by the current release. Historical
artifacts with the same `format_version` can lack fields added later, including
`id`, `target_id`, and `fact_id`. A consumer that accepts historical artifacts
must detect those capabilities by field presence. A consumer that only accepts
artifacts from its pinned writer can validate the current required fields.

Upgrading the enola binary changes `snapshot_id` even when source code has not
changed because the enola version is part of the fingerprint. Plan to re-ingest
snapshots after an upgrade.

## Cognee

[Cognee](https://github.com/topoteretes/cognee) is a Python framework that builds a knowledge graph from repositories, documents and conversations. Its code-graph route implements the core integration flow: a pinned release of enola run as a subprocess, the snapshot loaded into its own graph database, and the result served through a deterministic `SearchType.CODE` search that needs no LLM key. Its current behavior is:

- **Install (step 1).** Cognee declares `enola-cli` as a core dependency pinned to the release it has validated, so the binary arrives with the cognee install and nothing is fetched at runtime. The wheel installs it into the Python environment's scripts directory - `.venv/bin/enola`, or `Scripts\enola.exe` on Windows - the same place a direct `pip install enola-cli` puts it.
- **Binary discovery.** Cognee resolves the binary in this order: `ENOLA_PATH` (an explicit override that always wins; a path to a missing file is a loud error, not a silent fallback), then the environment's scripts directory directly, then `PATH`. The scripts-directory step is what makes it work when the venv is not on `PATH` - container entrypoints, service units and schedulers run the interpreter by path. When nothing is found, cognee raises `EnolaNotInstalledError`; its documented fix is to reinstall cognee, never to download.
- **Run generation (step 2).** It runs `enola --generate <repo>` from the repository directory with the step-2 environment pair set - `ENOLA_NO_UPDATE_CHECK=1` and `ENOLA_NO_PROMPTS=1` - which disables enola's own update check and prompts. A repository-local configuration can still invoke an external provider with its own network behavior. The snapshot lands in `.enola/` inside the repository; cognee's `.gitignore` carries that entry, and yours should too.
- **Artifacts (steps 3-6).** It reads the three contract artifacts - `facts.jsonl`, `insights.json` and `receipt.json` - and a receipt whose `format_version` cognee does not understand is a hard error whose message points at upgrading cognee, not at editing the receipt. A missing receipt is tolerated for compatibility with historical snapshots, while an unparseable receipt is logged and ignored. Cognee checks `fact_count` and surfaces extraction-quality signals, but does not currently verify `insight_count` or `output_hashes`.
- **Upgrade compatibility (step 7).** It stores the receipt's `snapshot_id` on its repository node and skips the load when it is unchanged; a new pinned enola release changes the id, so the pin is bumped deliberately rather than floating.
- **How users drive it.** `cognee.remember(path, content_type="code")` runs the route in one call; `add()` accepts a repository directory or a GitHub/GitLab URL, and remote repositories are shallow-cloned under `~/.cognee/repos`. A list passed to `remember()` is processed as one enola run per repository into the same dataset; cognee does not currently create the single multi-repository snapshot described in [CLUSTERS.md](CLUSTERS.md), so it does not resolve cross-repository edges through that workflow.

The discovery order, the error name and the clone directory above are facts about cognee's current implementation, not part of enola's contract. If cognee changes them, its documentation - not this page - is the source of truth.

## Unsupported dependencies

Do not depend on:

| Data | Reason |
|---|---|
| Undocumented props such as `language`, `exported`, or `handler` | Extractors may change them without a format-version bump |
| Insight titles and descriptions | They are presentation text, not identifiers |
| JSON field order | Parse JSON objects by field name |
| `snapshot.meta.json` | Internal artifact |
| `llm_context.md` | Renderer output |

Report integration problems through
[GitHub Issues](https://github.com/enola-labs/enola/issues). Include the enola
version, `format_version`, `snapshot_id`, and receipt `quality` block when
available.
