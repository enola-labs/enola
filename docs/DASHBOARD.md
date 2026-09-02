# The dashboard

The dashboard displays snapshots produced by Enola's CLI and MCP tools. It opens on the
findings and changes for the selected repository, follows newer snapshots written to disk,
and keeps process details out of the way unless you open Diagnostics.

![The Architecture tab showing the searchable module graph, dependency direction, inspector, and finding summary](images/DashboardArchitecture.png)

## Start here

Generate a snapshot, then start the dashboard:

```bash
enola --generate
enola dashboard --open
```

The dashboard stays attached to the terminal until Ctrl-C. Its startup message names
the snapshot directory and prints the complete local `http://` URL. Pass a repository
or multi-repository config path to select its snapshot:

```bash
enola dashboard --open ../service-a
enola dashboard --open cluster.yaml
```

The server binds to loopback (`127.0.0.1`). If Safari has HTTPS-Only mode enabled, allow
the local HTTP address or open the printed URL in another browser.

## Review a change

Pin a baseline before editing, then write a snapshot after the change:

```bash
enola baseline pin
# make the change
enola check --write
enola dashboard --open
```

Start on **Overview**. Change cards group new and resolved findings. Open a finding to
inspect its evidence in the Architecture tab. Findings describe the measured structure;
project policy determines which findings fail a check.

New and resolved categories require a comparable baseline. Without one, Overview shows
the current snapshot.

## Follow a dependency

Use **Architecture** to inspect:

- module consumers;
- module dependencies;
- source facts supporting an edge;
- highly connected modules;
- findings associated with the selected module.

Search covers every connected module, even when the overview renders only a smaller
set for clarity. Select a module to replace the overview with its immediate dependency
neighborhood. The graph, inspector, finding list, and module table share the current
selection. **Fit view** fits the visible graph in its viewport.

The labels distinguish total relationships from the subset currently drawn. A finding
such as “33 dependants” can refer to symbol-level evidence. A focused module graph shows
module-level neighbors. The inspector names the scope of the selected metric.

## Verify what the snapshot represents

**Snapshots** records when the graph was generated, the Git ref and working-tree state,
the extraction schema, contributing extractors, and analysis duration.

![The Snapshots tab showing Git capture, snapshot identity, extraction schema, and analysis scope](images/DashboardSnapshots.png)

“Captured with local changes” means the snapshot includes the working tree at generation
time. Reproducing it requires the commit and those local changes.

The technical receipt contains the comparison inputs. Enola checks receipt compatibility
before computing a change: extractor and ignore-rule changes can alter the observed
graph.

## Check whether the analysis is complete

**Quality** accounts for indexed source, configured exclusions, unsupported or
non-source inputs, recognized files with zero facts, inactive extractors, parse failures,
and unresolved cross-repository connections.

![The Quality tab explaining file accounting and a narrowly scoped inactive extractor](images/DashboardQuality.png)

The file-accounting row classifies every walked file. In the example, one TypeScript file
belonged to an inactive extractor, representing 0.1% of the 999 walked files.

The categories are:

- **Intentionally ignored** matched configured exclusions. Samples show which rules
  applied.
- **Non-source / unsupported** lists assets and file types outside the graph. The
  file-kind table identifies types that may carry architecture.
- **No facts emitted** contains recognized files with zero indexable facts.
- **Parse errors** contains extractor failures. The GitHub action opens an issue with
  snapshot details filled in.
- **Unresolved cross-repository connections** lists edges whose remote target was absent
  from the cluster snapshot.

## See lifetime usage

**Lifetime usage** shows tool calls and estimated value accumulated across repositories
and completed sessions. These machine-wide totals are deliberately separate from the
repository snapshot shown on the other screens.

![The Activity tab showing the standalone dashboard session serving the current page](images/DashboardActivity.png)

For active processes, dashboard URLs, ports, paths, and the process serving the current
page, open **Diagnostics**.

![Lifetime activity aggregated across completed and active sessions](images/DashboardLifetimeActivity.png)

Lifetime activity aggregates completed and active sessions across repositories. Enola
retains aggregate totals. Time and token savings estimate the repository reconstruction
avoided by using snapshot data.

## Automatic refresh

After another Enola process writes a newer snapshot, a standalone dashboard reloads it
from disk and updates the page automatically. **Refresh** performs the same disk check
immediately. An MCP-hosted dashboard reads its server's live graph.

An ordinary repository launch shows only that repository's snapshot. A multi-repository
graph is restored only when the dashboard is launched with an explicit cluster config.
Use `enola --status` to find the URL for another workspace.

`enola upgrade` replaces the binary; it does not regenerate repository snapshots. When
the extraction schema differs, the dashboard keeps the existing findings visible and
shows one regeneration notice with the command to run.

## Several dashboards at once

An MCP session can host the same dashboard, and several coding-agent sessions can run at
once. Each process has its own graph and ephemeral URL. `enola --status` lists the
running sessions and their dashboard URLs.

While any dashboard is running, Enola also tries to provide the bookmarkable shared URL
`http://127.0.0.1:7171`. The first process owns it; another can take over after that
process exits. Diagnostics identifies the process serving the page.

## Related commands

Use the CLI to generate and compare snapshots. Declare architectural rules in
`enola-intent.yaml` and enforce selected finding classes with
`enola check --fail-on=...`.

For every command and option, see [CLI.md](CLI.md). For the meaning of findings and
confidence, see [EXPLAINERS.md](EXPLAINERS.md). For why Enola uses explicit snapshots,
see [SNAPSHOTS.md](SNAPSHOTS.md).
