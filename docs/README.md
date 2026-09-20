# Documentation

**Architectural regression testing for AI-assisted development.**

Enola compares a code change with a pinned baseline and acts as an architectural quality gate. It
fails only the architectural rules selected by the project.

Start here:

| Task | Page |
|---|---|
| Install or upgrade Enola | [Installation](INSTALL.md) |
| Configure an MCP client | [MCP integration](MCP.md) |
| Script Enola or look up flags | [CLI reference](CLI.md) |
| Run the baseline/change/check workflow | [First change](FIRST-CHANGE.md) |
| Gate a Ruby or Rails application | [Rails guide](RAILS.md) |
| Analyze multiple repositories | [Clusters](CLUSTERS.md) |
| Define architecture policy | [Intent](INTENT.md) and [constraints](CONSTRAINTS.md) |
| Consume snapshot data | [Integration](INTEGRATING.md) and [schema](schema/README.md) |
| Modify the engine | [Architecture](../ARCHITECTURE.md) and [contributing](../CONTRIBUTING.md) |

## Using Enola

| Page | Covers |
|---|---|
| [CLI.md](CLI.md) | Installation, agent integration, commands, flags, exit codes, scope controls, reviewer routing and the dashboard. |
| [FIRST-CHANGE.md](FIRST-CHANGE.md) | The loop end to end on a module small enough to read: declare a layer order, pin, change, grade, and hold the change to its declared scope. The same rule is shown in Go and TypeScript. |
| [CLUSTERS.md](CLUSTERS.md) | Two services in one graph: how client calls are matched to server routes across repositories, and how unresolved calls are reported. |
| [DASHBOARD.md](DASHBOARD.md) | Reviewing a change visually, following dependencies, verifying snapshot provenance and judging analysis completeness. |
| [RAILS.md](RAILS.md) | The Rails workflow from installation through a graded change, with the output from each step. |

## Change analysis

| Page | Covers |
|---|---|
| [EXPLAINERS.md](EXPLAINERS.md) | The twenty-two structural checks, confidence levels, and how before/after comparison isolates findings introduced by a change. |
| [SNAPSHOTS.md](SNAPSHOTS.md) | Why Enola computes addressable snapshots instead of maintaining one continuously updated graph. |
| [HISTORY.md](HISTORY.md) | `log`, `show`, `diff`, `blame`, `gc` and `history` - the recorded timeline of a repository's architecture, what it costs to keep, and how to share it across machines. |

## Evidence and limitations

| Page | Covers |
|---|---|
| [BENCHMARKS.md](BENCHMARKS.md) | Reproducibility, delta precision, cross-repository coverage and scale, measured on public repositories with scripts you can rerun. |
| [BLIND-SPOTS.md](BLIND-SPOTS.md) | Six reproducible failures against public codebases, including a bug found in Enola itself. |

## Architecture policy

| Page | Covers |
|---|---|
| [INTENT.md](INTENT.md) | Declaring services, layers, dependencies and cross-repository seams in repository, cluster or page metadata. |
| [CONSTRAINTS.md](CONSTRAINTS.md) | Components, architecture rules, enforcement modes, exemptions, recipes and Ruby-authored laws. |

## Building on Enola

| Page | Covers |
|---|---|
| [INTEGRATING.md](INTEGRATING.md) | Run Enola as a subprocess and load its snapshot artifacts into another store. |

## Extending the graph

| Page | Covers |
|---|---|
| [PROVIDERS.md](PROVIDERS.md) | Adding facts from Rubydex, runtime observations, RBS and Sorbet through the fail-closed provider interface. |
| [extraction/](extraction/README.md) | What each language extractor records, with examples from committed fixtures and its known limits. |
| [EXTENDING.md](EXTENDING.md) | Teaching Enola a connection it does not know through binders, cross-repository signals, `linking:` configuration and declared in-house HTTP clients (`clients:`). |

## Reference

| Page | Covers |
|---|---|
| [schema/](schema/README.md) | The documented on-disk format of the snapshot artifacts (facts.jsonl, insights.json, receipt.json): field names, kind and relation vocabularies, the identity convention, and how format changes are versioned. |
| [GLOSSARY.md](GLOSSARY.md) | Terms used in Enola output, including findings, baselines, receipts, coverage gaps and incidental shifts. |
| [ARCHITECTURE.md](../ARCHITECTURE.md) | The fact model, pipeline, graph, MCP tools and value model. |
| [CHANGELOG.md](../CHANGELOG.md) | Every released version, newest first. |

## Contributing

[CONTRIBUTING.md](../CONTRIBUTING.md) covers the development workflow. Documentation has two build-enforced rules:

- Counts and backticked repository paths are checked against the code. Historical measurements that intentionally keep an older count are waived by name and reason in `internal/docslint`.
- Every page under `docs/extraction/` must state its limits, and the extraction index must list every page.

Prose has no compiler; `internal/docslint` is the closest thing this repository has.
