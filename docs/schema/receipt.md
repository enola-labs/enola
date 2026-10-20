# receipt.json

`receipt.json` describes a generated snapshot and its extraction quality. It is
a projection of the internal `snapshot.meta.json` artifact.

This repository-local artifact is separate from `~/.enola/receipt.json`, which
describes the current graph across repositories.

## Fields

| Field | Type | Meaning |
|---|---|---|
| `snapshot_id` | string, `"sha256:..."` | SHA-256 fingerprint of the fact serialization, enola version, and effective configuration hash |
| `format_version` | int | Artifact format generation. Current value: `1`; treat `0` as unknown |
| `enola_version` | string | Build that produced the snapshot; local builds use `dev` |
| `extractor_version` | string, when set | Extraction-behavior version used for cache and compatibility checks |
| `generated_at` | string, RFC3339 | Snapshot write time |
| `duration` | string | Generation duration |
| `repo_path` | string | Absolute analyzed repository path |
| `git` | object, when a Git repository | `{ref, commit, dirty, remote}` at snapshot time. `remote` is normalized without scheme, credentials, port, or `.git` suffix |
| `extractors` / `explainers` / `renderers` | array of string | Plugins used during generation |
| `providers` | array, when set | External provider records. Contract fields are `name`, `fact_count`, and, when set, `version`, `skipped`, and `reason`; other counters are diagnostic |
| `config_hash` | string, `"sha256:..."`, when set | Hash of the effective configuration |
| `ignore_glob_hash` | string, `"sha256:..."`, when set | Hash of sorted ignore and test globs |
| `output_hashes` | object, when set | Artifact filename to SHA-256 hash of written bytes |
| `fact_count` / `insight_count` | int | Artifact record counts |
| `quality` | object | Extraction-quality fields described below |

`snapshot_id` remains stable when its fact serialization, enola version, and
configuration hash remain stable. Repository labels are part of fact identity;
without a usable Git remote they depend on the checkout directory name.

The receipt itself is not byte-stable because `generated_at`, `duration`, and
`repo_path` can change between runs or machines.

## quality

| Field | Meaning |
|---|---|
| `files_seen` | Source files enumerated by the walker, excluding ignored files |
| `files_parsed` | Distinct files that produced at least one fact |
| `files_skipped` / `dirs_skipped` | Ignored files visited and ignored directories pruned |
| `skipped_sample` | Capped sample of skipped paths and their matching globs |
| `parse_errors` | Non-fatal extractor detection or parsing failures, and one per file an extractor read only in part (its parser left an error node; the extraction fact names the file under `parsed_with_errors`) |
| `parse_error_sample` | Capped sample of `{extractor, file?, msg}` records |
| `heuristic_insights` | Insights with confidence below `1.0` |
| `coverage` | Multi-repo coverage summary: `{services_total, coverage_gaps, unresolved_edges, external_edges?, extractors_reporting?, extraction_unresolved?}` |
| `census` | File accounting: `{files_walked, parsed, excluded_by_ignore, excluded_by_kind, excluded_kinds?, skipped_with_cause, top_skip_causes?}` |

Read `quality` before accepting a snapshot. `parse_errors`, the relationship
between `files_parsed` and `files_seen`, and `census.excluded_kinds` indicate
possible extraction gaps. They are signals to surface, not universal rejection
thresholds.
