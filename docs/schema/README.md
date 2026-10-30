# Snapshot artifact format

This directory documents the files written by `enola --generate` to the
configured output directory, `.enola/` by default. These files are the contract
for external snapshot consumers.

## Artifacts

| File | Format | Contract |
|---|---|---|
| `facts.jsonl` | JSON Lines, one fact per line | [facts.md](facts.md) |
| `insights.json` | JSON array of findings | [insights.md](insights.md) |
| `receipt.json` | JSON object with provenance and extraction quality | [receipt.md](receipt.md) |
| `flows.json` | JSON object ranking the ways into the program | [flows.md](flows.md) |
| `docs.jsonl` | JSON Lines, one documented fact's whole comment per line | [docs.md](docs.md) |
| `snapshot.meta.json` | Internal superset of the receipt | Not stable |
| `llm_context.md` | Rendered text | Not stable |

The output of `enola locate --json` and the `locate` MCP tool is documented in
[locate.md](locate.md). It is a result, not a file `--generate` writes.

## Contract scope

The contract covers:

- Documented JSON field names and presence rules.
- Fact-kind and relation-kind values.
- Props explicitly identified as contract props in [facts.md](facts.md).
- Fact identity and the resolution semantics of `target_id` and `fact_id`.

It does not cover undocumented props, JSON object field order, insight prose, or
renderer output. Consumers must accept unknown fields, kinds, relation kinds,
and prop values for a supported format version because additive vocabulary does
not change `format_version`.

The field tables describe artifacts written by the current release. Historical
artifacts with the same `format_version` can omit fields added later. In
particular, consumers that accept historical version-1 artifacts must detect
`id`, `target_id`, and `fact_id` by field presence.

## Versioning

`receipt.json` contains `format_version`. The current value is `1`.

- Additive changes, including new optional fields and vocabulary values, do not
  change `format_version`.
- Renaming or removing a documented field, changing identity semantics, or
  changing the meaning of an existing value requires a new `format_version`.

Reject unsupported versions. Treat `format_version: 0` as unknown.

## Determinism

`facts.jsonl`, `insights.json` and `flows.json` are byte-stable when the source tree,
effective configuration, enola build, and repository labels are unchanged.
`snapshot_id` hashes the fact serialization, enola version, and effective
configuration hash.

Repository labels normally come from Git remote repository names and otherwise
fall back to checkout directory names. Different fallback labels produce
different fact IDs and snapshot IDs for the same source tree.

`receipt.json` is not byte-stable. It contains run-specific and machine-specific
values such as `generated_at`, `duration`, and `repo_path`.
